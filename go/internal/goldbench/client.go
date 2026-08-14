package goldbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SamplingOpts sind die pro Achse festgelegten Sampling-Parameter. Sie
// spiegeln die llm.Options der jeweiligen ctx-Pipeline, soweit die
// OpenAI-Chat-API sie trägt.
//
// ABWEICHUNG (Wire-Format): ctx spricht Ollama/eigene Chains und setzt dort
// num_predict/num_ctx sowie teils top_k (internal/dream/evaluate.go:66) und
// repeat_penalty (internal/llm/client.go:369). Die OpenAI-Chat-API kennt
// max_tokens statt num_predict; top_k, num_ctx und repeat_penalty existieren
// dort nicht portabel und werden weggelassen (strikte OpenAI-Server lehnen
// unbekannte Felder ab). Siehe README-Abschnitt „Mock-Treue".
type SamplingOpts struct {
	Temperature float64
	TopP        float64 // 0 = weglassen
	MaxTokens   int     // 0 = weglassen
	JSONFormat  bool    // response_format {"type":"json_object"} — classify (internal/llm/classify.go:181 Format:"json")
}

// ChatRequest ist ein einzelner Prompt-Abruf (System + User + Sampling).
type ChatRequest struct {
	System string
	User   string
	Opts   SamplingOpts
}

// Client ist ein minimaler OpenAI-kompatibler Chat-Completions-Client.
type Client struct {
	url    string
	model  string
	apiKey string
	seed   int64
	hc     *http.Client
	// extraBody wird in jeden Request-Body gemerged (z. B. chat_template_kwargs
	// für Thinking-Schalter oder Sampler, die die portable API nicht trägt).
	// Kollisionen gewinnen die Struct-Felder — extraBody füllt nur Lücken.
	extraBody map[string]json.RawMessage
}

// SetExtraBody parst das JSON-Objekt für den Request-Merge (nil bei leer).
func (c *Client) SetExtraBody(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return fmt.Errorf("goldbench: extra-body kein JSON-Objekt: %w", err)
	}
	c.extraBody = m
	return nil
}

// NewClient baut den Client. endpoint darf die Basis-URL oder bereits die
// volle /chat/completions-URL sein.
func NewClient(endpoint, model, apiKey string, seed int64, timeout time.Duration) *Client {
	url := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(url, "/chat/completions") {
		if !strings.HasSuffix(url, "/v1") {
			url += "/v1"
		}
		url += "/chat/completions"
	}
	return &Client{url: url, model: model, apiKey: apiKey, seed: seed,
		hc: &http.Client{Timeout: timeout}}
}

// wire-Typen des OpenAI-Chat-Completions-Formats (nur die benutzten Felder).
type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireRequest struct {
	Model          string        `json:"model"`
	Messages       []wireMessage `json:"messages"`
	Temperature    float64       `json:"temperature"`
	TopP           float64       `json:"top_p,omitempty"`
	MaxTokens      int           `json:"max_tokens,omitempty"`
	Seed           int64         `json:"seed,omitempty"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ChatResult trägt neben dem Content die Server-Token-Zählung (Durchsatz)
// und den finish_reason ("length" = Output am max_tokens-Budget gerissen).
type ChatResult struct {
	Content          string
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
}

// ErrContextOverflow markiert Calls, die der Server wegen erreichter
// Context-Grenze abgelehnt hat (Fail-Metrik: context_errors statt transport).
var ErrContextOverflow = fmt.Errorf("context-grenze erreicht")

// isContextOverflowBody erkennt Context-Überlauf-Ablehnungen am Fehler-Body.
// llama.cpp: "the request exceeds the available context size" /
// "exceed_context_size"; OpenAI-kompatibel: "context_length_exceeded".
func isContextOverflowBody(body string) bool {
	s := strings.ToLower(body)
	return strings.Contains(s, "context size") ||
		strings.Contains(s, "context length") ||
		strings.Contains(s, "context_length") ||
		strings.Contains(s, "exceed_context") ||
		strings.Contains(s, "n_ctx")
}

// Chat führt einen Chat-Completions-Call aus und gibt den Antwort-Content
// zurück. Fehlerpfade sind mit %w gewrappt; der Aufrufer entscheidet über
// Retry-Politik (der Harness macht bewusst keine — ein Bench misst das
// Modell, nicht die Netz-Resilienz).
// Chat behält die alte Signatur; ChatWithUsage liefert zusätzlich Usage
// und finish_reason (für Durchsatz- und Fail-Aggregation im Report).
func (c *Client) Chat(ctx context.Context, req ChatRequest) (string, error) {
	res, err := c.ChatWithUsage(ctx, req)
	return res.Content, err
}

func (c *Client) ChatWithUsage(ctx context.Context, req ChatRequest) (ChatResult, error) {
	body := wireRequest{
		Model: c.model,
		Messages: []wireMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		Temperature: req.Opts.Temperature,
		TopP:        req.Opts.TopP,
		MaxTokens:   req.Opts.MaxTokens,
		Seed:        c.seed,
	}
	if req.Opts.JSONFormat {
		body.ResponseFormat = &struct {
			Type string `json:"type"`
		}{Type: "json_object"}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ChatResult{}, fmt.Errorf("goldbench: marshal request: %w", err)
	}
	if len(c.extraBody) > 0 {
		merged := map[string]json.RawMessage{}
		if err := json.Unmarshal(payload, &merged); err != nil {
			return ChatResult{}, fmt.Errorf("goldbench: merge extra-body: %w", err)
		}
		for k, v := range c.extraBody {
			if _, exists := merged[k]; !exists {
				merged[k] = v
			}
		}
		if payload, err = json.Marshal(merged); err != nil {
			return ChatResult{}, fmt.Errorf("goldbench: marshal merged request: %w", err)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return ChatResult{}, fmt.Errorf("goldbench: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return ChatResult{}, fmt.Errorf("goldbench: chat call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return ChatResult{}, fmt.Errorf("goldbench: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if isContextOverflowBody(string(raw)) {
			return ChatResult{}, fmt.Errorf("goldbench: chat HTTP %d: %s: %w", resp.StatusCode, truncateErr(raw), ErrContextOverflow)
		}
		return ChatResult{}, fmt.Errorf("goldbench: chat HTTP %d: %s", resp.StatusCode, truncateErr(raw))
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return ChatResult{}, fmt.Errorf("goldbench: decode response: %w", err)
	}
	if wr.Error != nil {
		if isContextOverflowBody(wr.Error.Message) {
			return ChatResult{}, fmt.Errorf("goldbench: chat error: %s: %w", wr.Error.Message, ErrContextOverflow)
		}
		return ChatResult{}, fmt.Errorf("goldbench: chat error: %s", wr.Error.Message)
	}
	if len(wr.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("goldbench: chat response without choices")
	}
	return ChatResult{
		Content:          wr.Choices[0].Message.Content,
		PromptTokens:     wr.Usage.PromptTokens,
		CompletionTokens: wr.Usage.CompletionTokens,
		FinishReason:     wr.Choices[0].FinishReason,
	}, nil
}

// truncateErr kürzt Fehler-Bodies für die Fehlermeldung.
func truncateErr(b []byte) string {
	const cap = 300
	s := string(b)
	if len(s) > cap {
		return s[:cap] + "…"
	}
	return s
}

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
		Message wireMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Chat führt einen Chat-Completions-Call aus und gibt den Antwort-Content
// zurück. Fehlerpfade sind mit %w gewrappt; der Aufrufer entscheidet über
// Retry-Politik (der Harness macht bewusst keine — ein Bench misst das
// Modell, nicht die Netz-Resilienz).
func (c *Client) Chat(ctx context.Context, req ChatRequest) (string, error) {
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
		return "", fmt.Errorf("goldbench: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("goldbench: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("goldbench: chat call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	if err != nil {
		return "", fmt.Errorf("goldbench: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("goldbench: chat HTTP %d: %s", resp.StatusCode, truncateErr(raw))
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return "", fmt.Errorf("goldbench: decode response: %w", err)
	}
	if wr.Error != nil {
		return "", fmt.Errorf("goldbench: chat error: %s", wr.Error.Message)
	}
	if len(wr.Choices) == 0 {
		return "", fmt.Errorf("goldbench: chat response without choices")
	}
	return wr.Choices[0].Message.Content, nil
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

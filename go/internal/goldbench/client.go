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

	"github.com/GottZ/ctx/internal/clientkey"
	"github.com/GottZ/ctx/internal/llm"
)

// keyNames are ctx-goldbench's words in a refusal of internal/clientkey.
var keyNames = clientkey.Names{Client: "ctx-goldbench", BaseURL: "-endpoint"}

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
	Temperature float64 `json:"temperature"`
	TopP        float64 `json:"top_p,omitempty"`       // 0 = weglassen
	MaxTokens   int     `json:"max_tokens,omitempty"`  // 0 = weglassen
	JSONFormat  bool    `json:"json_format,omitempty"` // response_format {"type":"json_object"} — classify (internal/llm/classify.go:181 Format:"json")
	// Seed je Request (KW4, Mehrfach-Sampling): 0 = Client-Seed (Sample 0 bleibt
	// bitkompatibel zur Historie); Sample s>0 trägt cfg.Seed+s — ohne dieses Feld
	// trügen alle N Wire-Requests denselben Seed und Server, die seed honorieren,
	// lieferten N identische Samples.
	Seed int64 `json:"seed,omitempty"`
	// TopLogprobs > 0 fordert logprobs/top_logprobs an (Decide-Achsen, die
	// die Antwort aus den Alternativen des ersten Tokens lesen —
	// llm.DecideOptions). 0 = weglassen; die Byte-Form bestehender Dumps
	// bleibt damit stabil.
	TopLogprobs int `json:"top_logprobs,omitempty"`
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
	Logprobs    bool `json:"logprobs,omitempty"`
	TopLogprobs int  `json:"top_logprobs,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
		Logprobs     *struct {
			Content []struct {
				Token       string   `json:"token"`
				Logprob     *float64 `json:"logprob"` // null = maskiert (llama.cpp), siehe llm.FirstTokenLogprobs
				TopLogprobs []struct {
					Token   string   `json:"token"`
					Logprob *float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage struct {
		PromptTokens            int `json:"prompt_tokens"`
		CompletionTokens        int `json:"completion_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
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
	ReasoningTokens  int // aus usage.completion_tokens_details (0 wenn Server es nicht liefert)
	FinishReason     string
	ThinkStripped    bool // Content enthielt <think>-Blöcke (client-seitig entfernt)
	// TopLogprobs sind die Alternativen des ersten Antwort-Tokens (nur wenn
	// TopLogprobs angefordert und vom Server geliefert; sonst nil) — dieselbe
	// Form wie llm.ChatResponse.TopLogprobs, inkl. des gesampelten Tokens.
	TopLogprobs []llm.TokenLogprob
}

// stripThink entfernt <think>…</think>-Blöcke aus dem Content — auch einen
// unterminierten Block (Truncation mitten im Denken). Kein Achsen-Contract
// enthält legitime Think-Tags; ohne Stripping bricht jeder strikte Parser
// an Servern, die Reasoning nicht als eigenes Feld separieren.
func stripThink(s string) (string, bool) {
	if !strings.Contains(s, "<think>") {
		return s, false
	}
	var b strings.Builder
	rest, stripped := s, true
	for {
		start := strings.Index(rest, "<think>")
		if start < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:start])
		end := strings.Index(rest[start:], "</think>")
		if end < 0 {
			break // unterminiert: Rest ist Denk-Text
		}
		rest = rest[start+end+len("</think>"):]
	}
	return strings.TrimSpace(b.String()), stripped
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

// ChatWithUsage führt einen Chat-Completions-Call aus und liefert neben dem
// Antwort-Content Usage und finish_reason (für Durchsatz- und Fail-Aggregation
// im Report). Fehlerpfade sind mit %w gewrappt; der Aufrufer entscheidet über
// Retry-Politik (der Harness macht bewusst keine — ein Bench misst das Modell,
// nicht die Netz-Resilienz).
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
	if req.Opts.Seed != 0 {
		body.Seed = req.Opts.Seed
	}
	if req.Opts.JSONFormat {
		body.ResponseFormat = &struct {
			Type string `json:"type"`
		}{Type: "json_object"}
	}
	if req.Opts.TopLogprobs > 0 {
		body.Logprobs = true
		body.TopLogprobs = req.Opts.TopLogprobs
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

	resp, err := c.send(httpReq)
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
	content, thinkStripped := stripThink(wr.Choices[0].Message.Content)
	out := ChatResult{
		Content:          content,
		PromptTokens:     wr.Usage.PromptTokens,
		CompletionTokens: wr.Usage.CompletionTokens,
		ReasoningTokens:  wr.Usage.CompletionTokensDetails.ReasoningTokens,
		FinishReason:     wr.Choices[0].FinishReason,
		ThinkStripped:    thinkStripped,
	}
	if lp := wr.Choices[0].Logprobs; lp != nil && len(lp.Content) > 0 {
		// Dieselbe Ableseregel wie der Produktions-Client (llm/client.go), damit
		// die Achse dieselbe Verteilung misst, die der Modus schreiben würde.
		first := lp.Content[0]
		alts := make([]llm.TokenLogprob, 0, len(first.TopLogprobs))
		for _, alt := range first.TopLogprobs {
			if alt.Logprob == nil {
				continue
			}
			alts = append(alts, llm.TokenLogprob{Token: alt.Token, Logprob: *alt.Logprob})
		}
		out.TopLogprobs = llm.FirstTokenLogprobs(first.Token, first.Logprob, alts)
	}
	return out, nil
}

// send is how a chat call leaves ctx-goldbench (#432, #446): through
// internal/clientkey, by the rule of the ctx CLI and ctx-armsweep — the call
// only to the origin (scheme, host, port) of the endpoint, and a redirect
// followed only within that origin; one anywhere else is refused before it is
// sent, the prompt with it. With an API key the key goes along as a bearer
// token (net/http alone keeps Authorization on another port or scheme of the
// same host name); without one no header is attached, and the rule still
// holds the prompt, which net/http alone re-sends on a 307 or 308 to any host.
func (c *Client) send(req *http.Request) (*http.Response, error) {
	kt, err := c.transport()
	if err != nil {
		return nil, err
	}
	return kt.Do(c.hc, req)
}

// transport binds the key, or the body without one, to the origin of the
// chat URL; an -endpoint without an http or https host is the error.
// executeJobs asks it once before the first call (#459), so such an endpoint
// ends the run instead of failing every case alike.
func (c *Client) transport() (clientkey.Transport, error) {
	var kt clientkey.Transport
	var err error
	if c.apiKey == "" {
		kt, err = clientkey.NewUnkeyed(c.url, keyNames)
	} else {
		kt, err = clientkey.NewBearer(c.url, c.apiKey, keyNames)
	}
	if err != nil {
		return clientkey.Transport{}, fmt.Errorf("-endpoint is not a usable URL: %w", err)
	}
	return kt, nil
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

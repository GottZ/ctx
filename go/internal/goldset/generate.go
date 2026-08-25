package goldset

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

// The G-Q prompt. Frozen: its sha256 goes into the stamp, so any later edit is
// visible as a changed provenance hash rather than as silent slice drift.
const (
	gqSystemPrompt = "You write retrieval evaluation questions. " +
		"Given a knowledge note, produce EXACTLY ONE question that the note's BODY answers. " +
		"Rules: the question must be answerable from the body text alone; " +
		"it must NOT quote or restate the note's title; " +
		"it must not mention 'the note', 'the document' or 'this text'; " +
		"write it in the same language as the body (German or English); " +
		"one line, ending with a question mark, no preamble, no quotes, no numbering."

	gqUserTemplate = "TITLE: %s\n\nBODY:\n%s\n\nQuestion:"
)

// PromptSHA256 digests the frozen prompt pair (template placeholders included,
// block content excluded — the hash identifies the prompt, not the corpus).
func PromptSHA256() string {
	return SHA256Hex(gqSystemPrompt + "\n\n" + gqUserTemplate)
}

// maxBodyChars bounds the prompt so one oversized block cannot dominate the
// generation window on a production serving host.
const maxBodyChars = 4000

// ChatClient is a minimal OpenAI-compatible chat client. It is deliberately
// separate from internal/llm: this tool must not be able to pick up a runtime
// backend switch and silently send private block content somewhere else.
type ChatClient struct {
	URL       string
	Model     string
	ExtraBody map[string]json.RawMessage
	HTTP      *http.Client
}

// NewChatClient builds a client for a verified on-prem backend. extraBody
// carries the row's own settings — for qwen38 on SGLang that is
// chat_template_kwargs.enable_thinking=false, which is mandatory.
func NewChatClient(b Backend, model string, timeout time.Duration) (*ChatClient, error) {
	if err := RequireOnPrem(b); err != nil {
		return nil, err
	}
	extra := map[string]json.RawMessage{}
	if len(b.ExtraBody) > 0 {
		if err := json.Unmarshal(b.ExtraBody, &extra); err != nil {
			return nil, fmt.Errorf("backend %q extra_body: %w", b.Name, err)
		}
	}
	if model == "" {
		model = b.DefaultModel()
	}
	if model == "" {
		return nil, fmt.Errorf("backend %q: no model in model_map.default", b.Name)
	}
	return &ChatClient{
		URL:       strings.TrimSuffix(b.BaseURL, "/") + "/v1/chat/completions",
		Model:     model,
		ExtraBody: extra,
		HTTP:      &http.Client{Timeout: timeout},
	}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Ask sends one completion and returns the trimmed first choice.
func (c *ChatClient) Ask(ctx context.Context, system, user string, maxTokens int, temperature float64) (string, error) {
	body := map[string]any{
		"model":       c.Model,
		"messages":    []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		"max_tokens":  maxTokens,
		"temperature": temperature,
		"stream":      false,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(raw, &merged); err != nil {
		return "", err
	}
	for k, v := range c.ExtraBody {
		if _, clash := merged[k]; !clash {
			merged[k] = v
		}
	}
	payload, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chat %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("chat: no choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// GQPrompt renders the frozen user prompt for one block.
func GQPrompt(b Block) string {
	body := b.Content
	if len(body) > maxBodyChars {
		body = body[:maxBodyChars]
	}
	return fmt.Sprintf(gqUserTemplate, b.Title, body)
}

// GQSystem exposes the frozen system prompt to the caller.
func GQSystem() string { return gqSystemPrompt }

// AcceptQuestion is the mechanical part of the G-Q quality filter: shape, and
// the one substantive rule the slice depends on — a question that restates the
// title has silently become a G-KI case and would import G-KI's trigram bias
// into the primary slice. Everything beyond this is the hand check (§4.5).
func AcceptQuestion(q, title string) (string, bool) {
	q = strings.TrimSpace(q)
	q = strings.Trim(q, "\"'`")
	if i := strings.IndexAny(q, "\n"); i >= 0 {
		q = strings.TrimSpace(q[:i])
	}
	if len(q) < 20 || !strings.HasSuffix(q, "?") {
		return "", false
	}
	if strings.Contains(q, "?") && strings.Count(q, "?") > 1 {
		return "", false
	}
	if t := strings.ToLower(normalizeTitle(title)); t != "" && strings.Contains(strings.ToLower(q), t) {
		return "", false
	}
	return q, true
}

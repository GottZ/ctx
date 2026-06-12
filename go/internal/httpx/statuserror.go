package httpx

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// StatusError is the typed form of a non-200 backend response. The wire
// clients (llm, embed, rerank) wrap it as fmt.Errorf("<pkg>: %w", err) so the
// rendered string stays byte-identical to the historical
// "<pkg>: unexpected status %d: %s" format while errors.As becomes possible —
// the backend pool's failover classifier needs the status code, not a string.
type StatusError struct {
	Code int
	// Body is the response body, already truncated by the caller (1 KiB).
	// It may carry provider prompts/fragments — log-only, never client-facing.
	Body string
	// RetryAfter is the parsed Retry-After header (0 = absent). Feeds the
	// 429 cooldown; only delta-seconds form is parsed (HTTP-date is rare on
	// inference APIs and the 60s class default covers it).
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Code, e.Body)
}

// NewStatusError builds a StatusError from a response status, truncated body
// and the raw Retry-After header value.
func NewStatusError(resp *http.Response, body []byte) *StatusError {
	e := &StatusError{Code: resp.StatusCode, Body: string(body)}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

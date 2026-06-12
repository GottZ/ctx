package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestStatusErrorFormatIdentity pins the F3-P1 wrap contract: the rendered
// string of fmt.Errorf("llm: %w", NewStatusError(...)) is byte-identical to
// the historical "llm: unexpected status %d: %s" — only errors.As is new.
func TestStatusErrorFormatIdentity(t *testing.T) {
	resp := &http.Response{StatusCode: 500, Header: http.Header{}}
	body := []byte("upstream exploded")

	wrapped := fmt.Errorf("llm: %w", NewStatusError(resp, body))
	historical := fmt.Sprintf("llm: unexpected status %d: %s", 500, "upstream exploded")
	if wrapped.Error() != historical {
		t.Fatalf("format drifted:\n got %q\nwant %q", wrapped.Error(), historical)
	}

	var se *StatusError
	if !errors.As(wrapped, &se) || se.Code != 500 {
		t.Fatal("errors.As did not reach the StatusError")
	}
}

func TestStatusErrorRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "90")
	se := NewStatusError(&http.Response{StatusCode: 429, Header: h}, nil)
	if se.RetryAfter != 90*time.Second {
		t.Fatalf("RetryAfter = %s, want 90s", se.RetryAfter)
	}

	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	se = NewStatusError(&http.Response{StatusCode: 429, Header: h}, nil)
	if se.RetryAfter != 0 {
		t.Fatalf("HTTP-date form must fall back to 0, got %s", se.RetryAfter)
	}
}

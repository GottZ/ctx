package store

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/httpx"
)

// --- NormalizeEmbedError ---.

func TestNormalizeEmbedError_PrefixesClass(t *testing.T) {
	got := NormalizeEmbedError(EmbedFailureWire, "boom")
	if got != "wire: boom" {
		t.Errorf("NormalizeEmbedError = %q, want %q", got, "wire: boom")
	}
}

func TestNormalizeEmbedError_StripsControlChars(t *testing.T) {
	got := NormalizeEmbedError(EmbedFailureWire, "line1\nline2\ttab\x00null")
	if strings.ContainsAny(got, "\n\t\x00") {
		t.Errorf("NormalizeEmbedError = %q, control chars survived", got)
	}
}

func TestNormalizeEmbedError_CapsAt500Runes(t *testing.T) {
	raw := strings.Repeat("x", 2000)
	got := NormalizeEmbedError(EmbedFailureWire, raw)
	if n := len([]rune(got)); n != maxLastErrorLen {
		t.Errorf("normalized length = %d, want %d", n, maxLastErrorLen)
	}
}

// --- ClassifyEmbedError ---.

func TestClassifyEmbedError_StatusErrorExceedContextSize_Oversize(t *testing.T) {
	err := fmt.Errorf("embed: %w", &httpx.StatusError{
		Code: http.StatusBadRequest,
		Body: `{"error":{"type":"exceed_context_size_error","message":"input too long"}}`,
	})
	class, msg := ClassifyEmbedError(err)
	if class != EmbedFailureOversize {
		t.Errorf("class = %q, want %q", class, EmbedFailureOversize)
	}
	if !strings.HasPrefix(msg, "oversize: ") {
		t.Errorf("normalized message = %q, want oversize-prefixed", msg)
	}
	if !strings.Contains(msg, "400") {
		t.Errorf("normalized message = %q, want HTTP status embedded", msg)
	}
}

func TestClassifyEmbedError_StatusErrorOther_Wire(t *testing.T) {
	err := fmt.Errorf("embed: %w", &httpx.StatusError{
		Code: http.StatusBadGateway,
		Body: "upstream unavailable",
	})
	class, msg := ClassifyEmbedError(err)
	if class != EmbedFailureWire {
		t.Errorf("class = %q, want %q", class, EmbedFailureWire)
	}
	if !strings.HasPrefix(msg, "wire: ") {
		t.Errorf("normalized message = %q, want wire-prefixed", msg)
	}
}

// W04-4 (Lead-Messung 2026-07-24): oversize is the 400-AND-substring
// contract — a 5xx that merely ECHOES the token in its body is a transient
// server condition and must keep the retryable wire class, never the
// permanent infinity park.
func TestClassifyEmbedError_NonBadRequestWithMarker_Wire(t *testing.T) {
	err := fmt.Errorf("embed: %w", &httpx.StatusError{
		Code: http.StatusInternalServerError,
		Body: `{"error":"proxy log: upstream said exceed_context_size once"}`,
	})
	class, _ := ClassifyEmbedError(err)
	if class != EmbedFailureWire {
		t.Errorf("class = %q, want %q (oversize is a 400-only classification)", class, EmbedFailureWire)
	}
}

func TestClassifyEmbedError_NonStatusError_Wire(t *testing.T) {
	class, msg := ClassifyEmbedError(errors.New("connection refused"))
	if class != EmbedFailureWire {
		t.Errorf("class = %q, want %q", class, EmbedFailureWire)
	}
	if !strings.Contains(msg, "connection refused") {
		t.Errorf("normalized message = %q, want raw error text preserved", msg)
	}
}

package handler

// MW8 (D3-W4, DECISIONS amendment B1): unit gates for the rejection → 429
// mapping logic shared by the query/embed/synthesis/daily sites — the
// Retry-After header (B1 estimate), the B6-generic body (no target/depth leak,
// C2 negative probe), host extraction from both AdmissionError forms, and the
// IsRejection routing that keeps NON-rejection errors on their 500/503 path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
)

// hintAdmitter is a fake Admitter that ALSO implements dispatch.RetryHinter,
// echoing a fixed estimate for a known origin (0 for anything else).
type hintAdmitter struct {
	origin string
	hint   time.Duration
}

func (a hintAdmitter) Acquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, dispatch.ErrTargetSaturated
}

func (a hintAdmitter) RetryAfterHint(origin string, _ dispatch.Class) time.Duration {
	if origin == a.origin {
		return a.hint
	}
	return 0
}

// plainAdmitter implements ONLY Admitter — no RetryHinter (the shape of a test
// fake or a non-dispatcher admitter): the header must be omitted, never faked.
type plainAdmitter struct{}

func (plainAdmitter) Acquire(context.Context, dispatch.Request) (*dispatch.Lease, context.Context, error) {
	return nil, nil, dispatch.ErrTargetSaturated
}

func TestSetRejectRetryAfterWithHinter(t *testing.T) {
	adm := hintAdmitter{origin: "http://gpu:8089", hint: 12 * time.Second}
	err := &llm.AdmissionError{Err: dispatch.ErrTargetSaturated, Host: "http://gpu:8089"}
	rr := httptest.NewRecorder()
	setRejectRetryAfter(rr, adm, err)
	if got := rr.Header().Get("Retry-After"); got != "12" {
		t.Fatalf("Retry-After = %q, want \"12\"", got)
	}
}

// The embed path rejects as an embedcache.AdmissionError (the mirror type) —
// admissionHost must read its Host too.
func TestSetRejectRetryAfterEmbedcacheError(t *testing.T) {
	adm := hintAdmitter{origin: "http://embed:8081", hint: 3 * time.Second}
	err := &embedcache.AdmissionError{Err: dispatch.ErrPrincipalSaturated, Host: "http://embed:8081"}
	rr := httptest.NewRecorder()
	setRejectRetryAfter(rr, adm, err)
	if got := rr.Header().Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q, want \"3\" (embedcache host extracted)", got)
	}
}

func TestSetRejectRetryAfterNoHinter(t *testing.T) {
	err := &llm.AdmissionError{Err: dispatch.ErrTargetSaturated, Host: "http://gpu:8089"}
	rr := httptest.NewRecorder()
	setRejectRetryAfter(rr, plainAdmitter{}, err)
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want omitted (admitter is no RetryHinter)", got)
	}
}

func TestSetRejectRetryAfterUnknownHost(t *testing.T) {
	adm := hintAdmitter{origin: "http://gpu:8089", hint: 12 * time.Second}
	// Error carries a DIFFERENT host ⇒ hint 0 ⇒ header omitted.
	err := &llm.AdmissionError{Err: dispatch.ErrTargetSaturated, Host: "http://other:1234"}
	rr := httptest.NewRecorder()
	setRejectRetryAfter(rr, adm, err)
	if got := rr.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want omitted (unknown host, no fabricated value)", got)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 0},
		{-5 * time.Second, 0},
		{time.Second, 1},
		{1500 * time.Millisecond, 2}, // ceil
		{12 * time.Second, 12},
		{30 * time.Second, 30},
		{500 * time.Millisecond, 1}, // sub-second rounds up to 1
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.in); got != c.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestRejectionBodyGeneric is the C2 negative probe: the 429 body carries the
// fixed B6 text and success:false — and NOTHING that identifies the target,
// depth, backend, or which Deckel-Staffel cap fired.
func TestRejectionBodyGeneric(t *testing.T) {
	raw, err := json.Marshal(rejectionBody())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ToLower(string(raw))
	if !strings.Contains(body, "\"success\":false") {
		t.Fatalf("body not a generic fail: %s", body)
	}
	if !strings.Contains(body, "server busy") {
		t.Fatalf("body missing the §3.3 generic text: %s", body)
	}
	for _, leak := range []string{"http", "gpu", "8089", "8081", "backend", "origin", "queue", "depth", "tenant", "principal", "saturat"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body leaks topology signal %q: %s", leak, body)
		}
	}
}

// TestIsRejectionRouting pins the ONE check point: every Deckel-Staffel
// sentinel (wrapped in an AdmissionError, as it arrives at the handler) routes
// to the 429 branch; a non-rejection error (ctx cancel, would-block, the
// nil-admitter wiring error, a plain fault) does NOT — it stays on its
// 500/503 path (C1: capacity ≠ fault, and a real fault must not become a 429).
func TestIsRejectionRouting(t *testing.T) {
	rejections := []error{
		&llm.AdmissionError{Err: dispatch.ErrQueueFull},
		&llm.AdmissionError{Err: dispatch.ErrPrincipalSaturated},
		&llm.AdmissionError{Err: dispatch.ErrTenantSaturated},
		&llm.AdmissionError{Err: dispatch.ErrTargetSaturated},
		&embedcache.AdmissionError{Err: dispatch.ErrTargetSaturated},
		fmt.Errorf("llm: synthesize: %w", &llm.AdmissionError{Err: dispatch.ErrTenantSaturated}),
	}
	for i, e := range rejections {
		if !dispatch.IsRejection(e) {
			t.Errorf("rejection[%d] = %v: IsRejection = false, want true", i, e)
		}
	}
	notRejections := []error{
		context.Canceled,
		dispatch.ErrWouldBlock,
		&llm.AdmissionError{Err: fmt.Errorf("llm: chat call site without dispatch admitter (I-D1)")},
		errors.New("boom: a real 500-class fault"),
		fmt.Errorf("embedding failed: %w", context.DeadlineExceeded),
	}
	for i, e := range notRejections {
		if dispatch.IsRejection(e) {
			t.Errorf("non-rejection[%d] = %v: IsRejection = true, want false (stays 500/503)", i, e)
		}
	}
}

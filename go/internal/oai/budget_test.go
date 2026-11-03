package oai_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// countingBody records whether anyone read the request body.
type countingBody struct {
	r     io.Reader
	reads atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.r.Read(p)
}

func (c *countingBody) Close() error { return nil }

// TestInflightBudgetRejectsSecondBody is B9a (design/02 §5): with a budget
// that holds one large body's reservation (3 × Content-Length) but not two,
// the second of two concurrent large requests is refused at once — 503
// server_overloaded with Retry-After: 1 — without its body being read and
// without the handler running.
func TestInflightBudgetRejectsSecondBody(t *testing.T) {
	const bodyLen = 1 << 20
	if got := b9aSecond(t, func(inner http.Handler) http.Handler {
		var budget oai.Budget
		return oai.InflightBudget(&budget, func() oai.Settings {
			return oai.Settings{InflightBudget: 3*bodyLen + bodyLen} // one body's share fits, two do not
		})(inner)
	}); len(got) > 0 {
		for _, v := range got {
			t.Error(v)
		}
	}
}

// TestInflightBudgetCounterproof keeps the red side of B9a reproducible:
// without the budget stage the second large body reaches the handler.
func TestInflightBudgetCounterproof(t *testing.T) {
	got := b9aSecond(t, func(inner http.Handler) http.Handler { return inner })
	for _, v := range got {
		t.Log(v)
	}
	if len(got) == 0 {
		t.Error("the stack without a budget passes B9a — the probe has lost its teeth")
	}
}

// b9aSecond runs two concurrent 1 MiB requests through wrap(handler): the
// first holds its slot in the handler while the second arrives. It returns
// what is wrong with the second answer (empty = refused as B9a demands).
func b9aSecond(t *testing.T, wrap func(http.Handler) http.Handler) []string {
	t.Helper()
	const bodyLen = 1 << 20
	var out []string
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	stack := wrap(inner)

	firstDone := make(chan int)
	go func() {
		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(strings.Repeat("a", bodyLen))))
		firstDone <- rec.Code
	}()
	<-entered

	body := &countingBody{r: strings.NewReader(strings.Repeat("b", bodyLen))}
	req := httptest.NewRequest(http.MethodPost, "/v1/x", body)
	req.ContentLength = bodyLen
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)
	close(release)
	if code := <-firstDone; code != http.StatusOK {
		out = append(out, fmt.Sprintf("first request: status %d, want 200", code))
	}

	if rec.Code != http.StatusServiceUnavailable {
		out = append(out, fmt.Sprintf("second request: status %d, want 503 (budget exhausted)", rec.Code))
	}
	if n := body.reads.Load(); n != 0 {
		out = append(out, fmt.Sprintf("second request: body read %d time(s), want 0 — the refusal comes before the body", n))
	}
	if n := calls.Load(); n != 1 {
		out = append(out, fmt.Sprintf("handler ran %d time(s), want 1", n))
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		out = append(out, fmt.Sprintf("Retry-After %q, want 1", got))
	}
	if got := rec.Header().Get("x-should-retry"); got != "true" {
		out = append(out, fmt.Sprintf("x-should-retry %q, want true", got))
	}
	var env struct {
		Error struct {
			Type string  `json:"type"`
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code == nil || *env.Error.Code != "server_overloaded" ||
		env.Error.Type != "service_unavailable_error" {
		out = append(out, fmt.Sprintf("body %q, want the OpenAI envelope with code server_overloaded", rec.Body.String()))
	}
	return out
}

// TestReservationAPI: Grow admits up to the limit and refuses past it without
// taking anything, Release returns everything once (idempotent), and the
// process-wide counter is shared by every reservation of one Budget.
func TestReservationAPI(t *testing.T) {
	var b oai.Budget
	a := b.NewReservation(100)
	if !a.Grow(60) || !a.Grow(40) {
		t.Fatal("grow within the limit refused")
	}
	if a.Grow(1) {
		t.Error("grow past the limit admitted")
	}
	c := b.NewReservation(100)
	if c.Grow(1) {
		t.Error("second reservation admitted while the budget is full")
	}
	if got := b.InUse(); got != 100 {
		t.Errorf("in use %d, want 100", got)
	}
	a.Release()
	a.Release()
	if got := b.InUse(); got != 0 {
		t.Errorf("in use after release %d, want 0 (double release must not go negative)", got)
	}
	if !c.Grow(100) {
		t.Error("budget not available after release")
	}
	if c.Grow(-1) {
		t.Error("negative grow admitted")
	}
	c.Release()
	if got := b.InUse(); got != 0 {
		t.Errorf("in use %d, want 0", got)
	}
}

// TestInflightBudgetChunkedCountsWhileReading: a body without a declared
// length is reserved as it is read (3 × bytes read); past the budget the
// read fails with ErrBudgetExhausted, and everything is released when the
// handler returns.
func TestInflightBudgetChunkedCountsWhileReading(t *testing.T) {
	var b oai.Budget
	h := oai.InflightBudget(&b, func() oai.Settings { return oai.Settings{InflightBudget: 3 * 1000} })
	var readErr error
	var during int64
	stack := h(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
		during = b.InUse()
		if res := oai.ReservationFrom(r.Context()); res == nil {
			t.Error("no reservation in the request context")
		}
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/x", io.NopCloser(strings.NewReader(strings.Repeat("a", 999))))
	req.ContentLength = -1
	stack.ServeHTTP(httptest.NewRecorder(), req)
	if readErr != nil {
		t.Errorf("999 bytes under a 1000-byte share: read error %v", readErr)
	}
	if during != 3*999 {
		t.Errorf("reserved while reading %d, want %d", during, 3*999)
	}
	if got := b.InUse(); got != 0 {
		t.Errorf("in use after the handler %d, want 0", got)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/x", io.NopCloser(strings.NewReader(strings.Repeat("a", 5000))))
	req.ContentLength = -1
	stack.ServeHTTP(httptest.NewRecorder(), req)
	if !errors.Is(readErr, oai.ErrBudgetExhausted) {
		t.Errorf("over-budget chunked body: read error %v, want ErrBudgetExhausted", readErr)
	}
	if got := b.InUse(); got != 0 {
		t.Errorf("in use after the refused read %d, want 0", got)
	}
}

// TestInflightBudgetReleasesOnPanic: the share is returned by defer, so a
// panicking handler does not leak budget.
func TestInflightBudgetReleasesOnPanic(t *testing.T) {
	var b oai.Budget
	stack := oai.Recover(oai.InflightBudget(&b, func() oai.Settings { return oai.Settings{InflightBudget: 1 << 20} })(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("hello"))
	stack.ServeHTTP(httptest.NewRecorder(), req)
	if got := b.InUse(); got != 0 {
		t.Errorf("in use after a panic %d, want 0", got)
	}
}

// TestReservationSealedAfterRelease: a Grow that comes after Release — or
// races with it — takes nothing, so no byte stays reserved after the
// request is done (Fremd-Review finding 2: before the seal, 19 999 of 20 000
// such races leaked).
func TestReservationSealedAfterRelease(t *testing.T) {
	var b oai.Budget
	r := b.NewReservation(100)
	if !r.Grow(10) {
		t.Fatal("grow refused")
	}
	r.Release()
	if r.Grow(50) {
		t.Error("grow after release admitted")
	}
	if got := b.InUse(); got != 0 {
		t.Errorf("in use %d after a grow on a released reservation, want 0", got)
	}

	for i := 0; i < 2000; i++ {
		r := b.NewReservation(1 << 20)
		done := make(chan struct{})
		go func() {
			defer close(done)
			r.Grow(7)
		}()
		r.Release()
		<-done
		if got := b.InUse(); got != 0 {
			t.Fatalf("round %d: %d bytes left reserved after release raced a grow", i, got)
		}
	}
}

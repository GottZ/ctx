package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// kaWriter is a ResponseWriter that counts what the keepalive does to it:
// statuses, body bytes, flushes and write-deadline rolls.
type kaWriter struct {
	mu        sync.Mutex
	hdr       http.Header
	status    int
	body      bytes.Buffer
	flushes   int
	deadlines int
}

func newKAWriter() *kaWriter { return &kaWriter{hdr: make(http.Header)} }

func (f *kaWriter) Header() http.Header { return f.hdr }

func (f *kaWriter) WriteHeader(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == 0 {
		f.status = code
	}
}

func (f *kaWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == 0 {
		f.status = http.StatusOK
	}
	return f.body.Write(b)
}

func (f *kaWriter) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
}

func (f *kaWriter) SetWriteDeadline(time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadlines++
	return nil
}

func (f *kaWriter) snapshot() (status int, body string, deadlines int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.body.String(), f.deadlines
}

// An answer before the first tick is the answer writeJSON gives without the
// keepalive: same status, same headers (a handler header included), same
// bytes — with the keepalive bytes armed as well as without.
func TestQueryKeepaliveAnswerBeforeFirstTickIsUnchanged(t *testing.T) {
	shrinkHeartbeat(t, time.Hour, time.Hour)
	body := map[string]any{"success": false, "error": "server busy — retry shortly"}

	want := httptest.NewRecorder()
	want.Header().Set("Retry-After", "3")
	writeJSON(want, http.StatusTooManyRequests, body)

	for _, armed := range []bool{false, true} {
		got := httptest.NewRecorder()
		kw := armQueryKeepalive(context.Background(), got, armed)
		kw.Header().Set("Retry-After", "3")
		writeJSON(kw, http.StatusTooManyRequests, body)
		kw.close()
		if got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("bytes armed=%v: %d %q, want %d %q", armed, got.Code, got.Body.String(), want.Code, want.Body.String())
		}
		if g, w := got.Header().Get("Retry-After"), want.Header().Get("Retry-After"); g != w {
			t.Errorf("bytes armed=%v: Retry-After %q, want %q", armed, g, w)
		}
		if g, w := got.Header().Get("Content-Type"), want.Header().Get("Content-Type"); g != w {
			t.Errorf("bytes armed=%v: Content-Type %q, want %q", armed, g, w)
		}
	}
}

// Without the keepalive bytes the ticks only roll the write deadline: many
// ticks, not one byte and no status on the wire.
func TestQueryKeepaliveDeadlineRollWritesNothing(t *testing.T) {
	shrinkHeartbeat(t, 5*time.Millisecond, time.Second)
	f := newKAWriter()
	kw := armQueryKeepalive(context.Background(), f, false)
	time.Sleep(60 * time.Millisecond)
	status, body, deadlines := f.snapshot()
	if status != 0 || body != "" {
		t.Errorf("status %d body %q before the answer, want nothing on the wire", status, body)
	}
	if deadlines < 3 {
		t.Errorf("write deadline rolled %d times in 60 ms at a 5 ms interval", deadlines)
	}
	writeJSON(kw, http.StatusOK, map[string]any{"success": true})
	kw.close()
	if status, body, _ := f.snapshot(); status != http.StatusOK || body != "{\"success\":true}\n" {
		t.Errorf("answer %d %q, want the plain 200 body", status, body)
	}
}

// With the bytes armed the first tick commits 200 and writes spaces; a status
// the handler answers afterwards is dropped, its body follows the spaces, and
// no space is written after it.
func TestQueryKeepaliveStatusAfterFirstTick(t *testing.T) {
	shrinkHeartbeat(t, 5*time.Millisecond, time.Second)
	f := newKAWriter()
	kw := armQueryKeepalive(context.Background(), f, true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, body, _ := f.snapshot(); body != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no keepalive byte within 2 s at a 5 ms interval")
		}
		time.Sleep(time.Millisecond)
	}
	writeJSON(kw, http.StatusInternalServerError, map[string]string{"error": "embedding failed"})
	_, atAnswer, _ := f.snapshot()
	time.Sleep(40 * time.Millisecond) // ticks that must not write any more
	kw.close()

	status, body, _ := f.snapshot()
	if status != http.StatusOK {
		t.Errorf("status %d, want the keepalive's 200", status)
	}
	if body != atAnswer {
		t.Errorf("body grew after the answer: %q -> %q", atAnswer, body)
	}
	if !strings.HasPrefix(body, " ") || strings.TrimLeft(body, " ") != "{\"error\":\"embedding failed\"}\n" {
		t.Errorf("body %q, want keepalive spaces and then the error body", body)
	}
	if ct := f.hdr.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
}

// close ends the ticker: no roll and no byte after it.
func TestQueryKeepaliveNothingAfterClose(t *testing.T) {
	shrinkHeartbeat(t, 5*time.Millisecond, time.Second)
	f := newKAWriter()
	kw := armQueryKeepalive(context.Background(), f, true)
	kw.close()
	_, body, deadlines := f.snapshot()
	time.Sleep(40 * time.Millisecond)
	if _, b, d := f.snapshot(); b != body || d != deadlines {
		t.Errorf("after close: body %q -> %q, deadline rolls %d -> %d", body, b, deadlines, d)
	}
	if n, err := kw.Write([]byte("x")); err == nil || n != 0 {
		t.Errorf("Write after close = %d, %v; want 0 and an error", n, err)
	}
}

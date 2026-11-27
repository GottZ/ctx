package httpx

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newReq(t *testing.T, ctx context.Context, url string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// resetNTimes hijacks and hard-closes the first n connections (client sees a
// reset/EOF before any response), then answers normally.
func resetNTimes(n int64, hits *atomic.Int64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= n {
			hj, ok := w.(http.Hijacker)
			if !ok {
				panic("not hijackable")
			}
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = io.WriteString(w, "ok")
	}
}

func TestDoRetryOnceRecoversFromSingleReset(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(resetNTimes(1, &hits))
	defer srv.Close()
	// Fresh connection per attempt so Go's own reused-conn retry stays out.
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	body := []byte(`{"x":1}`)
	resp, err := DoRetryOnce(c, newReq(t, context.Background(), srv.URL, body), body)
	if err != nil {
		t.Fatalf("want recovery after one reset, got error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if got, _ := io.ReadAll(resp.Body); string(got) != "ok" {
		t.Errorf("body = %q, want ok", got)
	}
	if hits.Load() != 2 {
		t.Errorf("server hits = %d, want 2 (original + one retry)", hits.Load())
	}
}

func TestDoRetryOnceGivesUpAfterSecondReset(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(resetNTimes(99, &hits))
	defer srv.Close()
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	body := []byte(`{}`)
	_, err := DoRetryOnce(c, newReq(t, context.Background(), srv.URL, body), body) //nolint:bodyclose // error path, no body
	if err == nil {
		t.Fatal("want error when every attempt is reset")
	}
	if hits.Load() != 2 {
		t.Errorf("server hits = %d, want exactly 2 (no retry storm)", hits.Load())
	}
}

func TestDoRetryOnceSkipsRetryOnContextDeadline(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	body := []byte(`{}`)
	_, err := DoRetryOnce(http.DefaultClient, newReq(t, ctx, srv.URL, body), body) //nolint:bodyclose // error path, no body
	if err == nil {
		t.Fatal("want deadline error")
	}
	if hits.Load() != 1 {
		t.Errorf("server hits = %d, want 1 (deadline must not retry)", hits.Load())
	}
}

// TestDoRetryOnceLogsNoServerText: a response that breaks the HTTP parse
// is a transient error when its text matches a transient phrase — and that
// text quotes what the server sent (here a malformed header line), which can
// echo the request. The retry's log line names the kind of error, never its
// text; the retry itself is unchanged.
func TestDoRetryOnceLogsNoServerText(t *testing.T) {
	const secret = "password=Tr0ub4dor-retry-575"
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nconnection reset by peer " + secret + "\r\n\r\n")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer srv.Close()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	c := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	body := []byte(`{}`)
	_, err := DoRetryOnce(c, newReq(t, context.Background(), srv.URL, body), body) //nolint:bodyclose // error path, no body
	if err == nil || !strings.Contains(err.Error(), "connection reset by peer") {
		t.Fatalf("want the broken response's error, got %v", err)
	}
	if hits.Load() != 2 {
		t.Errorf("server hits = %d, want 2 (the retry stays)", hits.Load())
	}
	out := logs.String()
	if !strings.Contains(out, "retrying once") {
		t.Fatalf("no retry line: %s", out)
	}
	if strings.Contains(out, "Tr0ub4dor") {
		t.Errorf("the retry line carries the server's text: %s", out)
	}
}

package httpx

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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

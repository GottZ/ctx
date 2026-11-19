//go:build integration

// Issue #220: POST /api/query with synthesize:false wrote no byte until it was
// done, so nothing moved http.Server's absolute WriteTimeout and an answer past
// it was logged as a 200 and never delivered. The tests drive the REAL handler
// behind a real http.Server whose WriteTimeout is shrunk to 300 ms, with an
// embed backend that answers after 900 ms — three times that deadline, in the
// phase before the search, where the old body heartbeat never ran.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryKeepalive -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// kaSlowEmbed is the embed half of bw2NewBackend that answers after d. With
// fail set it answers 500 instead of a vector, the late failure of the
// question's embed.
func kaSlowEmbed(t *testing.T, d time.Duration, fail bool) *httptest.Server {
	t.Helper()
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// kaServer serves the query handler behind an http.Server with the given
// WriteTimeout, as the bw2 user key.
func kaServer(t *testing.T, h *QueryHandler, writeTimeout time.Duration) *httptest.Server {
	t.Helper()
	ar := &auth.AuthResult{ApiKeyID: bw2UserKeyID, HomeScope: bw2Scope, ReadScopes: []string{bw2Scope}, IsValid: true}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.HandleQuery(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}))
	srv.Config.WriteTimeout = writeTimeout
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// kaPost sends one query and returns the status, the time until the first
// body byte arrived, and the whole body. A transport error (the connection cut
// at the WriteTimeout) fails the test.
func kaPost(t *testing.T, url, body string) (int, time.Duration, []byte) {
	t.Helper()
	start := time.Now()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v — the answer did not arrive (cut at the server WriteTimeout?)", body, err)
	}
	defer func() { _ = resp.Body.Close() }()
	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first body byte: %v — the answer did not arrive (cut at the server WriteTimeout?)", err)
	}
	ttfb := time.Since(start)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, ttfb, append(first, rest...)
}

// kaHandler builds a query handler with its own backend pool, so the health
// a failed embed reports in one request cannot short-circuit the next one.
func kaHandler(t *testing.T, pool *pgxpool.Pool, embedHost string) *QueryHandler {
	t.Helper()
	return NewQueryHandler(pool, config.NewStore(bw2Config()), bw2Pool(embedHost), nil,
		blocktype.NewRegistry(), snapshotTestAdmitter(t))
}

func kaSources(t *testing.T, body []byte) int {
	t.Helper()
	var resp struct {
		Success bool  `json:"success"`
		Sources []any `json:"sources"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	if !resp.Success {
		t.Fatalf("success=false: %s", body)
	}
	return len(resp.Sources)
}

// TestQueryKeepaliveRetrievalOnlyOutlivesWriteTimeout: without the new field a
// retrieval-only answer that takes three WriteTimeouts arrives, as the same
// plain JSON body it always had — the write deadline rolls while the request
// works, no byte goes out before the answer.
//
// RED before the fix: the client reads EOF (the answer is written after the
// absolute deadline and the connection is closed).
func TestQueryKeepaliveRetrievalOnlyOutlivesWriteTimeout(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 500*time.Millisecond)
	h := kaHandler(t, bw2Setup(t), kaSlowEmbed(t, 900*time.Millisecond, false).URL)
	srv := kaServer(t, h, 300*time.Millisecond)

	status, _, body := kaPost(t, srv.URL, `{"query":"`+bw2GoldenQuery+`","synthesize":false}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}
	if body[0] != '{' {
		t.Errorf("body starts with %q — a request without keepalive must not get keepalive bytes", body[:min(8, len(body))])
	}
	if n := kaSources(t, body); n != bw2Blocks {
		t.Errorf("sources = %d, want %d", n, bw2Blocks)
	}
}

// TestQueryKeepaliveFromTheStart: with keepalive:true the response commits 200
// on the first tick, long before the slow embed answers, and the JSON follows
// the whitespace.
//
// RED before the fix: the field is ignored, no byte arrives before the answer,
// and the answer itself is cut at the WriteTimeout.
func TestQueryKeepaliveFromTheStart(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 500*time.Millisecond)
	h := kaHandler(t, bw2Setup(t), kaSlowEmbed(t, 900*time.Millisecond, false).URL)
	srv := kaServer(t, h, 300*time.Millisecond)

	status, ttfb, body := kaPost(t, srv.URL, `{"query":"`+bw2GoldenQuery+`","synthesize":false,"keepalive":true}`)
	if status != http.StatusOK {
		t.Fatalf("status %d, body %s", status, body)
	}
	if ttfb >= 600*time.Millisecond {
		t.Errorf("first byte after %v — the keepalive did not commit before the 900 ms embed answered", ttfb)
	}
	if body[0] != ' ' {
		t.Errorf("body starts with %q, want the keepalive space", body[:min(8, len(body))])
	}
	if n := kaSources(t, body); n != bw2Blocks {
		t.Errorf("sources = %d, want %d", n, bw2Blocks)
	}
}

// TestQueryKeepaliveLateFailure: a failure of the question's embed after the
// first tick. Without the field the real status still arrives (the deadline
// rolled, no byte went out) — retrieval-only and on the synthesis path, whose
// keepalive bytes start only after the search; with keepalive:true the 200 is
// already on the wire and the body carries the error.
//
// RED before the fix: both answers are cut at the WriteTimeout.
func TestQueryKeepaliveLateFailure(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 500*time.Millisecond)
	pool := bw2Setup(t)
	embed := kaSlowEmbed(t, 900*time.Millisecond, true).URL
	srv := kaServer(t, kaHandler(t, pool, embed), 300*time.Millisecond)

	status, _, body := kaPost(t, srv.URL, `{"query":"`+bw2GoldenQuery+`","synthesize":false}`)
	if status != http.StatusInternalServerError || string(body) != "{\"error\":\"embedding failed\"}\n" {
		t.Errorf("without keepalive: status %d body %q, want 500 and the unchanged error body", status, body)
	}
	// The synthesis path without the field: its bytes start only after the
	// search, so a refusal before it keeps its status as well.
	srv = kaServer(t, kaHandler(t, pool, embed), 300*time.Millisecond)
	status, _, body = kaPost(t, srv.URL, `{"query":"`+bw2GoldenQuery+`"}`)
	if status != http.StatusInternalServerError || string(body) != "{\"error\":\"embedding failed\"}\n" {
		t.Errorf("synthesis path without keepalive: status %d body %q, want 500 and the unchanged error body", status, body)
	}

	srv = kaServer(t, kaHandler(t, pool, embed), 300*time.Millisecond)
	status, _, body = kaPost(t, srv.URL, `{"query":"`+bw2GoldenQuery+`","synthesize":false,"keepalive":true}`)
	if status != http.StatusOK {
		t.Errorf("with keepalive: status %d, want the 200 the first tick committed", status)
	}
	if body[0] != ' ' || strings.TrimLeft(string(body), " ") != "{\"error\":\"embedding failed\"}\n" {
		t.Errorf("with keepalive: body %q, want keepalive spaces and then the error body", body)
	}
}

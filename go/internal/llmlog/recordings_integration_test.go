//go:build integration

// The recordings store (issue #571, migration 197) over a real database:
//
//   - TestRecordingsRoundTrip: Open holds the body byte for byte (invalid
//     UTF-8 and NUL included) in state open; Finish ends it once; Insert
//     writes a refused request's row done, ends an open row of the same id
//     (an Open whose commit its caller did not see) and leaves a done one;
//
//   - TestRecordingsJanitor: EvictBodies NULLs both bodies of a recording
//     past the retention, keeps its sizes and outcome, and leaves a fresh
//     one alone;
//
//   - TestRecordingStatementCost (opt-in, CTX_RECORD_PROBE=1): the time and
//     the bytes one Open allocates for bodies up to 8 MiB
//     (gateway.max_request_bytes) — the measure behind oai.recordCopyFactor.
//
//     go test -tags=integration -p 1 -count=1 -run 'TestRecordings' -v ./internal/llmlog/
//     CTX_RECORD_PROBE=1 go test -tags=integration -p 1 -count=1 -run TestRecordingStatementCost -v ./internal/llmlog/
package llmlog_test

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

func recID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}

type storedRecording struct {
	state, path              string
	op, model, code, process *string
	stream                   *bool
	status                   *int16
	declared                 *int64
	reqBytes, respBytes      int64
	complete                 bool
	body, response           []byte
	finished                 *time.Time
}

func readRecording(t *testing.T, ctx context.Context, q *pgxpool.Pool, id string) storedRecording {
	t.Helper()
	var r storedRecording
	if err := q.QueryRow(ctx, `SELECT state, path, op, model, code, process_id::text, stream, status, declared_bytes,
		       request_bytes, response_bytes, request_complete, request_body, response_body, finished_at
		  FROM context_llm_recordings WHERE id = $1::uuid`, id).
		Scan(&r.state, &r.path, &r.op, &r.model, &r.code, &r.process, &r.stream, &r.status, &r.declared,
			&r.reqBytes, &r.respBytes, &r.complete, &r.body, &r.response, &r.finished); err != nil {
		t.Fatalf("read recording %s: %v", id, err)
	}
	return r
}

func TestRecordingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	q := pool
	st := llmlog.Recordings{Pool: pool}
	body := append([]byte(`{"model":"m","input":"`), 0xff, 0xfe, 0x00, 'x')
	body = append(body, `","secret":"password=s3cr3t-571"}`...)
	stream := false
	req := llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, ModelKeyID: "not-a-uuid", Path: "/v1/embeddings",
		Op: llmlog.GatewayOpEmbed, Model: "m", Stream: &stream, Declared: int64(len(body)), Body: body, Complete: true}
	if err := st.Open(ctx, req); err != nil {
		t.Fatalf("open: %v", err)
	}
	r := readRecording(t, ctx, q, req.ID)
	if r.state != llmlog.RecordingOpen || r.finished != nil || !bytes.Equal(r.body, body) || r.reqBytes != int64(len(body)) ||
		!r.complete || r.process == nil || *r.process != llmlog.ProcessID || r.status != nil {
		t.Fatalf("open row: state %q finished %v body exact %v (%d bytes) complete %v process %v status %v",
			r.state, r.finished, bytes.Equal(r.body, body), r.reqBytes, r.complete, r.process, r.status)
	}
	answer := []byte("the answer \x00\xff")
	if err := st.Finish(ctx, req.ID, llmlog.RecordedOutcome{Status: 200, Response: answer}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r = readRecording(t, ctx, q, req.ID)
	if r.state != llmlog.RecordingDone || r.finished == nil || r.status == nil || *r.status != 200 || r.code != nil ||
		!bytes.Equal(r.response, answer) || r.respBytes != int64(len(answer)) || !bytes.Equal(r.body, body) {
		t.Fatalf("finished row: state %q status %v code %v answer exact %v", r.state, r.status, r.code, bytes.Equal(r.response, answer))
	}
	if err := st.Finish(ctx, req.ID, llmlog.RecordedOutcome{Status: 503}); err == nil {
		t.Errorf("a second Finish of the same row succeeded; an outcome is written once")
	}
	// An answer committed and then withheld from the client: the outcome
	// the client got supersedes it on the done row — and on an open one.
	if err := st.Finish(ctx, req.ID, llmlog.RecordedOutcome{Status: 401, Code: "invalid_api_key", Response: []byte("refusal"), Supersedes: true}); err != nil {
		t.Fatalf("superseding finish: %v", err)
	}
	r = readRecording(t, ctx, q, req.ID)
	if r.state != llmlog.RecordingDone || r.status == nil || *r.status != 401 || r.code == nil || *r.code != "invalid_api_key" ||
		string(r.response) != "refusal" || r.respBytes != int64(len("refusal")) || !bytes.Equal(r.body, body) {
		t.Errorf("superseded row: state %q status %v code %v answer %q (%d bytes), request kept %v — want done 401 invalid_api_key with the refusal",
			r.state, r.status, r.code, r.response, r.respBytes, bytes.Equal(r.body, body))
	}
	open := llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, Path: "/v1/chat/completions", Body: []byte("asked"), Complete: true}
	if err := st.Open(ctx, open); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Finish(ctx, open.ID, llmlog.RecordedOutcome{Status: 503, Code: "recording_unavailable", Supersedes: true}); err != nil {
		t.Fatalf("superseding finish of an open row: %v", err)
	}
	if r = readRecording(t, ctx, q, open.ID); r.state != llmlog.RecordingDone || r.status == nil || *r.status != 503 || r.response != nil {
		t.Errorf("open row after a superseding finish: state %q status %v answer %v — want done 503 without an answer", r.state, r.status, r.response)
	}

	// A request refused before its walk: done at once, with the part read.
	refused := llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, Path: "/v1/chat/completions", Op: llmlog.GatewayOpChat,
		Declared: 400, Body: []byte(`{"model":`), Complete: false}
	if err := st.Insert(ctx, refused, llmlog.RecordedOutcome{Status: 400, Code: "invalid_json"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	r = readRecording(t, ctx, q, refused.ID)
	if r.state != llmlog.RecordingDone || r.complete || string(r.body) != `{"model":` || r.declared == nil || *r.declared != 400 ||
		r.code == nil || *r.code != "invalid_json" || r.response != nil || r.stream != nil || r.model != nil {
		t.Errorf("refused row: state %q complete %v body %q declared %v code %v response %v stream %v model %v",
			r.state, r.complete, r.body, r.declared, r.code, r.response, r.stream, r.model)
	}
	// A refusal before any body was read: no body, the declared size, no op.
	stage := llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, Path: "/v1/nowhere", Declared: -1}
	if err := st.Insert(ctx, stage, llmlog.RecordedOutcome{Status: 404, Code: "not_found"}); err != nil {
		t.Fatalf("insert stage refusal: %v", err)
	}
	if r = readRecording(t, ctx, q, stage.ID); r.body != nil || r.reqBytes != 0 || r.declared != nil || r.op != nil {
		t.Errorf("stage refusal row: body %v bytes %d declared %v op %v, want none of them", r.body, r.reqBytes, r.declared, r.op)
	}

	// An Open the caller saw fail although it committed: Insert ends it and
	// keeps its request.
	lost := llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, Path: "/v1/chat/completions", Body: []byte("held"), Complete: true}
	if err := st.Open(ctx, lost); err != nil {
		t.Fatalf("open: %v", err)
	}
	other := lost
	other.Body = []byte("other")
	if err := st.Insert(ctx, other, llmlog.RecordedOutcome{Status: 503, Code: "recording_unavailable"}); err != nil {
		t.Fatalf("insert over an open row: %v", err)
	}
	if r = readRecording(t, ctx, q, lost.ID); r.state != llmlog.RecordingDone || string(r.body) != "held" || r.code == nil || *r.code != "recording_unavailable" {
		t.Errorf("open row after Insert: state %q body %q code %v — want done, its own request kept", r.state, r.body, r.code)
	}
	// ... and an Insert over a done row changes nothing.
	if err := st.Insert(ctx, other, llmlog.RecordedOutcome{Status: 200}); err != nil {
		t.Fatalf("insert over a done row: %v", err)
	}
	if r = readRecording(t, ctx, q, lost.ID); r.status == nil || *r.status != 503 {
		t.Errorf("a done row's outcome changed: %v", r.status)
	}
}

func TestRecordingsJanitor(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	q := pool
	st := llmlog.Recordings{Pool: pool}
	old, fresh := recID(t), recID(t)
	for _, id := range []string{old, fresh} {
		if err := st.Open(ctx, llmlog.RecordedRequest{ID: id, ConsumerID: gwiConsumer, Path: "/v1/embeddings", Body: []byte("input"), Complete: true}); err != nil {
			t.Fatalf("open: %v", err)
		}
		if err := st.Finish(ctx, id, llmlog.RecordedOutcome{Status: 200, Response: []byte("answer")}); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}
	// The time shift is the fixture's only hand edit: rows are written by Open.
	if _, err := pool.Exec(ctx, `UPDATE context_llm_recordings SET created_at = now() - interval '91 days' WHERE id = $1::uuid`, old); err != nil {
		t.Fatal(err)
	}
	n, err := llmlog.EvictBodies(ctx, pool, 90)
	if err != nil || n != 1 {
		t.Fatalf("evict: %d rows, %v — want the one recording past the retention", n, err)
	}
	if r := readRecording(t, ctx, q, old); r.body != nil || r.response != nil || r.reqBytes != 5 || r.respBytes != 6 || r.state != llmlog.RecordingDone {
		t.Errorf("past the retention: body %v answer %v sizes %d/%d state %q — want both NULL, sizes and outcome kept", r.body, r.response, r.reqBytes, r.respBytes, r.state)
	}
	if r := readRecording(t, ctx, q, fresh); string(r.body) != "input" || string(r.response) != "answer" {
		t.Errorf("the janitor touched a fresh recording")
	}
	if n, _ := llmlog.EvictBodies(ctx, pool, 90); n != 0 {
		t.Errorf("a second run evicted %d rows, want 0 (idempotent)", n)
	}
}

func TestRecordingStatementCost(t *testing.T) {
	if os.Getenv("CTX_RECORD_PROBE") != "1" {
		t.Skip("opt-in probe (CTX_RECORD_PROBE=1)")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	st := llmlog.Recordings{Pool: pool}
	// Warm the pool's connection and statement cache.
	_ = st.Open(ctx, llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, Path: "/warm", Body: []byte("x"), Complete: true})
	for _, n := range []int{64 << 10, 1 << 20, 4 << 20, 8 << 20} {
		body := bytes.Repeat([]byte("0123456789abcdef"), n/16)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		if err := st.Open(ctx, llmlog.RecordedRequest{ID: recID(t), ConsumerID: gwiConsumer, Path: "/v1/embeddings", Body: body, Complete: true}); err != nil {
			t.Fatalf("open %d: %v", n, err)
		}
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		alloc := after.TotalAlloc - before.TotalAlloc
		t.Logf("open of %d bytes: %v, allocated %d bytes = %.2f × the body", n, took, alloc, float64(alloc)/float64(n))
	}
}

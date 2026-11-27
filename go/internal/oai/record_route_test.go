package oai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// The recording of a recording consumer on the routes (issue #571, record.go):
// the request is held — the store's Open returned — before the first
// backend attempt, or it is not served; a non-streamed answer is held before
// it is written, or the client gets 503 recording_unavailable instead; a
// stream's relayed lines are held when it ends; every request the gateway
// refuses after the door leaves its row with what was read; and a consumer
// without the switch never reaches the store.

// recSecret is a credentials pattern in a prompt: the sensitivity scan raises
// a request carrying it to credentials, where the body policy stores nothing.
const recSecret = "password=hunter2-recorded-571"

// recordingConsumer is the rig's consumer with the switch set to on or off.
func recordingConsumer(on bool) *auth.ConsumerAuth {
	ca := urgentApp()
	ca.RecordBodies = on
	return ca
}

// answerIdentity drops what differs between two requests by construction:
// the answer's id and created stamp.
var answerIdentity = regexp.MustCompile(`"(id|created)":("[^"]*"|\d+)`)

func sameAnswer(a, b []byte) bool {
	return answerIdentity.ReplaceAllString(string(a), "") == answerIdentity.ReplaceAllString(string(b), "")
}

// recRow is one row of the fake store.
type recRow struct {
	req        llmlog.RecordedRequest
	body       []byte // a copy of req.Body at the write
	state      string
	out        llmlog.RecordedOutcome
	response   []byte // a copy of out.Response
	hitsAtOpen int64
	opened     bool
}

// recStore is a RecordingStore in memory: it notes every call, the upstream
// hits at the open, and can refuse or hang any of the three statements.
type recStore struct {
	mu    sync.Mutex
	rows  map[string]*recRow
	order []string
	calls atomic.Int64
	hits  func() int64

	openErr, finishErr, insertErr error
	openHang                      bool
}

func newRecStore(hits func() int64) *recStore {
	return &recStore{rows: map[string]*recRow{}, hits: hits}
}

func (s *recStore) Open(ctx context.Context, q llmlog.RecordedRequest) error {
	s.calls.Add(1)
	s.mu.Lock()
	hang, err := s.openHang, s.openErr
	s.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var hits int64
	if s.hits != nil {
		hits = s.hits()
	}
	s.rows[q.ID] = &recRow{req: q, body: append([]byte(nil), q.Body...), state: llmlog.RecordingOpen, hitsAtOpen: hits, opened: true}
	s.order = append(s.order, "open")
	return nil
}

func (s *recStore) Finish(_ context.Context, id string, o llmlog.RecordedOutcome) error {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finishErr != nil {
		return s.finishErr
	}
	r := s.rows[id]
	if r == nil || r.state != llmlog.RecordingOpen {
		return errors.New("no open row")
	}
	r.state, r.out, r.response = llmlog.RecordingDone, o, append([]byte(nil), o.Response...)
	s.order = append(s.order, "finish")
	return nil
}

func (s *recStore) Insert(_ context.Context, q llmlog.RecordedRequest, o llmlog.RecordedOutcome) error {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.insertErr != nil {
		return s.insertErr
	}
	s.rows[q.ID] = &recRow{req: q, body: append([]byte(nil), q.Body...), state: llmlog.RecordingDone, out: o, response: append([]byte(nil), o.Response...)}
	s.order = append(s.order, "insert")
	return nil
}

func (s *recStore) set(f func(s *recStore)) {
	s.mu.Lock()
	f(s)
	s.mu.Unlock()
}

// only is the one row of the store.
func (s *recStore) only(t *testing.T) *recRow {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rows) != 1 {
		t.Fatalf("%d recording rows, want 1 (calls %v)", len(s.rows), s.order)
	}
	for _, r := range s.rows {
		return r
	}
	return nil
}

// withRecordings mounts the recording stage over store (its wait for room is
// the rig's MaxWait).
func withRecordings(store oai.RecordingStore) rigOption {
	return func(r *chatRig) {
		r.recStage = oai.NewRecordingStage(store, r.deps.Consumer, func() oai.Settings { return *r.settings.Load() },
			func() time.Duration { return r.deps.MaxWait() })
	}
}

// statsOf is the consumer's counts in this process.
func statsOf(id string) oai.RecordingConsumer {
	for _, c := range oai.RecordingStats().Consumers {
		if c.ConsumerID == id {
			return c
		}
	}
	return oai.RecordingConsumer{ConsumerID: id}
}

func checkRecordingRefusal(t *testing.T, status int, header http.Header, body []byte) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	if status != http.StatusServiceUnavailable || env.Error.Code != oai.CodeRecordingUnavailable ||
		header.Get("Retry-After") != "1" || header.Get("X-Should-Retry") != "true" {
		t.Errorf("answer %d %q (Retry-After %q, x-should-retry %q), want 503 recording_unavailable, Retry-After 1, x-should-retry true: %.200s",
			status, env.Error.Code, header.Get("Retry-After"), header.Get("X-Should-Retry"), body)
	}
}

const recChatAnswer = `{"id":"gen-1","object":"chat.completion","created":1700000000,"model":"M",` +
	`"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"the answer"}}],` +
	`"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`

// TestRecordingHoldsTheRequestBeforeAnyBackend: for every route, the
// recording consumer's request is in the store — the exact bytes sent, a
// credentials pattern past 64 KiB included — before the backend saw it; the
// row ends done with the answer the client got (embeddings: none); the
// answer, the upstream body and the llmlog row's bodies are those of the
// same request without the switch; the llmlog row names the recording.
func TestRecordingHoldsTheRequestBeforeAnyBackend(t *testing.T) {
	long := strings.Repeat("abcdefgh", 12<<10) // 96 KiB, past the 64 KiB cap
	t.Run("chat", func(t *testing.T) {
		u := newUpstream(t, recChatAnswer)
		store := newRecStore(func() int64 { return u.hits.Load() })
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
		rig.setSettings(func(s *oai.Settings) { s.BodyLogging = llmlog.BodyLoggingFull })
		body := `{"model":"pub","messages":[{"role":"user","content":"` + recSecret + " " + long + `"}]}`
		rig.ca.Store(recordingConsumer(false))
		off := rig.post(t, body)
		if store.calls.Load() != 0 {
			t.Fatalf("a consumer without the switch reached the store (%d calls)", store.calls.Load())
		}
		rig.ca.Store(recordingConsumer(true))
		on := rig.post(t, body)
		if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
			t.Fatalf("status off %d on %d, answers equal %v", off.status, on.status, sameAnswer(off.body, on.body))
		}
		if len(u.raws) != 2 || string(u.raws[0]) != string(u.raws[1]) {
			t.Errorf("the upstream bodies differ with the switch (%d calls)", len(u.raws))
		}
		r := store.only(t)
		if !r.opened || r.hitsAtOpen != 1 { // the first hit was the unrecorded request
			t.Errorf("open %v with %d upstream hits, want the open before the recorded request's backend call (1: the earlier unrecorded one)", r.opened, r.hitsAtOpen)
		}
		if string(r.body) != body || !r.req.Complete || r.req.Op != llmlog.GatewayOpChat || r.req.Model != "pub" || r.req.Stream == nil || *r.req.Stream {
			t.Errorf("held request: %d bytes (want %d, exact), complete %v, op %q, model %q, stream %v", len(r.body), len(body), r.req.Complete, r.req.Op, r.req.Model, r.req.Stream)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || string(r.response) != string(on.body) {
			t.Errorf("row: state %q status %d answer %d bytes, want done 200 with the client's %d bytes", r.state, r.out.Status, len(r.response), len(on.body))
		}
		if !strings.Contains(string(on.body), `"id":"chatcmpl-`+r.req.ID+`"`) {
			t.Errorf("the answer's id does not name the recording %s: %.120s", r.req.ID, on.body)
		}
		rows := rig.rows.all()
		if len(rows) != 2 || rows[0].Recorded || !rows[1].Recorded || rows[1].RecordingID != r.req.ID {
			t.Fatalf("llmlog rows %d: recorded %v/%v, recording id %q (want %q)", len(rows), rows[0].Recorded, rows[1].Recorded, rows[1].RecordingID, r.req.ID)
		}
		eo, en := llmlog.GatewayEntry(rows[0]), llmlog.GatewayEntry(rows[1])
		if eo.RequestUser != en.RequestUser || eo.ResponseContent != en.ResponseContent || en.RequiredSensitivity != eo.RequiredSensitivity {
			t.Errorf("the llmlog row's bodies or class differ with the switch: %d/%d vs %d/%d bytes, %q vs %q",
				len(eo.RequestUser), len(eo.ResponseContent), len(en.RequestUser), len(en.ResponseContent), eo.RequiredSensitivity, en.RequiredSensitivity)
		}
	})
	t.Run("stream", func(t *testing.T) {
		sh := streamShapes[0]
		u := newUpstream(t, "")
		u.setRespond(sseRespond(sh.steps(), nil, nil))
		store := newRecStore(func() int64 { return u.hits.Load() })
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
		srv := serveHTTP(t, rig.router)
		rig.ca.Store(recordingConsumer(false))
		off := postStream(context.Background(), t, srv, streamBody("chat"))
		rig.ca.Store(recordingConsumer(true))
		on := postStream(context.Background(), t, srv, streamBody("chat"))
		checkRelay(t, on)
		if !sameAnswer([]byte(off.raw), []byte(on.raw)) {
			t.Errorf("the stream differs with the switch:\n off %q\n on  %q", off.raw, on.raw)
		}
		r := store.only(t)
		if r.hitsAtOpen != 1 || r.req.Stream == nil || !*r.req.Stream || string(r.body) != streamBody("chat") {
			t.Errorf("open with %d upstream hits (want 1), stream %v, request %q", r.hitsAtOpen, r.req.Stream, r.body)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || string(r.response)+"data: [DONE]\n\n" != on.raw {
			t.Errorf("row: state %q status %d answer %q, want done 200 with the client's lines before [DONE]: %q", r.state, r.out.Status, r.response, on.raw)
		}
	})
	t.Run("embeddings", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		store := newRecStore(func() int64 { return e.hits.Load() })
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withRecordings(store))
		body := embedBody("emb1", []string{recSecret + " " + long, "second"})
		rig.ca.Store(recordingConsumer(false))
		off := rig.postEmbed(t, body)
		offHits := e.hits.Load()
		rig.ca.Store(recordingConsumer(true))
		on := rig.postEmbed(t, body)
		if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
			t.Fatalf("status off %d on %d, answers equal %v: %.200s", off.status, on.status, sameAnswer(off.body, on.body), on.body)
		}
		r := store.only(t)
		if r.hitsAtOpen != offHits || e.hits.Load() != 2*offHits || string(r.body) != body || r.req.Op != llmlog.GatewayOpEmbed {
			t.Errorf("open with %d engine hits (want %d, the unrecorded request's), request %d bytes (want %d), op %q", r.hitsAtOpen, offHits, len(r.body), len(body), r.req.Op)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || r.response != nil {
			t.Errorf("row: state %q status %d answer %d bytes, want done 200 without the vectors", r.state, r.out.Status, len(r.response))
		}
	})
	t.Run("rerank", func(t *testing.T) {
		e := newRerankEngine(t)
		store := newRecStore(func() int64 { return e.hits.Load() })
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
		body := rerankBody("rrpub", "which document?", []string{recSecret + " " + long, "other"})
		rig.ca.Store(recordingConsumer(false))
		off := rig.postRerank(t, "/v1/rerank", body)
		rig.ca.Store(recordingConsumer(true))
		on := rig.postRerank(t, "/v1/rerank", body)
		if off.status != 200 || on.status != 200 || !sameAnswer(off.body, on.body) {
			t.Fatalf("status off %d on %d, answers equal %v: %.200s", off.status, on.status, sameAnswer(off.body, on.body), on.body)
		}
		r := store.only(t)
		if r.hitsAtOpen != 1 || string(r.body) != body || r.req.Op != llmlog.GatewayOpRerank {
			t.Errorf("open with %d engine hits (want 1), request %d bytes (want %d), op %q", r.hitsAtOpen, len(r.body), len(body), r.req.Op)
		}
		if r.state != llmlog.RecordingDone || r.out.Status != 200 || string(r.response) != string(on.body) {
			t.Errorf("row: state %q status %d answer %q, want done 200 with the client's answer %q", r.state, r.out.Status, r.response, on.body)
		}
	})
}

// TestRecordingUnheldIsNotServed: a request that cannot be held — the store
// refuses, hangs past recordWriteTimeout, or the account has no room within
// the wait — gets 503 recording_unavailable and never reaches a backend; its
// row is written at its end if the store takes it then.
func TestRecordingUnheldIsNotServed(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(s *recStore) (undo func())
	}{
		{"store_refuses", func(s *recStore) func() {
			s.set(func(s *recStore) { s.openErr = errors.New("database down") })
			return func() {}
		}},
		{"store_hangs", func(s *recStore) func() {
			s.set(func(s *recStore) { s.openHang = true })
			return func() {}
		}},
		{"account_full", func(*recStore) func() {
			return oai.FillRecordingAccount(oai.RecordingAccountLimit(chatSettings()))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "store_hangs" && testing.Short() {
				t.Skip("waits recordWriteTimeout (5 s)")
			}
			u := newUpstream(t, recChatAnswer)
			store := newRecStore(func() int64 { return u.hits.Load() })
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
				withDeps(func(d *oai.Deps) { d.MaxWait = func() time.Duration { return 50 * time.Millisecond } }), withRecordings(store))
			rig.ca.Store(recordingConsumer(true))
			before := statsOf(urgentApp().ConsumerID)
			undo := tc.prep(store)
			a := rig.post(t, chatBody("pub"))
			undo()
			checkRecordingRefusal(t, a.status, a.header, a.body)
			if n := u.hits.Load(); n != 0 {
				t.Errorf("the backend was called %d times for a request that was not held", n)
			}
			if rig.limiter.aborts.Load() != 1 {
				t.Errorf("the meter's reservation: %d aborts, want 1 (no wire contact)", rig.limiter.aborts.Load())
			}
			got := statsOf(urgentApp().ConsumerID)
			if tc.name == "account_full" {
				// The account stayed full through the request's end: no row
				// could be written — counted, the request was refused.
				if store.calls.Load() != 0 || got.Unrecordable-before.Unrecordable != 1 || got.Recorded != before.Recorded {
					t.Errorf("full account: %d store calls, unrecordable +%d, recorded +%d — want 0, +1, +0",
						store.calls.Load(), got.Unrecordable-before.Unrecordable, got.Recorded-before.Recorded)
				}
				return
			}
			// The store takes the row at the request's end: refused, with its body.
			r := store.only(t)
			if r.opened || r.out.Status != 503 || r.out.Code != oai.CodeRecordingUnavailable || string(r.body) != chatBody("pub") {
				t.Errorf("row at the end: opened %v status %d code %q body %q", r.opened, r.out.Status, r.out.Code, r.body)
			}
			if got.Recorded-before.Recorded != 1 || got.Unrecordable != before.Unrecordable {
				t.Errorf("recorded +%d unrecordable +%d, want +1 +0", got.Recorded-before.Recorded, got.Unrecordable-before.Unrecordable)
			}
		})
	}
}

// TestRecordedAnswerHeldBeforeItIsWritten: a non-streamed answer the store
// cannot take is not written — the client gets 503 recording_unavailable,
// never the answer — on chat, embeddings and rerank; the row stays open and
// counts as unfinished. A stream cannot take its lines back: it ends as it
// would, its row stays open and counts as a stream answer not recorded.
func TestRecordedAnswerHeldBeforeItIsWritten(t *testing.T) {
	failFinish := func(s *recStore) { s.finishErr = errors.New("database down") }
	check := func(t *testing.T, store *recStore, status int, header http.Header, body []byte, before oai.RecordingConsumer) {
		t.Helper()
		checkRecordingRefusal(t, status, header, body)
		if strings.Contains(string(body), "the answer") || strings.Contains(string(body), "relevance_score") || strings.Contains(string(body), `"embedding"`) {
			t.Errorf("the client got the answer that was not held: %.200s", body)
		}
		r := store.only(t)
		if r.state != llmlog.RecordingOpen {
			t.Errorf("row state %q, want open (its outcome could not be held)", r.state)
		}
		got := statsOf(urgentApp().ConsumerID)
		if got.Unfinished-before.Unfinished != 1 || got.Recorded-before.Recorded != 1 {
			t.Errorf("unfinished +%d recorded +%d, want +1 +1", got.Unfinished-before.Unfinished, got.Recorded-before.Recorded)
		}
	}
	t.Run("chat", func(t *testing.T) {
		u := newUpstream(t, recChatAnswer)
		store := newRecStore(nil)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		a := rig.post(t, chatBody("pub"))
		check(t, store, a.status, a.header, a.body, before)
	})
	t.Run("embeddings", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		store := newRecStore(nil)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 4, 1, withRecordings(store))
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		a := rig.postEmbed(t, embedBody("emb1", []string{"a"}))
		check(t, store, a.status, a.header, a.body, before)
	})
	t.Run("rerank", func(t *testing.T) {
		e := newRerankEngine(t)
		store := newRecStore(nil)
		rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(store))
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		a := rig.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
		check(t, store, a.status, a.header, a.body, before)
	})
	t.Run("stream", func(t *testing.T) {
		sh := streamShapes[0]
		u := newUpstream(t, "")
		u.setRespond(sseRespond(sh.steps(), nil, nil))
		store := newRecStore(nil)
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
		srv := serveHTTP(t, rig.router)
		rig.ca.Store(recordingConsumer(true))
		store.set(failFinish)
		before := statsOf(urgentApp().ConsumerID)
		on := postStream(context.Background(), t, srv, streamBody("chat"))
		checkRelay(t, on)
		r := store.only(t)
		got := statsOf(urgentApp().ConsumerID)
		if r.state != llmlog.RecordingOpen || got.StreamAnswersUnrecorded-before.StreamAnswersUnrecorded != 1 || got.Unfinished-before.Unfinished != 1 {
			t.Errorf("row %q, stream answers unrecorded +%d, unfinished +%d — want open, +1, +1",
				r.state, got.StreamAnswersUnrecorded-before.StreamAnswersUnrecorded, got.Unfinished-before.Unfinished)
		}
	})
}

// TestRecordingStreamWithoutRoomEnds: a stream whose next line the account
// cannot take ends before that line with recording_unavailable — every line
// the client got is a line the recording kept.
func TestRecordingStreamWithoutRoomEnds(t *testing.T) {
	sh := streamShapes[0]
	u := newUpstream(t, "")
	u.setRespond(sseRespond(sh.steps(), nil, nil))
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
	srv := serveHTTP(t, rig.router)
	rig.ca.Store(recordingConsumer(true))
	// The open's copies fit; the stream's first line does not.
	limit := oai.RecordingAccountLimit(chatSettings())
	release := oai.FillRecordingAccount(limit - int64(oai.RecordCopyFactor*len(streamBody("chat"))))
	on := postStream(context.Background(), t, srv, streamBody("chat"))
	release()
	r := store.only(t)
	if !strings.Contains(on.raw, oai.CodeRecordingUnavailable) {
		t.Errorf("the stream did not end with recording_unavailable: %d %q", on.status, on.raw)
	}
	if strings.Contains(on.raw, `"delta"`) || len(r.response) != 0 {
		t.Errorf("a line went out that the recording did not keep: client %q, kept %q", on.raw, r.response)
	}
}

// TestRecordingRefusedRequestsLeaveTheirRow: a request the gateway refuses
// after the door — a body that does not parse, a body cut short, an unknown
// model, the meter's refusal — leaves one row, never opened, with what was
// read of the body and the status and code the client got.
func TestRecordingRefusedRequestsLeaveTheirRow(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		declared   int64
		limiter    bool
		status     int
		code       string
		complete   bool
	}{
		{"unparsable", `{"model":"pub","messages":[{"role":"user","content":"` + recSecret + `"`, -1, false, 400, "invalid_json", true},
		{"cut_short", `{"model":"pub","messages":`, 400, false, 400, "invalid_json", false},
		{"unknown_model", `{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`, -1, false, 404, "model_not_found", true},
		{"limiter", chatBody("pub"), -1, true, 429, "rate_limit_exceeded", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, recChatAnswer)
			store := newRecStore(nil)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
			rig.ca.Store(recordingConsumer(true))
			if tc.limiter {
				rig.limiter.refuse = &oai.LimitRefusal{Code: oai.CodeRateLimitExceeded, Message: "slow down", ShouldRetry: true, RetryAfter: time.Second}
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			if tc.declared >= 0 {
				req.ContentLength = tc.declared
			}
			w := httptest.NewRecorder()
			rig.router.ServeHTTP(w, req)
			r := store.only(t)
			if w.Code != tc.status || r.opened || r.out.Status != tc.status || r.out.Code != tc.code {
				t.Errorf("client %d, row opened %v status %d code %q — want %d %q, never opened", w.Code, r.opened, r.out.Status, r.out.Code, tc.status, tc.code)
			}
			if string(r.body) != tc.body || r.req.Complete != tc.complete {
				t.Errorf("row body %q complete %v, want %q complete %v", r.body, r.req.Complete, tc.body, tc.complete)
			}
			if u.hits.Load() != 0 {
				t.Errorf("a refused request reached the backend")
			}
		})
	}
}

// TestRecordingStagePassesOtherConsumers: for a consumer without the switch
// the stage hands the route the very writer and request it got — no
// wrapper, no context value, no store call; for a recording consumer it does
// not.
func TestRecordingStagePassesOtherConsumers(t *testing.T) {
	store := newRecStore(nil)
	var ca atomic.Pointer[auth.ConsumerAuth]
	stage := oai.NewRecordingStage(store, func(context.Context) *auth.ConsumerAuth { return ca.Load() },
		func() oai.Settings { return chatSettings() }, func() time.Duration { return time.Second })
	var sawW http.ResponseWriter
	var sawR *http.Request
	h := stage.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sawW, sawR = w, r }))
	for _, on := range []bool{false, true} {
		ca.Store(recordingConsumer(on))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/models", nil)
		h.ServeHTTP(w, req)
		same := sawW == http.ResponseWriter(w) && sawR == req
		if same == on {
			t.Errorf("switch %v: the route got the stage's own writer and request %v, want %v", on, same, !on)
		}
	}
	if n := store.calls.Load(); n != 1 {
		t.Errorf("%d store calls, want 1 (the recording consumer's row, none for the other)", n)
	}
}

// TestRecordingPanicLeavesTheRow: a request that panics behind the stage
// leaves its row with the 503 internal_error oai.Recover answers.
func TestRecordingPanicLeavesTheRow(t *testing.T) {
	store := newRecStore(nil)
	stage := oai.NewRecordingStage(store, func(context.Context) *auth.ConsumerAuth { return recordingConsumer(true) },
		func() oai.Settings { return chatSettings() }, func() time.Duration { return time.Second })
	rt := chi.NewRouter()
	rt.Use(oai.Recover, stage.Handler)
	rt.Post("/v1/chat/completions", func(http.ResponseWriter, *http.Request) { panic("boom") })
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	r := store.only(t)
	if w.Code != 503 || r.out.Status != 503 || r.out.Code != oai.CodeInternalError {
		t.Errorf("client %d, row status %d code %q — want 503 internal_error both", w.Code, r.out.Status, r.out.Code)
	}
}

// TestRecordingCountsBalance: whatever happened to the requests, a recording
// consumer's counts hold admitted = in_flight + recorded + unrecordable, and
// recorded is the number of rows the store holds for it.
func TestRecordingCountsBalance(t *testing.T) {
	u := newUpstream(t, recChatAnswer)
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withRecordings(store))
	ca := recordingConsumer(true)
	ca.ConsumerID = "0195a6f0-0000-7000-8000-0000000c0571"
	rig.ca.Store(ca)
	held := oai.RecordingHeld()
	rig.post(t, chatBody("pub"))  // served
	rig.post(t, `{"model":"pub"`) // refused before the walk
	store.set(func(s *recStore) { s.openErr = errors.New("down"); s.insertErr = errors.New("down") })
	rig.post(t, chatBody("pub")) // not held, not served, no row at all
	store.set(func(s *recStore) { s.openErr, s.insertErr, s.finishErr = nil, nil, errors.New("down") })
	rig.post(t, chatBody("pub")) // held, its answer not: refused, the row stays open
	got := statsOf(ca.ConsumerID)
	store.mu.Lock()
	rows := int64(len(store.rows))
	store.mu.Unlock()
	if got.Admitted != 4 || got.InFlight != 0 || got.Recorded != 3 || got.Unrecordable != 1 || got.Unfinished != 1 ||
		got.Admitted != got.InFlight+got.Recorded+got.Unrecordable || got.Recorded != rows {
		t.Errorf("counts %+v with %d rows — want admitted 4 = in flight 0 + recorded 3 + unrecordable 1, unfinished 1, recorded = rows", got, rows)
	}
	if after := oai.RecordingHeld(); after != held {
		t.Errorf("the recording account holds %d bytes after the requests ended, %d before — a request kept what it took", after, held)
	}
}

// TestRecordingAccountGivesBack: a stream's and a rerank answer's captures
// go back to the account when their requests end — served or not.
func TestRecordingAccountGivesBack(t *testing.T) {
	held := oai.RecordingHeld()
	sh := streamShapes[0]
	u := newUpstream(t, "")
	u.setRespond(sseRespond(sh.steps(), nil, nil))
	store := newRecStore(nil)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withRecordings(store))
	srv := serveHTTP(t, rig.router)
	rig.ca.Store(recordingConsumer(true))
	_ = postStream(context.Background(), t, srv, streamBody("chat"))
	store.set(func(s *recStore) { s.finishErr = errors.New("down") })
	_ = postStream(context.Background(), t, srv, streamBody("chat"))
	e := newRerankEngine(t)
	rstore := newRecStore(nil)
	rr := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withRecordings(rstore))
	rr.ca.Store(recordingConsumer(true))
	_ = rr.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
	rstore.set(func(s *recStore) { s.finishErr = errors.New("down") })
	_ = rr.postRerank(t, "/v1/rerank", rerankBody("rrpub", "q", []string{"d1", "d2"}))
	if after := oai.RecordingHeld(); after != held {
		t.Errorf("the recording account holds %d bytes after the requests ended, %d before", after, held)
	}
}

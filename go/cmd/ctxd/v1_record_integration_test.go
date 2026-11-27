//go:build integration

// Issue #571 over the PRODUCTION router (buildRouter, the v1ChatFixture of
// v1_chat_integration_test.go) and a real database: a consumer whose
// recording switch a server-admin set through /api/manage has every request
// held in context_llm_recordings by the recording stage behind the door —
//
//   - before the backend: while the recordings table refuses inserts (a
//     lock), the request waits and the engine sees nothing; the engine
//     holding the request finds its row committed, open, byte for byte —
//     what a process killed at that moment leaves;
//
//   - not held, not served: an insert the database refuses, or one that
//     hangs past the statement deadline, is 503 recording_unavailable and
//     the engine is never called; an answer the database does not take is
//     503 instead of the answer, the row stays open;
//
//   - refused after the door: an unparsable body, an unknown model, a body
//     over the cap (never read: its declared size), an unknown path, a wrong
//     method — each leaves one row with its status and code;
//
//   - the consumer without the switch: no recording row, the same answer,
//     the llmlog row's bodies of the same request;
//
//   - GET /api/status: gateway.recording for a server-admin — admitted =
//     in_flight + recorded + unrecordable, recorded = the process's rows —,
//     none for a tenant-admin; switching off applies from the next request;
//     model-alias-preview answers the same with and without the switch.
//
//     go test -tags=integration -p 1 -count=1 -run 'TestV1RecordedConsumer' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

// v1RecSecret is a credentials pattern (the sensitivity scan's assignment
// rule) — the kind of text a recording can carry in clear.
const v1RecSecret = "password=Tr0ub4dor-recorded-571"

// v1RecAPI calls the router's /api surface with a knowledge key.
func v1RecAPI(t *testing.T, f *v1ChatFixture, method, path, key string, body any) (int, []byte, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, rec.Body.Bytes(), out
}

// v1RecDo sends one /v1 request with key over TLS (prep edits it).
func v1RecDo(f *v1ChatFixture, key, method, path, body string, prep ...func(*http.Request)) v1Answer {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.1:40000"
	req.TLS = &tls.ConnectionState{}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for _, p := range prep {
		p(req)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return v1Answer{status: rec.Code, header: rec.Header().Clone(), body: rec.Body.Bytes()}
}

// v1Recording is one row of context_llm_recordings.
type v1Recording struct {
	id, state, path, process string
	op, code                 *string
	status                   *int16
	declared                 *int64
	body, response           []byte
}

func v1RecRows(t *testing.T, f *v1ChatFixture, consumerID string) []v1Recording {
	t.Helper()
	rs, err := f.seed.Query(context.Background(), `SELECT id::text, state, path, process_id::text, op, code, status, declared_bytes, request_body, response_body
		  FROM context_llm_recordings WHERE consumer_id = $1::uuid ORDER BY created_at, id`, consumerID)
	if err != nil {
		t.Fatalf("read recordings: %v", err)
	}
	defer rs.Close()
	var out []v1Recording
	for rs.Next() {
		var r v1Recording
		if err := rs.Scan(&r.id, &r.state, &r.path, &r.process, &r.op, &r.code, &r.status, &r.declared, &r.body, &r.response); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func v1RecLast(t *testing.T, f *v1ChatFixture, consumerID string) v1Recording {
	t.Helper()
	rows := v1RecRows(t, f, consumerID)
	if len(rows) == 0 {
		t.Fatalf("no recording of %s", consumerID)
	}
	return rows[len(rows)-1]
}

func v1RecCode(a v1Answer) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.body, &env)
	return env.Error.Code
}

func deref16(p *int16) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

func derefS(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

var v1RecIdentity = regexp.MustCompile(`"(id|created)":("[^"]*"|\d+)`)

func TestV1RecordedConsumer(t *testing.T) {
	e := newV1ChatEngine(t, http.StatusOK)
	f := newV1ChatFixture(t, []backends.Backend{e.row("eng", 10)})
	ctx := context.Background()
	k, adminKey, err := store.CreateApiKey(ctx, f.seed, "v1-rec-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	if _, err := f.seed.Exec(ctx, `UPDATE context_api_keys SET is_admin = true, tenant_role = 'owner' WHERE id = $1::uuid`, k.ID); err != nil {
		t.Fatalf("promote admin key: %v", err)
	}
	status, body, resp := v1RecAPI(t, f, http.MethodPost, "/api/manage", adminKey, map[string]any{
		"action": "tenant-create", "data": map[string]any{"slug": "v1rec", "display_name": "v1 rec"}})
	tenantAdmin, _ := resp["owner_key"].(string)
	tn, _ := resp["tenant"].(map[string]any)
	tenantID, _ := tn["id"].(string)
	if status != http.StatusOK || tenantAdmin == "" || tenantID == "" {
		t.Fatalf("tenant-create: %d %s", status, body)
	}
	mkConsumer := func(label string, extra map[string]any) (id, key string) {
		t.Helper()
		data := map[string]any{"label": label, "tenant_id": tenantID, "all_models": true}
		for k, v := range extra {
			data[k] = v
		}
		status, body, resp := v1RecAPI(t, f, http.MethodPost, "/api/manage", adminKey, map[string]any{"action": "model-consumer-create", "data": data})
		c, _ := resp["consumer"].(map[string]any)
		id, _ = c["id"].(string)
		key, _ = resp["api_key"].(string)
		if status != http.StatusOK || id == "" || key == "" {
			t.Fatalf("model-consumer-create %s: %d %s", label, status, body)
		}
		return id, key
	}
	recID, recKey := mkConsumer("v1-rec-on", map[string]any{"record_bodies": true, "confirm_record_bodies": true})
	plainID, plainKey := mkConsumer("v1-rec-off", nil)

	content := v1RecSecret + " " + strings.Repeat("0123456789abcdef", 5<<10) // 80 KiB
	chat := `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"` + content + `"}]}`

	t.Run("held_before_the_backend_and_answered", func(t *testing.T) {
		// The recordings table takes no insert while the lock is held: the
		// request waits for its recording, the engine sees nothing.
		tx, err := f.seed.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE context_llm_recordings IN SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		hits := e.hits.Load()
		done := make(chan v1Answer, 1)
		go func() { done <- f.post(t, recKey, chat) }()
		time.Sleep(400 * time.Millisecond)
		waiting := e.hits.Load() - hits
		_ = tx.Commit(ctx)
		on := <-done
		if waiting != 0 {
			t.Errorf("the engine was called %d times while the request's recording could not be committed", waiting)
		}
		off := f.post(t, plainKey, chat)
		if on.status != 200 || off.status != 200 || v1RecIdentity.ReplaceAllString(string(on.body), "") != v1RecIdentity.ReplaceAllString(string(off.body), "") {
			t.Fatalf("status on %d off %d, the same answer %v", on.status, off.status,
				v1RecIdentity.ReplaceAllString(string(on.body), "") == v1RecIdentity.ReplaceAllString(string(off.body), ""))
		}
		r := v1RecLast(t, f, recID)
		if r.state != llmlog.RecordingDone || string(r.body) != chat || string(r.response) != string(on.body) || deref16(r.status) != 200 ||
			r.process != llmlog.ProcessID || derefS(r.op) != llmlog.GatewayOpChat || r.path != "/v1/chat/completions" {
			t.Errorf("recording: state %q, request %d bytes (want %d, exact), answer %d bytes (want the client's %d), status %d, process %v, op %q, path %q",
				r.state, len(r.body), len(chat), len(r.response), len(on.body), deref16(r.status), r.process == llmlog.ProcessID, derefS(r.op), r.path)
		}
		if n := len(v1RecRows(t, f, plainID)); n != 0 {
			t.Errorf("the consumer without the switch has %d recordings, want none", n)
		}
		// Its llmlog row is the row of every consumer without bodies, plus
		// the marker.
		f.settle(t)
		var recUser, recAnswer, sens, recorded, recordingID string
		if err := f.seed.QueryRow(ctx, `SELECT COALESCE(request_user, ''), COALESCE(response_content, ''), COALESCE(required_sensitivity, ''),
			       COALESCE(metadata->'gw'->>'recorded', ''), COALESCE(metadata->'gw'->>'recording_id', '')
			  FROM context_llm_log WHERE consumer_id = $1::uuid ORDER BY created_at DESC LIMIT 1`, recID).
			Scan(&recUser, &recAnswer, &sens, &recorded, &recordingID); err != nil {
			t.Fatalf("llmlog row: %v", err)
		}
		if recorded != "true" || recordingID != r.id || recUser != "" || recAnswer != "" || sens != string(backends.SensCredentials) {
			t.Errorf("llmlog row: recorded %q recording_id %q (want %q), bodies %d/%d bytes (want none), class %q",
				recorded, recordingID, r.id, len(recUser), len(recAnswer), sens)
		}
	})
	t.Run("open_while_the_engine_holds_it", func(t *testing.T) {
		release := e.holdUntilReleased(t)
		done := make(chan v1Answer, 1)
		go func() { done <- f.post(t, recKey, chat) }()
		deadline := time.Now().Add(10 * time.Second)
		for e.hits.Load() == 0 || len(v1RecRows(t, f, recID)) < 2 {
			if time.Now().After(deadline) {
				t.Fatal("the engine never got the request")
			}
			time.Sleep(10 * time.Millisecond)
		}
		// What a process killed now leaves: the request, whole, open.
		r := v1RecLast(t, f, recID)
		release()
		<-done
		e.gate.Store(nil)
		if r.state != llmlog.RecordingOpen || string(r.body) != chat || r.status != nil {
			t.Errorf("while the engine held it: state %q, request exact %v, status %v — want open with the whole request", r.state, string(r.body) == chat, r.status)
		}
	})
	t.Run("insert_refused_not_served", func(t *testing.T) {
		if _, err := f.seed.Exec(ctx, `CREATE FUNCTION zz_rec_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'recordings refused'; END $$;
			CREATE TRIGGER zz_rec_refuse BEFORE INSERT ON context_llm_recordings FOR EACH ROW EXECUTE FUNCTION zz_rec_refuse()`); err != nil {
			t.Fatal(err)
		}
		hits, rows := e.hits.Load(), len(v1RecRows(t, f, recID))
		a := f.post(t, recKey, chat)
		if _, err := f.seed.Exec(ctx, `DROP TRIGGER zz_rec_refuse ON context_llm_recordings; DROP FUNCTION zz_rec_refuse()`); err != nil {
			t.Fatal(err)
		}
		if a.status != 503 || v1RecCode(a) != "recording_unavailable" || a.header.Get("X-Should-Retry") != "true" || a.header.Get("Retry-After") == "" {
			t.Errorf("answer %d %q (x-should-retry %q, Retry-After %q), want 503 recording_unavailable, retryable", a.status, v1RecCode(a),
				a.header.Get("X-Should-Retry"), a.header.Get("Retry-After"))
		}
		if n := e.hits.Load() - hits; n != 0 || len(v1RecRows(t, f, recID)) != rows {
			t.Errorf("engine calls %d, recordings %d → %d — want none of either", n, rows, len(v1RecRows(t, f, recID)))
		}
	})
	t.Run("insert_hangs_not_served", func(t *testing.T) {
		// Over a real server: the client gets its 503 when the hold gives up
		// (the 5 s statement deadline), not after the row's second attempt.
		srv := httptest.NewTLSServer(f.router)
		defer srv.Close()
		tx, err := f.seed.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE context_llm_recordings IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		hits := e.hits.Load()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(chat))
		req.Header.Set("Authorization", "Bearer "+recKey)
		req.Header.Set("Content-Type", "application/json")
		start := time.Now()
		resp, err := srv.Client().Do(req)
		took := time.Since(start)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("request: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		_ = tx.Rollback(ctx)
		a := v1Answer{status: resp.StatusCode, header: resp.Header, body: raw}
		if a.status != 503 || v1RecCode(a) != "recording_unavailable" || e.hits.Load() != hits {
			t.Errorf("answer %d %q after %v, %d engine calls — want 503 recording_unavailable, none", a.status, v1RecCode(a), took, e.hits.Load()-hits)
		}
		if took < 5*time.Second || took > 8*time.Second {
			t.Errorf("the client had its answer after %v — the statement deadline is 5 s", took)
		}
	})
	t.Run("answer_not_held_not_written", func(t *testing.T) {
		if _, err := f.seed.Exec(ctx, `CREATE FUNCTION zz_rec_refuse() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outcome refused'; END $$;
			CREATE TRIGGER zz_rec_refuse BEFORE UPDATE ON context_llm_recordings FOR EACH ROW EXECUTE FUNCTION zz_rec_refuse()`); err != nil {
			t.Fatal(err)
		}
		a := f.post(t, recKey, chat)
		if _, err := f.seed.Exec(ctx, `DROP TRIGGER zz_rec_refuse ON context_llm_recordings; DROP FUNCTION zz_rec_refuse()`); err != nil {
			t.Fatal(err)
		}
		r := v1RecLast(t, f, recID)
		if a.status != 503 || v1RecCode(a) != "recording_unavailable" || strings.Contains(string(a.body), "choices") {
			t.Errorf("answer %d %q, want 503 recording_unavailable and no answer: %.200s", a.status, v1RecCode(a), a.body)
		}
		if r.state != llmlog.RecordingOpen || string(r.body) != chat {
			t.Errorf("recording %q, request exact %v — want it open with the whole request", r.state, string(r.body) == chat)
		}
	})
	t.Run("refused_after_the_door", func(t *testing.T) {
		over := int64(8<<20) + 1 // gateway.max_request_bytes at its default (the fixture's environment sets none)
		for _, tc := range []struct {
			name, method, path, body string
			prep                     func(*http.Request)
			status                   int
			code                     string
			wantBody                 string
			declared                 int64
		}{
			{"unparsable", http.MethodPost, "/v1/chat/completions", `{"model":"` + v1ChatAlias + `","messages":[`, nil, 400, "invalid_json", `{"model":"` + v1ChatAlias + `","messages":[`, -2},
			{"unknown_model", http.MethodPost, "/v1/chat/completions", `{"model":"no-such-571","messages":[{"role":"user","content":"x"}]}`, nil, 404, "model_not_found", `{"model":"no-such-571","messages":[{"role":"user","content":"x"}]}`, -2},
			{"over_the_cap", http.MethodPost, "/v1/chat/completions", "{}", func(r *http.Request) {
				r.ContentLength = over
				r.Header.Set("Content-Length", strconv.FormatInt(over, 10))
			}, 413, "request_too_large", "", over},
			// The catch-alls name no reason on the access record: the row
			// carries their status, no code.
			{"unknown_path", http.MethodPost, "/v1/nowhere", "{}", nil, 404, "", "", -2},
			{"wrong_method", http.MethodGet, "/v1/chat/completions", "", nil, 405, "", "", -2},
		} {
			t.Run(tc.name, func(t *testing.T) {
				before := len(v1RecRows(t, f, recID))
				var prep []func(*http.Request)
				if tc.prep != nil {
					prep = append(prep, tc.prep)
				}
				a := v1RecDo(f, recKey, tc.method, tc.path, tc.body, prep...)
				rows := v1RecRows(t, f, recID)
				if a.status != tc.status || len(rows) != before+1 {
					t.Fatalf("answer %d (want %d), recordings %d → %d (want one more)", a.status, tc.status, before, len(rows))
				}
				r := rows[len(rows)-1]
				if r.state != llmlog.RecordingDone || deref16(r.status) != tc.status || derefS(r.code) != tc.code || string(r.body) != tc.wantBody || r.path != tc.path {
					t.Errorf("row: state %q status %d code %q body %q path %q — want done %d %q, body %q", r.state, deref16(r.status), derefS(r.code), r.body, r.path, tc.status, tc.code, tc.wantBody)
				}
				if tc.declared != -2 && (r.declared == nil || *r.declared != tc.declared) {
					t.Errorf("declared size %v, want %d (the body was never read)", r.declared, tc.declared)
				}
			})
		}
	})
	t.Run("status", func(t *testing.T) {
		st, b, out := v1RecAPI(t, f, http.MethodGet, "/api/status", adminKey, nil)
		gw, _ := out["gateway"].(map[string]any)
		rec, _ := gw["recording"].(map[string]any)
		if st != 200 || rec == nil || rec["process_id"] != llmlog.ProcessID {
			t.Fatalf("server-admin status: %d recording %v — want the section with this process's id: %.300s", st, rec, b)
		}
		var mine map[string]any
		cons, _ := rec["consumers"].([]any)
		for _, c := range cons {
			if m, _ := c.(map[string]any); m["consumer_id"] == recID {
				mine = m
			}
		}
		num := func(k string) int64 { v, _ := mine[k].(float64); return int64(v) }
		var rows int64
		if err := f.seed.QueryRow(ctx, `SELECT count(*) FROM context_llm_recordings WHERE consumer_id = $1::uuid AND process_id = $2::uuid`, recID, llmlog.ProcessID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if mine == nil || num("admitted") != num("in_flight")+num("recorded")+num("unrecordable") || num("recorded") != rows ||
			num("unrecordable") < 1 || num("unfinished") < 1 {
			t.Errorf("counts %v with %d rows of this process — want admitted = in_flight + recorded + unrecordable, recorded = rows, the refused insert and the open row counted", mine, rows)
		}
		_, bt, outT := v1RecAPI(t, f, http.MethodGet, "/api/status", tenantAdmin, nil)
		gwT, _ := outT["gateway"].(map[string]any)
		if _, ok := gwT["recording"]; ok || strings.Contains(string(bt), "Tr0ub4dor") {
			t.Errorf("the tenant-admin's status carries the recording section or a body")
		}
		if strings.Contains(string(b), "Tr0ub4dor") {
			t.Errorf("the server-admin's status carries a body")
		}
	})
	preview := func() string {
		t.Helper()
		st, b, _ := v1RecAPI(t, f, http.MethodPost, "/api/manage", adminKey, map[string]any{
			"action": "model-alias-preview", "data": map[string]any{"consumer_id": recID, "alias": v1ChatAlias}})
		if st != http.StatusOK {
			t.Fatalf("model-alias-preview: %d %s", st, b)
		}
		return string(b)
	}
	previewOn := preview()
	t.Run("switch_off_applies_from_the_next_request", func(t *testing.T) {
		st, b, _ := v1RecAPI(t, f, http.MethodPost, "/api/manage", tenantAdmin, map[string]any{
			"action": "model-consumer-update", "data": map[string]any{"id": recID, "record_bodies": false}})
		if st != http.StatusOK {
			t.Fatalf("tenant-admin switches off: %d %s", st, b)
		}
		if previewOff := preview(); previewOff != previewOn {
			t.Errorf("model-alias-preview differs with the switch:\n on  %s\n off %s", previewOn, previewOff)
		}
		before := len(v1RecRows(t, f, recID))
		if a := f.post(t, recKey, chat); a.status != http.StatusOK {
			t.Fatalf("request after the switch: %d", a.status)
		}
		if n := len(v1RecRows(t, f, recID)); n != before {
			t.Errorf("a request after switching off was recorded (%d → %d rows)", before, n)
		}
	})
	_ = io.EOF
}

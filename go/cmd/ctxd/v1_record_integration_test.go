//go:build integration

// Issue #571 over the PRODUCTION router (buildRouter, the v1ChatFixture of
// v1_chat_integration_test.go): a consumer whose recording switch a
// server-admin set through /api/manage has its /v1 chat row written by the
// production writer (llmlog.RecordGateway → the async insert) with the whole
// request and answer — a prompt past the 64 KiB cap that carries a
// credentials pattern included — marked metadata.gw.recorded; the same
// request of a consumer without the switch gets the same answer and the row
// it always got. Then every reader of the row (TestBodyColumnReadersCensus
// lists them) with that row:
//
//   - GET /api/llmlog/{id}: a server-admin reads the bodies; the tenant-admin
//     of the consumer's tenant gets the uniform 404;
//   - GET /api/llmlog (list): metadata only, for both — the pattern is in
//     neither answer;
//   - GET /api/status: recording_consumers names the consumer — for the
//     server-admin and for its tenant-admin — and no body;
//   - ctx-llmlog-export (llmlogexport.Export): the row leaves sealed;
//   - the retention janitor (llmlog.EvictBodies): the row's bodies go after
//     the retention like every row's;
//   - switching off (the tenant-admin may) applies from the next request, and
//     model-alias-preview answers the same with and without the switch.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestV1RecordedConsumer' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/llmlogexport"
	"github.com/GottZ/ctx/internal/store"
)

// v1RecSecret is a credentials pattern (the sensitivity scan's assignment
// rule) — the kind of text a recorded row can carry in clear.
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

// v1RecRow is the stored row of one consumer's newest gateway-chat call.
type v1RecRow struct {
	id, user, resp, sens string
	recorded             *string
	userNull             bool
}

func v1RecRead(t *testing.T, f *v1ChatFixture, consumerID string) v1RecRow {
	t.Helper()
	f.settle(t)
	var r v1RecRow
	var user, resp *string
	if err := f.seed.QueryRow(context.Background(), `
		SELECT id::text, request_user, response_content, COALESCE(required_sensitivity, ''), metadata->'gw'->>'recorded'
		  FROM context_llm_log WHERE consumer_id = $1::uuid AND pipeline = $2
		 ORDER BY created_at DESC, id DESC LIMIT 1`, consumerID, llmlog.PipelineGatewayChat).
		Scan(&r.id, &user, &resp, &r.sens, &r.recorded); err != nil {
		t.Fatalf("read the row of %s: %v", consumerID, err)
	}
	r.userNull = user == nil
	if user != nil {
		r.user = *user
	}
	if resp != nil {
		r.resp = *resp
	}
	return r
}

var v1RecIdentity = regexp.MustCompile(`"(id|created)":("[^"]*"|\d+)`)

func TestV1RecordedConsumerRowAndItsReaders(t *testing.T) {
	e := newV1ChatEngine(t, http.StatusOK)
	f := newV1ChatFixture(t, []backends.Backend{e.row("eng", 10)})
	ctx := context.Background()
	// The fixture minted the first key already, so the server-admin is a key
	// promoted the way model_inflight_integration_test.go promotes one.
	k, adminKey, err := store.CreateApiKey(ctx, f.seed, "v1-rec-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	if _, err := f.seed.Exec(ctx, `UPDATE context_api_keys SET is_admin = true, tenant_role = 'owner' WHERE id = $1::uuid`, k.ID); err != nil {
		t.Fatalf("promote admin key: %v", err)
	}
	v1RecAdminKey := adminKey
	status, body, resp := v1RecAPI(t, f, http.MethodPost, "/api/manage", v1RecAdminKey, map[string]any{
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
		status, body, resp := v1RecAPI(t, f, http.MethodPost, "/api/manage", v1RecAdminKey, map[string]any{"action": "model-consumer-create", "data": data})
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
	// A recording consumer of another tenant: the tenant-admin's status must
	// not name it, the server-admin's must.
	status, body, resp = v1RecAPI(t, f, http.MethodPost, "/api/manage", v1RecAdminKey, map[string]any{
		"action": "tenant-create", "data": map[string]any{"slug": "v1rec-other", "display_name": "v1 rec other"}})
	otherTn, _ := resp["tenant"].(map[string]any)
	otherTenant, _ := otherTn["id"].(string)
	if status != http.StatusOK || otherTenant == "" {
		t.Fatalf("tenant-create other: %d %s", status, body)
	}
	status, body, resp = v1RecAPI(t, f, http.MethodPost, "/api/manage", v1RecAdminKey, map[string]any{"action": "model-consumer-create",
		"data": map[string]any{"label": "v1-rec-foreign", "tenant_id": otherTenant, "record_bodies": true, "confirm_record_bodies": true}})
	foreign, _ := resp["consumer"].(map[string]any)
	foreignID, _ := foreign["id"].(string)
	if status != http.StatusOK || foreignID == "" {
		t.Fatalf("foreign recording consumer: %d %s", status, body)
	}

	content := v1RecSecret + " " + strings.Repeat("0123456789abcdef", 5<<10) // 80 KiB
	chat := `{"model":"` + v1ChatAlias + `","messages":[{"role":"user","content":"` + content + `"}]}`
	messages := `[{"role":"user","content":"` + content + `"}]`
	onA := f.post(t, recKey, chat)
	offA := f.post(t, plainKey, chat)
	if onA.status != http.StatusOK || offA.status != http.StatusOK {
		t.Fatalf("status on %d off %d, want 200: %s / %s", onA.status, offA.status, onA.body, offA.body)
	}
	if v1RecIdentity.ReplaceAllString(string(onA.body), "") != v1RecIdentity.ReplaceAllString(string(offA.body), "") {
		t.Errorf("the answer differs with the switch:\n on  %s\n off %s", onA.body, offA.body)
	}
	e.mu.Lock()
	sameUpstream := len(e.bodies) == 2 && bytes.Equal(e.bodies[0], e.bodies[1])
	e.mu.Unlock()
	if !sameUpstream {
		t.Errorf("the engine saw different bodies with and without the switch")
	}

	rec := v1RecRead(t, f, recID)
	plain := v1RecRead(t, f, plainID)
	if rec.user != messages || rec.resp != strings.TrimSuffix(string(onA.body), "\n") || rec.recorded == nil || *rec.recorded != "true" {
		t.Fatalf("recorded row: request %d bytes (want %d), answer %d bytes (want %d), recorded %v",
			len(rec.user), len(messages), len(rec.resp), len(onA.body)-1, rec.recorded)
	}
	if rec.sens != string(backends.SensCredentials) || plain.sens != rec.sens {
		t.Errorf("required_sensitivity recorded %q plain %q, want credentials both (the switch changes no class)", rec.sens, plain.sens)
	}
	if plain.user != "" || plain.resp != "" || plain.recorded != nil {
		t.Errorf("the plain consumer's row: %d/%d body bytes, recorded %v — want the sealed row it always got", len(plain.user), len(plain.resp), plain.recorded)
	}

	t.Run("detail", func(t *testing.T) {
		st, b, out := v1RecAPI(t, f, http.MethodGet, "/api/llmlog/"+rec.id, v1RecAdminKey, nil)
		d, _ := out["detail"].(map[string]any)
		if st != http.StatusOK || d["recorded"] != true || !strings.Contains(string(b), "Tr0ub4dor-recorded-571") {
			t.Errorf("server-admin detail: %d recorded %v, secret present %v — want 200, true, the whole bodies", st, d["recorded"], strings.Contains(string(b), "Tr0ub4dor"))
		}
		stT, bT, _ := v1RecAPI(t, f, http.MethodGet, "/api/llmlog/"+rec.id, tenantAdmin, nil)
		stU, bU, _ := v1RecAPI(t, f, http.MethodGet, "/api/llmlog/0195a6f0-0000-7000-8000-000000000571", tenantAdmin, nil)
		if stT != http.StatusNotFound || string(bT) != string(bU) || strings.Contains(string(bT), "Tr0ub4dor") {
			t.Errorf("tenant-admin detail of the recorded row: %d %s — want the uniform 404 of an unknown id (%d %s)", stT, bT, stU, bU)
		}
	})
	t.Run("list", func(t *testing.T) {
		for name, key := range map[string]string{"server_admin": v1RecAdminKey, "tenant_admin": tenantAdmin} {
			st, b, out := v1RecAPI(t, f, http.MethodGet, "/api/llmlog?consumer_id="+recID, key, nil)
			entries, _ := out["entries"].([]any)
			if st != http.StatusOK || len(entries) == 0 || strings.Contains(string(b), "Tr0ub4dor") || strings.Contains(string(b), "0123456789abcdef0123") {
				t.Errorf("%s list: %d, %d entries, a body in the answer %v — want 200, the row, metadata only", name, st, len(entries), strings.Contains(string(b), "Tr0ub4dor"))
			}
		}
	})
	t.Run("status", func(t *testing.T) {
		both := []string{recID, foreignID}
		sort.Strings(both)
		for name, c := range map[string]struct {
			key  string
			want []string
		}{"server_admin": {v1RecAdminKey, both}, "tenant_admin": {tenantAdmin, []string{recID}}} {
			st, b, out := v1RecAPI(t, f, http.MethodGet, "/api/status", c.key, nil)
			gw, _ := out["gateway"].(map[string]any)
			raw, _ := json.Marshal(gw["recording_consumers"])
			want, _ := json.Marshal(c.want)
			if st != http.StatusOK || string(raw) != string(want) || strings.Contains(string(b), "Tr0ub4dor") {
				t.Errorf("%s status: %d recording_consumers %s — want exactly %s, no body", name, st, raw, want)
			}
		}
	})
	t.Run("export", func(t *testing.T) {
		var buf bytes.Buffer
		sum, err := llmlogexport.Export(ctx, f.seed, &buf, llmlogexport.ExportOptions{UntilMargin: 1})
		if err != nil {
			t.Fatalf("export: %v", err)
		}
		// The summary by its wire (the CLI's stderr JSON), so the file
		// compiles against the base.
		var counts map[string]any
		_ = json.Unmarshal(llmlogexport.MarshalSummary(sum), &counts)
		if strings.Contains(buf.String(), "Tr0ub4dor") || counts["rows_recorded_sealed"] != float64(1) {
			t.Errorf("export: secret present %v, rows_recorded_sealed %v — want the recorded row sealed", strings.Contains(buf.String(), "Tr0ub4dor"), counts["rows_recorded_sealed"])
		}
		if !strings.Contains(buf.String(), rec.id) {
			t.Errorf("the export left the recorded row out — it keeps the row, without its bodies")
		}
	})
	preview := func() string {
		t.Helper()
		st, b, _ := v1RecAPI(t, f, http.MethodPost, "/api/manage", v1RecAdminKey, map[string]any{
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
		if a := f.post(t, recKey, chat); a.status != http.StatusOK {
			t.Fatalf("request after the switch: %d", a.status)
		}
		if r := v1RecRead(t, f, recID); r.id == rec.id || r.recorded != nil || r.user != "" {
			t.Errorf("the next row after switching off: recorded %v, %d body bytes — want an unrecorded, sealed row", r.recorded, len(r.user))
		}
	})
	t.Run("retention_janitor", func(t *testing.T) {
		// A hypertable row cannot move to an older chunk, so the recorded row
		// is copied 91 days back (its bodies and metadata as the writer
		// stored them) and the janitor runs over both.
		var old string
		if err := f.seed.QueryRow(ctx, `INSERT INTO context_llm_log
			SELECT (jsonb_populate_record(NULL::context_llm_log, to_jsonb(t) ||
			        jsonb_build_object('id', gen_random_uuid(), 'created_at', now() - interval '91 days'))).*
			  FROM context_llm_log t WHERE t.id = $1::uuid
			RETURNING id::text`, rec.id).Scan(&old); err != nil {
			t.Fatalf("copy the row back: %v", err)
		}
		if _, err := llmlog.EvictBodies(ctx, f.seed, 90); err != nil {
			t.Fatalf("evict: %v", err)
		}
		bodies := func(id string) (user, resp, recorded *string) {
			t.Helper()
			if err := f.seed.QueryRow(ctx, `SELECT request_user, response_content, metadata->'gw'->>'recorded' FROM context_llm_log WHERE id = $1::uuid`, id).
				Scan(&user, &resp, &recorded); err != nil {
				t.Fatalf("read: %v", err)
			}
			return user, resp, recorded
		}
		if u, r, m := bodies(old); u != nil || r != nil || m == nil || *m != "true" {
			t.Errorf("past the retention: request %v answer %v recorded %v — want NULL bodies and the marker kept", u != nil, r != nil, m)
		}
		if u, _, _ := bodies(rec.id); u == nil || *u != messages {
			t.Errorf("the janitor touched the recorded row inside the retention")
		}
	})
}

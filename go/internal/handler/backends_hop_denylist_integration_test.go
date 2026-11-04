//go:build integration

// F-W4 (design/04 §4.3 Härtung, §5.2 S4) — the API write path of the hop
// contract's reserved prefixes: backend-create and backend-update with a
// Ctx-… or X-Ctx-… name in extra_headers answer 422 with a field error on
// extra_headers and write nothing. The psql path is covered on the wire by
// fedhop.Stamp step 1 (chat/wire_hop_seed_integration_test.go). Red before
// the prefixes: 200, and the row carried the header.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestBackendExtraHeadersFederationPrefix -count=1 -v
package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBackendExtraHeadersFederationPrefix_Integration(t *testing.T) {
	ph := setupProfileHarness(t)

	assert422 := func(what, header string, code int, body []byte) {
		t.Helper()
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s with extra_headers %q = %d, want 422; body=%s", what, header, code, body)
			return
		}
		var resp struct {
			Fields []struct{ Field, Message string } `json:"fields"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Errorf("%s: decode 422 body: %v (%s)", what, err, body)
			return
		}
		for _, f := range resp.Fields {
			if f.Field == "extra_headers" && strings.Contains(f.Message, "reserved federation header") && !strings.Contains(f.Message, "credential") {
				return
			}
		}
		t.Errorf("%s: no reserved-federation-header field error on extra_headers: %s", what, body)
	}

	rec := ph.doRaw(ph.admin, map[string]any{"action": "backend-create", "data": map[string]any{
		"name": "hop-forger", "base_url": "http://hop-forger", "roles": []string{"chat"},
		"model_map": map[string]any{"default": "stub-model"}, "extra_headers": map[string]string{"Ctx-Hop": "forged"},
	}})
	assert422("backend-create", "Ctx-Hop", rec.Code, rec.Body.Bytes())
	var n int
	if err := ph.pool.QueryRow(ph.ctx, `SELECT count(*) FROM context_backends WHERE name = 'hop-forger'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("create wrote the row: n=%d err=%v", n, err)
	}

	id := ph.byID["chat-a"]
	rec = ph.doRaw(ph.admin, map[string]any{"action": "backend-update", "id": id, "data": map[string]any{
		"extra_headers": map[string]string{"X-Ctx-Foo": "interactive"},
	}})
	assert422("backend-update", "X-Ctx-Foo", rec.Code, rec.Body.Bytes())
	var stored string
	if err := ph.pool.QueryRow(ph.ctx, `SELECT extra_headers::text FROM context_backends WHERE id = $1`, id).Scan(&stored); err != nil {
		t.Fatalf("read back %s: %v", id, err)
	}
	if strings.Contains(strings.ToLower(stored), "ctx-") {
		t.Errorf("update stored the reserved header: %s", stored)
	}
}

//go:build integration

// Issue #372, part 2: before rerank and synthesis the query annotates every
// candidate with its floor-adjusted sensitivity (annotateSensitivities), and
// both stages choose the backend that receives the candidates' text by it.
// The annotation applied the floor of the caller's configuration only. For a
// read scope another tenant granted, the floor that tenant set on its scope
// (pool.scope_sensitivity_floor is tenant-overridable) did not count, and the
// granted block's text could go to a synthesis backend below it. The fold of
// the embed backfill (backfillFloor, issue #253) now applies here as well.
//
// On the #253 fixture: the foreign tenant floors its scope to personal and
// grants the caller's tenant read on it; the caller has no floor. The embed
// backend takes personal (the backfill embeds the granted block under the
// owner's floor), the synthesis backend takes internal at most. What the
// answer reports is read from the wire.
//
// RED before the fix: the synthesis is served (the block counted as public).
//
//	go test -tags=integration ./internal/handler/ -run TestQueryOwnerFloorReachesSynthesis -count=1 -v
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
)

func TestQueryOwnerFloorReachesSynthesis(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")

	run := func(t *testing.T, ownerFloor bool) map[string]any {
		t.Helper()
		f, rec := bfsSetup(t), ownfNewRecorder(t)
		if ownerFloor {
			ownfSetFloor(t, f, bfsForeignScope, map[string]string{bfsForeignScope: string(backends.SensPersonal)})
		}
		granted := ownfGrantedCaller(t, f)
		f.write(t, f.foreignCtx, bfsForeignTitle)

		base := bw2Config()
		base.EmbedBackfill.SyncCap = 4
		base.EmbedBackfill.Interval = 15 * time.Second
		cs := config.NewStore(base)
		cs.SetOverlay(settings.TenantOverlay(f.pool))
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{
			{ID: "e", Name: "osf-embed", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
				Model: "test-embed", Trust: backends.TrustNoCredentials, Enabled: true, Priority: 1,
				Roles: []string{backends.RoleEmbed}},
			{ID: "t", Name: "osf-translate", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
				Model: "test-chat", Trust: backends.TrustFull, Enabled: true, Priority: 1,
				Roles: []string{backends.RoleTranslate}},
			{ID: "s", Name: "osf-synthesis", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
				Model: "test-chat", Trust: backends.TrustNonPersonal, Enabled: true, Priority: 1,
				NumCtx: 8192, Roles: []string{backends.RoleSynthesis}},
		})
		h := NewQueryHandler(f.pool, ownfConfigStore{Store: cs, request: base}, bpool, nil, f.reg, snapshotTestAdmitter(t))
		raw, _ := json.Marshal(map[string]any{"query": bfsForeignTitle, "synthesize": true, "sensitivity": "public"})
		req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(string(raw))).WithContext(granted)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.HandleQuery(w, req)
		if !f.embedded(t, bfsForeignTitle) {
			t.Fatalf("premise: the granted block has no vector, so the answer proves nothing (status %d, body %s)", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(w.Body.String())), &body); err != nil {
			t.Fatalf("decode %q: %v", w.Body.String(), err)
		}
		return body
	}

	// Control: without the owner's floor the fixture synthesizes over the
	// granted block, so a refusal below is the floor's doing.
	t.Run("control_no_owner_floor", func(t *testing.T) {
		body := run(t, false)
		if body["error_code"] != nil {
			t.Fatalf("without a floor the synthesis was refused: %v", body)
		}
		if srcs, _ := body["sources"].([]any); len(srcs) == 0 {
			t.Fatalf("premise: the answer names no source: %v", body)
		}
	})

	t.Run("owner_floor_on_a_granted_scope", func(t *testing.T) {
		body := run(t, true)
		if body["error_code"] != "no_eligible_backend" || body["required_sensitivity"] != string(backends.SensPersonal) {
			t.Errorf("answer %v: want no_eligible_backend at personal — the granted block's text went to a synthesis backend below its owner's floor", body)
		}
	})
}

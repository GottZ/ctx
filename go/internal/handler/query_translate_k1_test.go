package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/llm"
)

// TestQueryTranslateSetting (#210): query.translate = off skips the
// detect+translate step — no translate-role wire call for a question the
// heuristic matches — while auto, and the empty value of a config built
// without the key, keep today's call. The question is German and carries no
// temporal word, so the translate role sees exactly one call when the step
// runs (the temporal LLM fallback shares that role and must stay out of the
// count). DB-less like the quota probe next door: the broken pool ends the
// request at the search stage, after both wire stages ran.
//
// Falsifying implementation: the gate missing — "off" still makes one call.
func TestQueryTranslateSetting(t *testing.T) {
	drive := func(t *testing.T, mode string) int32 {
		t.Helper()
		var trHits atomic.Int32
		trSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			trHits.Add(1)
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"what is the status of the database"},"eval_count":1,"prompt_eval_count":1}`)
		}))
		t.Cleanup(trSrv.Close)
		embSrv, embHits := fakeEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{
			{
				ID: "e", Name: "test-embed", Host: embSrv.URL, Protocol: backends.ProtocolOllama,
				Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
				Roles: []string{backends.RoleEmbed},
			},
			{
				ID: "t", Name: "test-translate", Host: trSrv.URL, Protocol: backends.ProtocolOllama,
				Model: "test-translate", Trust: backends.TrustFull, Enabled: true, Priority: 1,
				Roles: []string{backends.RoleTranslate},
			},
		})
		st := &countingStore{}
		cfg := snapshotTestConfig()
		cfg.Query.Translate = mode
		st.cfg.Store(cfg)
		h := NewQueryHandler(brokenPool(t), st, bpool, nil, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"query":"wie ist der status der datenbank"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
			ApiKeyID: "019e0011-aaaa-7000-9000-000000000001", HomeScope: "free:work", ReadScopes: []string{"free:work"}, IsValid: true,
		}))
		h.HandleQuery(httptest.NewRecorder(), req)
		if embHits.Load() != 1 {
			t.Fatalf("mode %q: embed hits %d, want 1 — the request ended before the stages this probe counts", mode, embHits.Load())
		}
		return trHits.Load()
	}

	if !llm.NeedsTranslation("wie ist der status der datenbank") {
		t.Fatal("fixture question no longer matches the heuristic — the probe would pass vacuously")
	}
	for _, tc := range []struct {
		mode string
		want int32
	}{
		{"", 1}, // a config without the key: today's behaviour
		{llm.TranslateAuto, 1},
		{llm.TranslateOff, 0},
	} {
		if got := drive(t, tc.mode); got != tc.want {
			t.Errorf("query.translate = %q: translate-role calls %d, want %d", tc.mode, got, tc.want)
		}
	}
}

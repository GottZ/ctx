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
)

// TestQueryTranslateTemporalQuotaGated (§C3): the query handler hands the
// tenant quota to the translation and the temporal LLM fallback, the two
// wire calls that run BEFORE the embed gate. The query is German (translate)
// and temporal without a rule match (LLM fallback), so an ungated handler
// makes two translate-role calls. A tenant over its call budget makes none
// and gets the embed gate's 429; a scope without a policy makes both
// (control). DB-less: the broken pool ends the control at the search stage.
func TestQueryTranslateTemporalQuotaGated(t *testing.T) {
	zero := 0
	acc := backends.NewQuotaAccountant(nil, 0)
	acc.SeedForTest("acme:work", backends.TenantQuota{DailyCalls: &zero, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)

	drive := func(t *testing.T, scope string) (int, int32, int32) {
		t.Helper()
		var trHits atomic.Int32
		trSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			trHits.Add(1)
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"what happened back then with the database"},"eval_count":1,"prompt_eval_count":1}`)
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
		st.cfg.Store(snapshotTestConfig())
		h := NewQueryHandler(brokenPool(t), st, bpool, acc, blocktype.NewRegistry(), snapshotTestAdmitter(t))
		req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"query":"was war damals mit der datenbank"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
			ApiKeyID: "019e0011-aaaa-7000-9000-000000000001", HomeScope: scope, ReadScopes: []string{scope}, IsValid: true,
		}))
		rec := httptest.NewRecorder()
		h.HandleQuery(rec, req)
		return rec.Code, trHits.Load(), embHits.Load()
	}

	code, tr, emb := drive(t, "acme:work")
	if code != http.StatusTooManyRequests {
		t.Fatalf("over call budget: status %d; want 429 from the embed gate", code)
	}
	if tr != 0 || emb != 0 {
		t.Fatalf("over call budget: translate-role hits %d, embed hits %d; want 0/0 — translate and the temporal fallback must not reach the wire", tr, emb)
	}

	_, tr, emb = drive(t, "free:work")
	if tr != 2 || emb != 1 {
		t.Fatalf("control: translate-role hits %d, embed hits %d; want 2 (translate + temporal fallback) and 1", tr, emb)
	}
}

package chat

// Site 12 through the real /v1 handler (W02-10): rerank.Score driven by
// oai.Rerank — the route's own sub-block walk — with the census planner of
// the embeddings route (embedCensusPlanner) on the SAME counter as every
// other site: five documents are the weight plan [2, 2, 1], block one under
// the walker's First lease, blocks two and three each under a continuation
// lease, so Σacquire == Σwire covers one lease per rerank sub-block.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// oaiRerankCensusRoute mounts POST /v1/rerank over chain (each row given the
// rerank role) with the census planner on ca.
func oaiRerankCensusRoute(t *testing.T, ca *countingAdmitter, chain []backends.Backend) http.Handler {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	now := time.Now()
	mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(pgxmock.NewRows(
		[]string{"id", "scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled", "created_at", "updated_at"}).
		AddRow("00000000-0000-4000-8000-0000000c3a21", backends.GlobalScope, "rr", "rerank", "rerank", "public",
			[]byte(`{}`), "", true, now, now))
	reg := modelalias.NewRegistry(mock)
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := make([]backends.Backend, len(chain))
	for i, b := range chain {
		b.Roles = []string{"rerank"}
		rows[i] = b
	}
	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest(rows)
	quota := backends.NewQuotaAccountant(nil, time.Hour)
	quota.SeedTenantForTest("none", backends.TenantQuota{}, 0, 0)
	consumer := &auth.ConsumerAuth{ConsumerID: "00000000-0000-4000-8000-0000000c3a22", TenantID: "00000000-0000-4000-8000-0000000c3a23",
		TenantScope: "acme:main", IsValid: true, AllModels: true, AdmissionLane: "urgent", PromptSensitivity: "public"}
	settings := oai.Settings{Enabled: true, MaxRequestBytes: 1 << 20, InflightBudget: 1 << 30, MaxResponseBytes: 1 << 20,
		RequestTimeout: 30 * time.Second, RequestDeadline: 60 * time.Second}
	get := func() oai.Settings { return settings }
	d := oai.Deps{
		Consumer:      func(context.Context) *auth.ConsumerAuth { return consumer },
		KnowledgeAuth: func(context.Context) bool { return false },
		Aliases:       reg,
		GrantIDs:      func(context.Context, string) ([]string, error) { return nil, nil },
		HasGrant:      func(context.Context, string, string) (bool, error) { return false, nil },
		Settings:      get,
		Pool:          pool,
		Quota:         quota,
		Federation: oai.Federation{
			Floor: func(context.Context) backends.Sensitivity { return backends.SensPublic },
			Scan:  func(r backends.Sensitivity, _ []byte) backends.Sensitivity { return r },
			Filter: func(_ context.Context, c []backends.Backend, _ backends.Sensitivity) ([]backends.Backend, error) {
				return c, nil
			},
		},
		TenantSensitivity: func(context.Context) backends.Sensitivity { return backends.SensPublic },
		Record:            func(llmlog.GatewayRow) {},
		MaxWait:           func() time.Duration { return time.Second },
		RetryAfterSteps:   func() []time.Duration { return nil },
		Planner:           embedCensusPlanner{ca: ca},
		Limiter:           censusLimiter{},
	}
	var budget oai.Budget
	return oai.InflightBudget(&budget, get)(oai.Rerank(d, oai.RerankJina))
}

// postRerankCensus sends one rerank request of n documents to h and fails
// unless it is served with n results.
func postRerankCensus(t *testing.T, h http.Handler, n int) {
	t.Helper()
	docs := make([]string, n)
	for i := range docs {
		docs[i] = fmt.Sprintf("%q", fmt.Sprintf("document %d", i))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/rerank",
		strings.NewReader(`{"model":"rr","query":"q","documents":[`+strings.Join(docs, ",")+`]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), `"relevance_score"`) != n {
		t.Fatalf("the /v1 rerank route answered %d %.300s", w.Code, w.Body.String())
	}
}

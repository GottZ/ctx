package chat

// Sites 9 through the real /v1 handler (W02-07, G16): the gateway's
// non-stream wire site (passthrough.go ChatRaw) driven by oai.ChatCompletions
// — the route's own walk, body build and attempt — instead of the test
// attempt above. The planner is the census's: its acquirer counts on the
// SAME counter as every other site (walkAcquirer), so Σacquire == Σwire
// covers the route.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// censusPlanner is the oai.AdmissionPlanner of the census: the counting
// walk acquirer, the chain unchanged.
type censusPlanner struct{ ca *countingAdmitter }

func (p censusPlanner) AllowedClasses() []dispatch.Class { return walkRules.AllowedClasses }

func (p censusPlanner) Plan(_ context.Context, _ *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	return oai.Plan{Acquirer: censusAcquirer{walkAcquirer{ca: p.ca, class: dispatch.ClassBackground}}, Chain: in.Chain, Lane: "urgent"}, nil
}

func (p censusPlanner) WriteAdmissionError(_ context.Context, w http.ResponseWriter, _ error) string {
	oai.WriteInternal(w)
	return oai.CodeInternalError
}

type censusAcquirer struct{ walkAcquirer }

func (censusAcquirer) Blocks() []int { return []int{1} }

type censusReservation struct{}

func (censusReservation) Settle(dispatch.Usage)        {}
func (censusReservation) Keep()                        {}
func (censusReservation) Abort()                       {}
func (censusReservation) Reject(string, time.Duration) {}
func (censusReservation) Headers() http.Header         { return nil }

type censusLimiter struct{}

func (censusLimiter) Admit(context.Context, *auth.ConsumerAuth, modelalias.Alias, oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	return censusReservation{}, nil
}

// oaiCensusRoute mounts POST /v1/chat/completions over chain with the census
// planner on ca.
func oaiCensusRoute(t *testing.T, ca *countingAdmitter, chain []backends.Backend) http.Handler {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	now := time.Now()
	mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(pgxmock.NewRows(
		[]string{"id", "scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled", "created_at", "updated_at"}).
		AddRow("00000000-0000-4000-8000-0000000c3a01", backends.GlobalScope, "chat", "chat", "chat", "public", []byte(`{}`), "", true, now, now))
	reg := modelalias.NewRegistry(mock)
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest(chain)
	quota := backends.NewQuotaAccountant(nil, time.Hour)
	quota.SeedTenantForTest("none", backends.TenantQuota{}, 0, 0)
	consumer := &auth.ConsumerAuth{ConsumerID: "00000000-0000-4000-8000-0000000c3a02", TenantID: "00000000-0000-4000-8000-0000000c3a03",
		TenantScope: "acme:main", IsValid: true, AllModels: true, AdmissionLane: "urgent", PromptSensitivity: "public"}
	settings := oai.Settings{Enabled: true, MaxRequestBytes: 1 << 20, InflightBudget: 1 << 30, MaxResponseBytes: 1 << 20,
		RequestTimeout: 30 * time.Second, RequestDeadline: 60 * time.Second, DefaultMaxTokens: 16, MaxOutputTokensCeiling: 32}
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
		Planner:           censusPlanner{ca: ca},
		Limiter:           censusLimiter{},
		Reauth:            func(_ context.Context, ca *auth.ConsumerAuth) (*auth.ConsumerAuth, error) { return ca, nil },
	}
	var budget oai.Budget
	return oai.InflightBudget(&budget, get)(oai.ChatCompletions(d))
}

// postCensusStream sends one streamed chat request to h and fails unless
// it is served to its [DONE] (site 10, W02-08).
func postCensusStream(t *testing.T, h http.Handler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("the /v1 stream answered %d %q", w.Code, w.Body.String())
	}
}

// postCensus sends one chat request to h and fails unless it is served.
func postCensus(t *testing.T, h http.Handler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("the /v1 route answered %d %s", w.Code, w.Body.String())
	}
}

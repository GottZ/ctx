package chat

// Site 11 through the real /v1 handler (W02-09, G3): embed.EmbedRaw driven
// by oai.Embeddings — the route's own sub-block walk — with the census
// planner on the SAME counter as every other site. Its acquirer serves the
// weight plan [2, 2, 1]: block one under the walker's First lease, blocks
// two and three each under a continuation lease it counts like any other
// acquire, so Σacquire == Σwire covers one lease per sub-block.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// embedCensusBlock is the census planner's block size: five inputs are the
// weight plan [2, 2, 1].
const embedCensusBlock = 2

// embedCensusPlanner is the census planner of the embeddings route: the
// counting walk acquirer with a continuation door, Continuation set.
type embedCensusPlanner struct{ ca *countingAdmitter }

func (p embedCensusPlanner) AllowedClasses() []dispatch.Class { return walkRules.AllowedClasses }

func (p embedCensusPlanner) Plan(_ context.Context, _ *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	var blocks []int
	for left := max(in.Units, 1); left > 0; left -= embedCensusBlock {
		blocks = append(blocks, min(embedCensusBlock, left))
	}
	return oai.Plan{Acquirer: embedCensusAcquirer{walkAcquirer{ca: p.ca, class: dispatch.ClassBackground}, blocks},
		Chain: in.Chain, Lane: "urgent", Continuation: true}, nil
}

func (p embedCensusPlanner) WriteAdmissionError(_ context.Context, w http.ResponseWriter, _ error) string {
	oai.WriteInternal(w)
	return oai.CodeInternalError
}

type embedCensusAcquirer struct {
	walkAcquirer
	blocks []int
}

func (a embedCensusAcquirer) Blocks() []int { return a.blocks }

// Continue counts every continuation like any other acquire.
func (a embedCensusAcquirer) Continue(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	req := a.req(b, role, timeout)
	req.Continuation = true
	return a.ca.Acquire(ctx, req)
}

// oaiEmbedCensusRoute mounts POST /v1/embeddings over chain (each row given
// the embed role) with the embeddings census planner on ca.
func oaiEmbedCensusRoute(t *testing.T, ca *countingAdmitter, chain []backends.Backend) http.Handler {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mock.Close)
	now := time.Now()
	mock.ExpectQuery(`SELECT .+ FROM context_model_aliases`).WillReturnRows(pgxmock.NewRows(
		[]string{"id", "scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled", "created_at", "updated_at"}).
		AddRow("00000000-0000-4000-8000-0000000c3a11", backends.GlobalScope, "emb", "embedding", "embed", "public",
			[]byte(`{"embedding.native_dims":1024}`), "", true, now, now))
	reg := modelalias.NewRegistry(mock)
	if err := reg.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := make([]backends.Backend, len(chain))
	for i, b := range chain {
		b.Roles = []string{"embed"}
		rows[i] = b
	}
	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest(rows)
	quota := backends.NewQuotaAccountant(nil, time.Hour)
	quota.SeedTenantForTest("none", backends.TenantQuota{}, 0, 0)
	consumer := &auth.ConsumerAuth{ConsumerID: "00000000-0000-4000-8000-0000000c3a12", TenantID: "00000000-0000-4000-8000-0000000c3a13",
		TenantScope: "acme:main", IsValid: true, AllModels: true, AdmissionLane: "urgent", PromptSensitivity: "public"}
	settings := oai.Settings{Enabled: true, MaxRequestBytes: 1 << 20, InflightBudget: 1 << 30, MaxResponseBytes: 1 << 20,
		EmbeddingMaxFloats: 1 << 20, RequestTimeout: 30 * time.Second, RequestDeadline: 60 * time.Second}
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
		Egress:            censusEgress{}, // every row stays: the census counts wire sites, not the egress rule (W06-14)
	}
	var budget oai.Budget
	return oai.InflightBudget(&budget, get)(oai.Embeddings(d))
}

// postEmbedCensus sends one embeddings request of n inputs to h and fails
// unless it is served with n vectors.
func postEmbedCensus(t *testing.T, h http.Handler, n int) {
	t.Helper()
	inputs := make([]string, n)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("%q", fmt.Sprintf("text %d", i))
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings",
		strings.NewReader(`{"model":"emb","input":[`+strings.Join(inputs, ",")+`]}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), `"object":"embedding"`) != n {
		t.Fatalf("the /v1 embeddings route answered %d %.300s", w.Code, w.Body.String())
	}
}

// textEngine answers /v1/embeddings with one 1024-d vector per input, a
// function of the text (so a vector of the wrong text is visible), entries
// in reverse order with their index.
func textEngine(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input json.RawMessage `json:"input"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		var inputs []string
		if json.Unmarshal(req.Input, &inputs) != nil {
			var one string
			_ = json.Unmarshal(req.Input, &one)
			inputs = []string{one}
		}
		var items []string
		for i := len(inputs) - 1; i >= 0; i-- {
			h := fnv.New64a()
			_, _ = io.WriteString(h, inputs[i])
			seed := float64(h.Sum64()%100000) / 1000
			v := make([]float32, embed.TargetDims)
			for j := range v {
				v[j] = float32(math.Sin(seed + float64(j)*0.37))
			}
			raw, _ := json.Marshal(v)
			items = append(items, fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":%s}`, i, raw))
		}
		fmt.Fprintf(w, `{"object":"list","data":[%s],"usage":{"prompt_tokens":%d}}`, strings.Join(items, ","), len(inputs))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmbedThroughTheGateway (W02-09 G11, federation): the internal
// embed.Embed over a backend row that points at the gateway — the alias as
// the row's model, a model key of the fixture — answers the vector of the
// direct path, cosine ≥ 0.9999: the gateway passes the client's own prefix
// through untouched and answers the native vector, the internal client cuts
// and normalizes it as on the direct path. (Here, not in internal/oai: the
// X13 census of that package refuses an embed.Embed call outside a lease.)
func TestEmbedThroughTheGateway(t *testing.T) {
	engine := textEngine(t)
	ca := newCountingAdmitter(t)
	gw := httptest.NewServer(oaiEmbedCensusRoute(t, ca, []backends.Backend{invariantBackend(engine.URL, backends.ProtocolOpenAI)}))
	t.Cleanup(gw.Close)
	viaGateway := backends.Backend{Name: "peer", Host: gw.URL, Protocol: backends.ProtocolOpenAI, Model: "emb",
		APIKey: "ctxm_fixture_model_key", Trust: backends.TrustFull}
	direct := backends.Backend{Name: "direct", Host: engine.URL, Protocol: backends.ProtocolOpenAI, Model: "m", Trust: backends.TrustFull}
	for _, text := range []string{"Wie hoch ist die Zugspitze?", "ctx federation hop"} {
		g, _, err := embed.Embed(context.Background(), viaGateway, text, embed.PrefixQuery)
		if err != nil {
			t.Fatalf("via the gateway: %v", err)
		}
		d, _, err := embed.Embed(context.Background(), direct, text, embed.PrefixQuery)
		if err != nil {
			t.Fatalf("direct: %v", err)
		}
		var dot, ng, nd float64
		for i := range g {
			dot += float64(g[i]) * float64(d[i])
			ng += float64(g[i]) * float64(g[i])
			nd += float64(d[i]) * float64(d[i])
		}
		cos := dot / math.Sqrt(ng*nd)
		t.Logf("%q: cosine %.9f", text, cos)
		if cos < 0.9999 {
			t.Errorf("%q: cosine %.6f, want ≥ 0.9999", text, cos)
		}
	}
	if ca.count() != 2 {
		t.Errorf("gateway acquires %d, want 2 (one lease per request of one block)", ca.count())
	}
}

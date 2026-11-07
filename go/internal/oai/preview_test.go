package oai_test

// The gates of the chain dry run (wave W05-04, design/05 §4.3, §5 B-18,
// masterplan §3.1 W05-04 "Parität zur /v1-Kette"): the preview of a
// (alias, consumer) pair is the chain the real /v1 chat route hands its
// planner — over the real chat rig (pool, federation ingress and filter,
// tenant gate, egress rule, V1Admission with the ExternalAdmissible filter)
// — without a wire call, a reservation, an acquire or a row.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// capturePlanner records the chain every Plan call returns — the chain the
// /v1 route walks after the ExternalAdmissible filter.
type capturePlanner struct {
	oai.AdmissionPlanner
	mu     sync.Mutex
	chains [][]string
}

func (c *capturePlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	p, err := c.AdmissionPlanner.Plan(ctx, ca, in)
	c.mu.Lock()
	c.chains = append(c.chains, rowNames(p.Chain))
	c.mu.Unlock()
	return p, err
}

func (c *capturePlanner) first(t testing.TB) []string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.chains) == 0 {
		t.Fatal("the /v1 request never reached the planner")
	}
	return c.chains[0]
}

func withCapture(cp *capturePlanner) rigOption {
	return withDeps(func(d *oai.Deps) {
		cp.AdmissionPlanner = d.Planner
		d.Planner = cp
	})
}

func rowNames(chain []backends.Backend) []string {
	out := make([]string, 0, len(chain))
	for _, b := range chain {
		out = append(out, b.Name)
	}
	return out
}

func previewNames(rows []oai.PreviewRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Backend)
	}
	return out
}

// previewCtx is the context a /v1 request of ca without federation headers
// reaches the chat handler with in the rig: the rig's consumer key and the
// PeerIngress policy.
func previewCtx(ca *auth.ConsumerAuth) context.Context {
	return federation.HeaderlessContext(context.WithValue(context.Background(), consumerKey{}, ca), ca)
}

// previewRows is the fixture pool of the preview gates: a local full-trust
// engine, a LAN engine and an OpenRouter row, both of trust no-credentials.
func previewRows(t *testing.T) (rows []backends.Backend, ups []*upstream) {
	t.Helper()
	spark, lan, or := newUpstream(t, answerSGLang), newUpstream(t, answerLlamaCpp), newUpstream(t, answerOpenRouter)
	lanRow := lan.row("lanbox", backends.TrustNoCredentials, 15)
	lanRow.Locality = backends.LocalityLAN
	orRow := externalRow(or, "openrouter", backends.ProviderOpenRouter, 10)
	orRow.Trust = backends.TrustNoCredentials
	return []backends.Backend{spark.row("spark", backends.TrustFull, 20), lanRow, orRow}, []*upstream{spark, lan, or}
}

// withPersonalAlias adds the _global chat alias "personal" (sensitivity
// personal) to the rig's registry.
func withPersonalAlias(t *testing.T) rigOption {
	return func(r *chatRig) {
		rows := append(chatAliases(), chatAlias{"00000000-0000-4000-8000-0000000c4a7a", backends.GlobalScope, "personal", "chat", "chat", backends.SensPersonal, `{}`, true})
		r.reg.reload(t, rows)
	}
}

func withConsumer(ca *auth.ConsumerAuth) rigOption {
	return func(r *chatRig) { r.ca.Store(ca) }
}

func consumerWithFloor(s backends.Sensitivity) *auth.ConsumerAuth {
	ca := urgentApp()
	ca.PromptSensitivity = string(s)
	return ca
}

// TestPreviewChainParityWithV1 is gate W05-04 (a): for the same (alias,
// consumer) input the preview's chain is, row for row and in order, the
// chain the /v1 chat route hands its planner after the planner's filter —
// across the terms of RequiredSensitivity (consumer floor, alias, tenant
// overlay) and the egress rule. Red against a free-parameter variant (a
// preview that takes its sensitivity from the alias alone or from the base
// default, B-18): the cases where the floor or the overlay is the maximum
// differ.
func TestPreviewChainParityWithV1(t *testing.T) {
	all := backends.EgressPolicy{AllowExternal: true, AllowUnmetered: true}
	cases := []struct {
		name   string
		alias  string
		ca     *auth.ConsumerAuth
		egress backends.EgressPolicy
	}{
		{"credentials alias, public floor", "chat", consumerWithFloor(backends.SensPublic), all},
		{"public alias, public floor, personal overlay", "pub", consumerWithFloor(backends.SensPublic), all},
		{"public alias, credentials floor", "pub", consumerWithFloor(backends.SensCredentials), all},
		{"personal alias, default egress (no external)", "personal", consumerWithFloor(backends.SensPublic), backends.EgressPolicy{}},
		{"personal alias, external allowed", "personal", consumerWithFloor(backends.SensPublic), all},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, _ := previewRows(t)
			cp := &capturePlanner{}
			r := newChatRig(t, rows, 4, 1, withPersonalAlias(t), withConsumer(tc.ca), withEgress(tc.egress), withCapture(cp))
			if a := r.post(t, chatBody(tc.alias)); a.status != http.StatusOK {
				t.Fatalf("/v1 request: %d %s — want 200 (the parity needs a served request)", a.status, a.body)
			}
			v1 := cp.first(t)
			pv, err := r.deps.PreviewChain(previewCtx(tc.ca), tc.ca, tc.alias)
			got := previewNames(pv.Chain)
			if err != nil || !slices.Equal(got, v1) || len(v1) == 0 {
				t.Errorf("W05-04 parity: preview chain %v (err %v) != /v1 chain %v", got, err, v1)
			}
		})
	}
}

// TestPreviewDefaultDataNoNoCredentialsRow is gate W05-04 (b), B-18: with the
// default data — an alias at the default sensitivity credentials, a consumer
// at the default floor credentials, the base overlay personal — no row of
// trust no-credentials is in the chain; the OpenRouter and LAN rows are
// listed as removed by the pool stage. Red against a preview that falls
// back to pool.default_query_sensitivity (personal) when it has no
// sensitivity source.
func TestPreviewDefaultDataNoNoCredentialsRow(t *testing.T) {
	rows, _ := previewRows(t)
	ca := consumerWithFloor(backends.SensCredentials)
	r := newChatRig(t, rows, 4, 1, withConsumer(ca))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "chat")
	if err != nil || len(pv.Chain) == 0 {
		t.Fatalf("W05-04 default data: preview err %v, chain %v — want a non-empty chain", err, previewNames(pv.Chain))
	}
	for _, row := range pv.Chain {
		if row.Trust == string(backends.TrustNoCredentials) {
			t.Errorf("W05-04 default data: chain carries the no-credentials row %q (B-18 egress downgrade)", row.Backend)
		}
	}
	if pv.RequiredSensitivity != string(backends.SensCredentials) {
		t.Errorf("W05-04 default data: required_sensitivity %q, want credentials", pv.RequiredSensitivity)
	}
	for _, name := range []string{"lanbox", "openrouter"} {
		if !excludedAt(pv, name, oai.PreviewStagePool) {
			t.Errorf("W05-04 default data: %q not listed as excluded at stage %q: %+v", name, oai.PreviewStagePool, pv.Excluded)
		}
	}
}

func excludedAt(pv oai.ChainPreview, backend, stage string) bool {
	return slices.ContainsFunc(pv.Excluded, func(e oai.PreviewExclusion) bool { return e.Backend == backend && e.Stage == stage && e.Reason != "" })
}

// TestPreviewPersonalAliasReachesOpenRouterExternal is the positive control
// of gate W05-04 (b): an alias of sensitivity personal under a public floor
// and an external permission reaches the OpenRouter row, marked locality
// external, provider class openrouter, billing metered. Red against a
// preview that never shows an external row (a gate (b) that passes by
// listing nothing).
func TestPreviewPersonalAliasReachesOpenRouterExternal(t *testing.T) {
	rows, _ := previewRows(t)
	ca := consumerWithFloor(backends.SensPublic)
	r := newChatRig(t, rows, 4, 1, withPersonalAlias(t), withConsumer(ca),
		withEgress(backends.EgressPolicy{AllowExternal: true}))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "personal")
	if err != nil {
		t.Fatalf("W05-04 positive control: preview err %v", err)
	}
	i := slices.IndexFunc(pv.Chain, func(row oai.PreviewRow) bool { return row.Backend == "openrouter" })
	if i < 0 {
		t.Fatalf("W05-04 positive control: no openrouter row in %+v", pv.Chain)
	}
	want := oai.PreviewRow{Backend: "openrouter", Locality: backends.LocalityExternal, ProviderClass: backends.ProviderOpenRouter,
		Billing: backends.BillingMetered.String(), Trust: string(backends.TrustNoCredentials), Priority: 10}
	if pv.Chain[i] != want {
		t.Errorf("W05-04 positive control: row %+v, want %+v", pv.Chain[i], want)
	}
	if pv.RequiredSensitivity != string(backends.SensPersonal) || pv.Op != llmlog.GatewayOpChat {
		t.Errorf("W05-04 positive control: required %q op %q, want personal chat", pv.RequiredSensitivity, pv.Op)
	}
}

// TestPreviewExplainsEgressDrop: the same alias without the external
// permission — the OpenRouter row is not in the chain and is listed as
// removed by the egress stage (WithoutBillable), the LAN row stays (free).
// Red against a preview that drops rows silently.
func TestPreviewExplainsEgressDrop(t *testing.T) {
	rows, _ := previewRows(t)
	ca := consumerWithFloor(backends.SensPublic)
	r := newChatRig(t, rows, 4, 1, withPersonalAlias(t), withConsumer(ca), withEgress(backends.EgressPolicy{}))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "personal")
	if err != nil || !slices.Equal(previewNames(pv.Chain), []string{"spark", "lanbox"}) {
		t.Fatalf("W05-04 egress hint: chain %v (err %v), want [spark lanbox]", previewNames(pv.Chain), err)
	}
	if !excludedAt(pv, "openrouter", oai.PreviewStageEgress) {
		t.Errorf("W05-04 egress hint: openrouter not listed at stage %q: %+v", oai.PreviewStageEgress, pv.Excluded)
	}
	if pv.Chain[1].Billing != backends.BillingFree.String() {
		t.Errorf("W05-04 egress hint: lanbox billing %q, want free", pv.Chain[1].Billing)
	}
}

// TestPreviewNoSideEffects is gate W05-04 (d): a preview that built its
// chain (non-empty, the tenant gate ran once) made 0 wire calls, 0 limiter
// admissions, 0 dispatcher acquires and wrote 0 llmlog rows and 0 access
// records. Red against the stub (no chain) and against a preview that runs
// the route's admission or walk.
func TestPreviewNoSideEffects(t *testing.T) {
	rows, ups := previewRows(t)
	ca := consumerWithFloor(backends.SensPublic)
	r := newChatRig(t, rows, 4, 1, withPersonalAlias(t), withConsumer(ca),
		withEgress(backends.EgressPolicy{AllowExternal: true, AllowUnmetered: true}))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "personal")
	if err != nil || len(pv.Chain) != 3 || r.gate.calls.Load() != 1 {
		t.Fatalf("W05-04 side effects: chain %v (err %v), tenant gate calls %d — want the full chain of 3 and one gate read",
			previewNames(pv.Chain), err, r.gate.calls.Load())
	}
	var wire int64
	for _, u := range ups {
		wire += u.hits.Load()
	}
	got := fmt.Sprintf("wire=%d admits=%d acquires=%d rows=%d records=%d",
		wire, r.limiter.admits.Load(), len(r.serveBy.all()), len(r.rows.all()), len(r.recs))
	if got != "wire=0 admits=0 acquires=0 rows=0 records=0" {
		t.Errorf("W05-04 side effects: %s — a preview contacts no wire and reserves, acquires and records nothing", got)
	}
}

// opEgress records the op of every egress read.
type opEgress struct {
	mu  sync.Mutex
	ops []string
}

func (e *opEgress) Egress(_ context.Context, _ *auth.ConsumerAuth, op string) backends.EgressPolicy {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ops = append(e.ops, op)
	return backends.EgressPolicy{AllowExternal: true, AllowUnmetered: true}
}

func (e *opEgress) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.ops)
}

// TestPreviewOpFollowsKind: the egress policy is read for the op of the
// alias's kind (embedding ⇒ embed — the op the embeddings route passes
// gateChain, where cost-reporting rows count as unmetered), and the answer
// names it. Red against a preview fixed on chat.
func TestPreviewOpFollowsKind(t *testing.T) {
	emb := newUpstream(t, answerSGLang)
	row := emb.row("spark-embed", backends.TrustFull, 20)
	row.Roles = []string{"embed"}
	ca := consumerWithFloor(backends.SensPublic)
	eg := &opEgress{}
	r := newChatRig(t, []backends.Backend{row}, 4, 1, withConsumer(ca), withDeps(func(d *oai.Deps) { d.Egress = eg }))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "emb")
	ops := eg.all()
	if err != nil || pv.Op != llmlog.GatewayOpEmbed || !slices.Equal(ops, []string{llmlog.GatewayOpEmbed}) {
		t.Errorf("W05-04 op: preview op %q, egress ops %v (err %v) — want embed and one egress read for embed", pv.Op, ops, err)
	}
}

// TestPreviewNotGrantedIsANote: a consumer without all_models and without a
// grant on the alias gets no chain and the note alias_not_granted (the /v1
// request would be 404 model_not_found); the alias is still named. Red
// against a preview that skips Resolve (it would show a chain the consumer
// can never reach).
func TestPreviewNotGrantedIsANote(t *testing.T) {
	rows, _ := previewRows(t)
	ca := consumerWithFloor(backends.SensPublic)
	ca.AllModels = false
	r := newChatRig(t, rows, 4, 1, withConsumer(ca))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "chat")
	if err != nil || len(pv.Chain) != 0 || !slices.Contains(pv.Notes, oai.PreviewNoteNotGranted) || pv.Alias == nil || pv.Alias.Name != "chat" {
		t.Errorf("W05-04 grant: chain %v notes %v alias %+v (err %v) — want no chain, note %q and the alias named",
			previewNames(pv.Chain), pv.Notes, pv.Alias, err, oai.PreviewNoteNotGranted)
	}
	r.setGrant(idChat, true)
	pv, err = r.deps.PreviewChain(previewCtx(ca), ca, "chat")
	if err != nil || len(pv.Chain) == 0 || slices.Contains(pv.Notes, oai.PreviewNoteNotGranted) {
		t.Errorf("W05-04 grant: with the grant chain %v notes %v (err %v) — want a chain and no grant note", previewNames(pv.Chain), pv.Notes, err)
	}
}

// TestPreviewAlwaysNotesBodyScan: every preview with an alias carries the
// body-scan note — the preview has no body, the request's scan can only
// raise. Red against a preview that presents its chain as final.
func TestPreviewAlwaysNotesBodyScan(t *testing.T) {
	rows, _ := previewRows(t)
	ca := consumerWithFloor(backends.SensPublic)
	r := newChatRig(t, rows, 4, 1, withConsumer(ca))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "chat")
	if err != nil || !slices.Contains(pv.Notes, oai.PreviewNoteBodyScan) {
		t.Errorf("W05-04 notes: %v (err %v), want %q", pv.Notes, err, oai.PreviewNoteBodyScan)
	}
}

// TestPreviewChainParityExternalOffer closes gate W05-04 (a) over the
// planner's filter: the OpenRouter engine stops offering the external class
// (internal_reserved_slots 0 — dispatch.TargetPolicy.externalCapacity), so
// the /v1 chain the planner returns loses the row; the preview loses it too
// and names the stage external_offer. Red against a preview that stops at
// the egress rule and skips Planner.Plan (mut/preview-skip-planner.md).
func TestPreviewChainParityExternalOffer(t *testing.T) {
	rows, _ := previewRows(t)
	ca := consumerWithFloor(backends.SensPublic)
	cp := &capturePlanner{}
	r := newChatRig(t, rows, 4, 1, withPersonalAlias(t), withConsumer(ca),
		withEgress(backends.EgressPolicy{AllowExternal: true, AllowUnmetered: true}), withCapture(cp))
	pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
	for _, b := range rows {
		reserved := 1
		if b.Name == "openrouter" {
			reserved = 0
		}
		pol.Targets[b.Host] = dispatch.TargetPolicy{Slots: 4, InternalReserved: reserved}
	}
	r.disp.UpdatePolicy(pol)
	if a := r.post(t, chatBody("personal")); a.status != http.StatusOK {
		t.Fatalf("/v1 request: %d %s — want 200", a.status, a.body)
	}
	v1 := cp.first(t)
	if slices.Contains(v1, "openrouter") {
		t.Fatalf("fixture: the /v1 chain %v still carries openrouter — the offer was not withdrawn", v1)
	}
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "personal")
	if got := previewNames(pv.Chain); err != nil || !slices.Equal(got, v1) || !excludedAt(pv, "openrouter", oai.PreviewStageExternalOffer) {
		t.Errorf("W05-04 parity (external offer): preview %v excluded %+v (err %v), /v1 chain %v — want equal and openrouter at stage %q",
			got, pv.Excluded, err, v1, oai.PreviewStageExternalOffer)
	}
}

// TestPreviewNamesWireAndExportStages covers the two BuildChain steps the
// trace reads off keepRows (S2, per production hunk): a row whose protocol
// does not serve chat on /v1 is listed at gateway_wire; a tenant alias
// whose role the server admin did not export loses every operator row at
// export (the no-credentials rows leave earlier, at the pool), the chain
// is empty and the note is the request's 503
// no_eligible_backend — and the /v1 request of the same pair is that 503.
// Red against a trace that drops either stage.
func TestPreviewNamesWireAndExportStages(t *testing.T) {
	rows, _ := previewRows(t)
	anth := newUpstream(t, answerSGLang)
	wireRow := anth.row("claude", backends.TrustFull, 30)
	wireRow.Protocol = backends.ProtocolAnthropic
	rows = append(rows, wireRow)
	ca := consumerWithFloor(backends.SensPublic)
	r := newChatRig(t, rows, 4, 1, withPersonalAlias(t), withConsumer(ca),
		withEgress(backends.EgressPolicy{AllowExternal: true, AllowUnmetered: true}))
	pv, err := r.deps.PreviewChain(previewCtx(ca), ca, "personal")
	if err != nil || slices.Contains(previewNames(pv.Chain), "claude") || !excludedAt(pv, "claude", oai.PreviewStageWire) {
		t.Errorf("W05-04 wire stage: chain %v excluded %+v (err %v) — want claude at stage %q", previewNames(pv.Chain), pv.Excluded, err, oai.PreviewStageWire)
	}
	pv, err = r.deps.PreviewChain(previewCtx(ca), ca, "tchat")
	if err != nil || len(pv.Chain) != 0 || !slices.Contains(pv.Notes, oai.PreviewNoteNoEligible) {
		t.Fatalf("W05-04 export stage: chain %v notes %v (err %v) — want no chain and %q", previewNames(pv.Chain), pv.Notes, err, oai.PreviewNoteNoEligible)
	}
	// tchat is credentials: the no-credentials rows leave at the pool, the
	// operator row spark at the export filter.
	for name, stage := range map[string]string{"spark": oai.PreviewStageExport, "lanbox": oai.PreviewStagePool, "openrouter": oai.PreviewStagePool} {
		if !excludedAt(pv, name, stage) {
			t.Errorf("W05-04 export stage: %q not listed at stage %q: %+v", name, stage, pv.Excluded)
		}
	}
	if a := r.post(t, chatBody("tchat")); a.status != http.StatusServiceUnavailable {
		t.Errorf("W05-04 export stage: the /v1 request answers %d %s, want the 503 the note names", a.status, a.body)
	}
}

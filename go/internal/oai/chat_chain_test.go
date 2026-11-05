package oai_test

// W02-07 gates G5 (K30 RequiredSensitivity), G6 (export filter), G7 (chain
// build before the walk, W02-06 F-4) and G8 (B21 + tenant quota) against the
// real chat route of chat_harness_test.go.

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// chSensRig is the two-row chain of the sensitivity gates: an OpenRouter row
// (trust no-credentials, external, first by priority) and a full-trust row
// behind it. A requirement of credentials leaves only the full-trust row.
type chSensRig struct {
	*chatRig
	or, full *upstream
}

func newChSensRig(t *testing.T, opts ...rigOption) chSensRig {
	t.Helper()
	or := newUpstream(t, answerOpenRouter)
	full := newUpstream(t, answerSGLang)
	orRow := or.row("openrouter", backends.TrustNoCredentials, 20)
	orRow.ProviderClass, orRow.Locality = backends.ProviderOpenRouter, backends.LocalityExternal
	rig := newChatRig(t, []backends.Backend{orRow, full.row("spark", backends.TrustFull, 10)}, 4, 1, opts...)
	return chSensRig{chatRig: rig, or: or, full: full}
}

// served asserts a 200 and which of the two rows answered.
func (r chSensRig) served(t *testing.T, a chatAnswer, wantOR int64) {
	t.Helper()
	if a.status != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", a.status, a.body)
	}
	if got := r.or.hits.Load(); got != wantOR {
		t.Fatalf("no-credentials (openrouter) row hits = %d, want %d", got, wantOR)
	}
	if got, want := r.full.hits.Load(), 1-wantOR; got != want {
		t.Fatalf("full-trust row hits = %d, want %d", got, want)
	}
}

// chConsumer is the harness consumer with prompt sensitivity s.
func chConsumer(s backends.Sensitivity) *auth.ConsumerAuth {
	ca := urgentApp()
	ca.PromptSensitivity = string(s)
	return ca
}

// G5: an app consumer's Ctx-Sensitivity raises its own request (lead
// decision F-W9 no. 1: from EVERY consumer, never lowering).
func TestCtxSensitivityHeaderRaisesChain(t *testing.T) {
	r := newChSensRig(t)
	if r.ca.Load().Kind != auth.KindApp {
		t.Fatal("the fixture consumer must be an app")
	}
	r.served(t, r.post(t, chatBody("pub"), "Ctx-Sensitivity", "credentials"), 0)

	// Control: without the header the public alias and the public consumer
	// leave the no-credentials row in, and it answers first.
	c := newChSensRig(t)
	c.served(t, c.post(t, chatBody("pub")), 1)
}

// G5: the consumer column is a floor of the chain.
func TestConsumerSensitivityRaisesChain(t *testing.T) {
	r := newChSensRig(t)
	r.ca.Store(chConsumer(backends.SensCredentials))
	r.served(t, r.post(t, chatBody("pub")), 0)

	// Control: the same request of a public consumer reaches the row.
	c := newChSensRig(t)
	c.ca.Store(chConsumer(backends.SensPublic))
	c.served(t, c.post(t, chatBody("pub")), 1)
}

// G5: a tenant overlay lowered to public is a term, never the result — the
// credentials alias keeps the chain.
func TestTenantOverlayLoweredKeepsChain(t *testing.T) {
	r := newChSensRig(t)
	r.overlay.Store(backends.SensPublic)
	r.served(t, r.post(t, chatBody("chat")), 0)

	// Control in the other direction: the overlay raises a public alias.
	c := newChSensRig(t)
	c.overlay.Store(backends.SensCredentials)
	c.served(t, c.post(t, chatBody("pub")), 0)
	// … and a public overlay leaves the public alias's chain as it is.
	p := newChSensRig(t)
	p.overlay.Store(backends.SensPublic)
	p.served(t, p.post(t, chatBody("pub")), 1)
}

// chShadowAliases replaces the tenant alias by one named "chat" (public)
// shadowing the _global "chat" (credentials) — the psql edit that bypasses
// the write rule (W02-04 handover 2).
func chShadowAliases(sens backends.Sensitivity) []chatAlias {
	rows := chatAliases()
	for i := range rows {
		if rows[i].id == idChatTenant {
			rows[i].name, rows[i].sens = "chat", sens
		}
	}
	return rows
}

// G5: the _global alias a tenant alias shadows stays the floor.
func TestTenantShadowKeepsGlobalFloor(t *testing.T) {
	r := newChSensRig(t)
	r.reg.reload(t, chShadowAliases(backends.SensPublic))
	r.overlay.Store(backends.SensPublic)
	r.setSettings(func(s *oai.Settings) { s.ExportableRoles = []string{"chat"} })
	r.served(t, r.post(t, chatBody("chat")), 0)
	if got := r.rows.all(); len(got) != 1 || got[0].Telemetry.RequiredSensitivity != string(backends.SensCredentials) {
		t.Fatalf("row required sensitivity = %+v, want credentials", got)
	}

	// Control: a tenant alias without a _global namesake ("tchat", made
	// public) reaches the no-credentials row.
	c := newChSensRig(t)
	rows := chatAliases()
	for i := range rows {
		if rows[i].id == idChatTenant {
			rows[i].sens = backends.SensPublic
		}
	}
	c.reg.reload(t, rows)
	c.overlay.Store(backends.SensPublic)
	c.setSettings(func(s *oai.Settings) { s.ExportableRoles = []string{"chat"} })
	c.served(t, c.post(t, chatBody("tchat")), 1)
}

// chCredentialPrompt carries an AWS access key id (internal/sensitivity
// reAWSKey) in the user message.
const chCredentialPrompt = `"messages":[{"role":"user","content":"deploy with AKIAQWERTYUIOPASDFGH please"}]`

// G5: the scan of the body raises a public request to credentials.
func TestScanRaisesChain(t *testing.T) {
	r := newChSensRig(t)
	r.overlay.Store(backends.SensPublic)
	r.served(t, r.post(t, `{"model":"pub",`+chCredentialPrompt+`}`), 0)

	// Control: the same request without the key stays public.
	c := newChSensRig(t)
	c.overlay.Store(backends.SensPublic)
	c.served(t, c.post(t, chatBody("pub")), 1)
}

// RequiredSensitivity is monotone: the result is never below any term, an
// empty or unknown term reads as credentials, and the shadow term counts
// only for a tenant alias.
func TestRequiredSensitivityMonotone(t *testing.T) {
	vals := []backends.Sensitivity{backends.SensPublic, backends.SensInternal, backends.SensPersonal, backends.SensCredentials, "", "bogus"}
	rank := func(s backends.Sensitivity) int {
		switch s {
		case backends.SensPublic:
			return 0
		case backends.SensInternal:
			return 1
		case backends.SensPersonal:
			return 2
		}
		return 3
	}
	tenant := modelalias.Alias{Name: "chat", Scope: tenantScope}
	for _, floor := range vals {
		for _, alias := range vals {
			for _, overlay := range vals {
				for _, shadow := range vals {
					a := tenant
					a.Sensitivity = alias
					g := modelalias.Alias{Name: "chat", Scope: backends.GlobalScope, Sensitivity: shadow}
					got := oai.RequiredSensitivity(oai.SensitivityTerms{Floor: floor, Alias: a, Shadowed: &g, Overlay: overlay})
					want := max(rank(floor), rank(alias), rank(overlay), rank(shadow))
					if rank(got) != want || !backends.ValidSensitivity(got) {
						t.Fatalf("RequiredSensitivity(floor=%q alias=%q overlay=%q shadow=%q) = %q, want rank %d",
							floor, alias, overlay, shadow, got, want)
					}
				}
			}
		}
	}
	// A _global alias has no shadow: the term is ignored.
	pub := modelalias.Alias{Name: "pub", Scope: backends.GlobalScope, Sensitivity: backends.SensPublic}
	cred := modelalias.Alias{Name: "pub", Scope: backends.GlobalScope, Sensitivity: backends.SensCredentials}
	if got := oai.RequiredSensitivity(oai.SensitivityTerms{Floor: backends.SensPublic, Alias: pub, Shadowed: &cred, Overlay: backends.SensPublic}); got != backends.SensPublic {
		t.Fatalf("a _global alias read a shadow term: %q", got)
	}
	// The zero value (every term empty) is credentials (fail-closed).
	if got := oai.RequiredSensitivity(oai.SensitivityTerms{}); got != backends.SensCredentials {
		t.Fatalf("zero terms = %q, want credentials", got)
	}
}

// G6: a tenant alias whose role is not exported reaches no _global row; an
// exported role does; a row of the tenant's own scope needs no export.
func TestExportFilterKeepsOperatorRows(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	a := r.post(t, chatBody("tchat"))
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend {
		t.Fatalf("tenant alias, role not exported: %d %s, want 503 no_eligible_backend", a.status, a.body)
	}
	if ra := a.header.Get("Retry-After"); ra != "" {
		t.Fatalf("no_eligible_backend carries Retry-After %q", ra)
	}
	if n := u.hits.Load(); n != 0 {
		t.Fatalf("_global row hits = %d, want 0", n)
	}
	if len(r.rows.all()) != 0 {
		t.Fatalf("a refused chain wrote rows: %+v", r.rows.all())
	}

	// Control 1: the server admin exports the role ⇒ served by the same row.
	r.setSettings(func(s *oai.Settings) { s.ExportableRoles = []string{"chat"} })
	if a := r.post(t, chatBody("tchat")); a.status != http.StatusOK || u.hits.Load() != 1 {
		t.Fatalf("exported role: %d %s, hits %d; want 200, 1 hit", a.status, a.body, u.hits.Load())
	}

	// Control 2: a row of the tenant's own scope passes the chain build
	// without the export and becomes the wire attempt. (Its egress is
	// SSRF-guarded — Backend.EgressGuarded — so the loopback fake is refused
	// at dial time: the proof is the attempt on the row, not a 200.)
	own := newUpstream(t, answerSGLang)
	row := own.row("tenant-gpu", backends.TrustFull, 10)
	row.Scope = tenantScope
	tr := newChatRig(t, []backends.Backend{row}, 4, 1)
	ta := tr.post(t, chatBody("tchat"))
	if code, _, _ := ta.envelope(t); code == oai.CodeNoEligibleBackend {
		t.Fatalf("tenant row: %d %s — the export filter removed the tenant's own row", ta.status, ta.body)
	}
	if rows := tr.rows.all(); len(rows) != 1 || len(rows[0].Attempts) != 1 || rows[0].Attempts[0].Backend != "tenant-gpu" {
		t.Fatalf("tenant row: rows %+v, want one attempt on tenant-gpu", rows)
	}
}

// G7: the protocol filter is part of the chain build — an anthropic row
// first by priority never becomes an attempt, gets no health report, and
// the openai row behind it serves with exactly one wire attempt.
func TestChainBuildFiltersProtocolBeforeWalk(t *testing.T) {
	ant := newUpstream(t, answerSGLang)
	oa := newUpstream(t, answerSGLang)
	antRow := ant.row("anthropic-row", backends.TrustFull, 30)
	antRow.Protocol = backends.ProtocolAnthropic
	r := newChatRig(t, []backends.Backend{antRow, oa.row("openai-row", backends.TrustFull, 10)}, 4, 1)
	a := r.post(t, chatBody("chat"))
	if a.status != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", a.status, a.body)
	}
	if ant.hits.Load() != 0 || oa.hits.Load() != 1 {
		t.Fatalf("hits anthropic %d openai %d, want 0 and 1", ant.hits.Load(), oa.hits.Load())
	}
	for _, s := range r.pool.Status() {
		if s.Name == "anthropic-row" && (s.ConsecutiveFails != 0 || s.LastErrorClass != "" || s.LastOK != "" || s.EffectiveState != "active") {
			t.Fatalf("the anthropic row got a health report: %+v", s)
		}
		if s.Name == "openai-row" && s.LastOK == "" {
			t.Fatalf("the serving row got no success report: %+v", s)
		}
	}
	rows := r.rows.all()
	if len(rows) != 1 || len(rows[0].Attempts) != 1 || rows[0].Attempts[0].Backend != "openai-row" {
		t.Fatalf("rows %+v, want one row with one attempt on openai-row", rows)
	}
}

// chNoTenant is a valid consumer without a tenant (a wiring defect).
func chNoTenant() *auth.ConsumerAuth {
	ca := urgentApp()
	ca.TenantID = ""
	return ca
}

// G8 B21: a consumer without a tenant never reaches the quota gate and never
// a backend.
func TestV1EmptyTenantNeverReachesQuota(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	r.ca.Store(chNoTenant())
	a := r.post(t, chatBody("chat"))
	code, _, typ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || typ != oai.TypeServiceUnavailable {
		t.Fatalf("empty tenant: %d %s, want 503 internal_error", a.status, a.body)
	}
	if got := a.header.Get("x-should-retry"); got != "false" {
		t.Fatalf("x-should-retry = %q, want false", got)
	}
	if n := u.hits.Load(); n != 0 {
		t.Fatalf("upstream hits = %d, want 0", n)
	}
	if n := r.gate.calls.Load(); n != 0 {
		t.Fatalf("tenant gate calls = %d, want 0", n)
	}
}

// G8: daily_calls = 0 blocks with 429 insufficient_quota until the next
// full hour — gated by the consumer's tenant id, which no scope maps to.
func TestV1QuotaZeroCallsBlocks(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	zero := 0
	r.quota.SeedTenantForTest(r.ca.Load().TenantID, backends.TenantQuota{Enabled: true, DailyCalls: &zero}, 0, 0)
	a := r.post(t, chatBody("chat"))
	code, _, typ := a.envelope(t)
	if a.status != http.StatusTooManyRequests || code != oai.CodeInsufficientQuota || typ != oai.TypeInsufficientQuota {
		t.Fatalf("quota 0: %d %s, want 429 insufficient_quota", a.status, a.body)
	}
	if got := a.header.Get("x-should-retry"); got != "false" {
		t.Fatalf("x-should-retry = %q, want false", got)
	}
	ra, err := strconv.Atoi(a.header.Get("Retry-After"))
	if err != nil || ra <= 0 || ra > 3600 {
		t.Fatalf("Retry-After = %q, want 1..3600", a.header.Get("Retry-After"))
	}
	if n := u.hits.Load(); n != 0 {
		t.Fatalf("upstream hits = %d, want 0", n)
	}
	if got := r.gate.ids; len(got) != 1 || got[0] != r.ca.Load().TenantID {
		t.Fatalf("gate ids %v, want the consumer's tenant id once", got)
	}

	// Control: a budget left ⇒ served.
	one := 1
	c := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	c.quota.SeedTenantForTest(c.ca.Load().TenantID, backends.TenantQuota{Enabled: true, DailyCalls: &one}, 0, 0)
	if a := c.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Fatalf("quota left: %d %s, want 200", a.status, a.body)
	}
}

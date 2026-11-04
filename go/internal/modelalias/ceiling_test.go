package modelalias

import (
	"context"
	"strings"
	"testing"
)

// testCeilings are the design defaults (design/02 §3.3, W02-02 keys).
func testCeilings() Ceilings {
	return Ceilings{
		MaxOutputTokens: 32768, MaxRequestBytes: 8 << 20, EmbeddingMaxFloats: 1 << 20,
		Tenant: TenantCeiling{MaxOutputTokens: 8192, MaxN: 1, TimeoutS: 600,
			EmbeddingMaxInputs: 32, RerankMaxDocuments: 128, ToolsMaxCount: 64},
	}
}

func mustParse(t *testing.T, kind Kind, raw string) Policy {
	t.Helper()
	p, err := ParsePolicy(kind, []byte(raw))
	if err != nil {
		t.Fatalf("ParsePolicy(%s, %s): %v", kind, raw, err)
	}
	return p
}

func errKeys(errs []*PolicyError) string {
	keys := make([]string, len(errs))
	for i, e := range errs {
		keys[i] = e.Key
	}
	return strings.Join(keys, ",")
}

// TestCheckCeilings (B10, design/02 §3.2/§3.3): every setting-bound ceiling
// refuses one over and admits one at the value, each named by its key; the
// tenant ceiling applies only to aliases outside _global.
func TestCheckCeilings(t *testing.T) {
	c := testCeilings()
	cases := []struct {
		name, kind, policy string
		tenant             bool
		want               string // offending keys, "" = fits
	}{
		{"global max_output_tokens at the ceiling", "chat", `{"max_output_tokens": 32768}`, false, ""},
		{"global max_output_tokens over", "chat", `{"max_output_tokens": 32769}`, false, "max_output_tokens"},
		{"global default_max_tokens over", "chat", `{"default_max_tokens": 32769}`, false, "default_max_tokens"},
		{"vision_max_bytes at the request cap", "chat", `{"vision_max_bytes": 8388608}`, false, ""},
		{"embedding floats at the ceiling", "embedding", `{"embedding.max_inputs": 256, "embedding.native_dims": 4096}`, false, ""},
		{"embedding floats over", "embedding", `{"embedding.max_inputs": 256, "embedding.native_dims": 4097}`, false, "embedding.max_inputs"},
		{"embedding floats at the default inputs blame native_dims", "embedding", `{"embedding.native_dims": 32769}`, false, "embedding.native_dims"},
		{"embedding without native_dims", "embedding", `{"embedding.max_inputs": 256}`, false, ""},
		{"global alias ignores the tenant ceiling", "chat", `{"max_n": 8, "timeout_s": 3600, "tools.max_count": 256}`, false, ""},
		{"tenant max_output_tokens at the ceiling", "chat", `{"max_output_tokens": 8192}`, true, ""},
		{"tenant max_output_tokens over", "chat", `{"max_output_tokens": 8193}`, true, "max_output_tokens"},
		{"tenant default_max_tokens over", "chat", `{"default_max_tokens": 8193}`, true, "default_max_tokens"},
		{"tenant max_n over", "chat", `{"max_n": 2}`, true, "max_n"},
		{"tenant timeout_s 3600", "chat", `{"timeout_s": 3600}`, true, "timeout_s"},
		{"tenant timeout_s at the ceiling", "rerank", `{"timeout_s": 600}`, true, ""},
		{"tenant tools.max_count over", "chat", `{"tools.max_count": 65}`, true, "tools.max_count"},
		{"tenant embedding.max_inputs over", "embedding", `{"embedding.max_inputs": 33}`, true, "embedding.max_inputs"},
		{"tenant rerank.max_documents over", "rerank", `{"rerank.max_documents": 129}`, true, "rerank.max_documents"},
		{"tenant: both global and tenant ceiling", "chat", `{"max_output_tokens": 40000, "max_n": 2}`, true, "max_output_tokens,max_output_tokens,max_n"},
		{"tenant: unset keys are not judged", "chat", `{}`, true, ""},
	}
	for _, tc := range cases {
		p := mustParse(t, Kind(tc.kind), tc.policy)
		if got := errKeys(CheckCeilings(p, c, tc.tenant)); got != tc.want {
			t.Errorf("%s: offending keys %q, want %q", tc.name, got, tc.want)
		}
	}
	over := mustParse(t, KindChat, `{"vision_max_bytes": 8388608}`)
	small := c
	small.MaxRequestBytes = 1 << 20
	if got := errKeys(CheckCeilings(over, small, false)); got != "vision_max_bytes" {
		t.Errorf("vision_max_bytes 8 MiB under a 1 MiB request cap: %q, want vision_max_bytes", got)
	}
	tools := c
	tools.Tenant.ToolsMaxCount = 0
	if got := errKeys(CheckCeilings(mustParse(t, KindChat, `{"tools.max_count": 1}`), tools, true)); got != "tools.max_count" {
		t.Errorf("tools.max_count 1 under a tenant ceiling of 0: %q, want tools.max_count", got)
	}
	for _, e := range CheckCeilings(mustParse(t, KindChat, `{"timeout_s": 3600}`), c, true) {
		if !strings.Contains(e.Reason, "gateway.tenant_alias_ceiling") || !strings.Contains(e.Reason, "600") {
			t.Errorf("reason %q does not name the setting and its value", e.Reason)
		}
	}
}

// TestClampTenant: set entries above the tenant ceiling come down to it,
// unset and fitting ones stay, one note per clamped entry; a ceiling of 0
// clamps tools.max_count to 0 (no tools).
func TestClampTenant(t *testing.T) {
	c := testCeilings().Tenant
	c.ToolsMaxCount = 0
	p := mustParse(t, KindChat, `{"max_output_tokens": 16000, "max_n": 4, "timeout_s": 3600, "tools.max_count": 8, "default_max_tokens": 100, "vision": true}`)
	got, notes := ClampTenant(p, c)
	if got.MaxOutputTokens.V != 8192 || got.MaxN.V != 1 || got.TimeoutS.V != 600 || got.ToolsMaxCount != (Opt[int]{V: 0, Set: true}) {
		t.Errorf("clamped policy = %+v", got)
	}
	if got.DefaultMaxTokens.V != 100 || !got.Vision.V || got.ContextLength.Set {
		t.Errorf("entries within the ceiling moved: %+v", got)
	}
	if strings.Join(notes, "; ") != "max_output_tokens: 16000 → 8192; max_n: 4 → 1; timeout_s: 3600 → 600; tools.max_count: 8 → 0" {
		t.Errorf("notes = %q", notes)
	}
	if p.MaxN.V != 4 {
		t.Errorf("ClampTenant changed its argument: %+v", p)
	}
	if _, notes := ClampTenant(mustParse(t, KindChat, `{"max_n": 1}`), c); notes != nil {
		t.Errorf("a fitting policy reports notes %q", notes)
	}
}

// TestRegistryClampsTenantAliases (design/02 §3.3 "Klemme + WARN"): with a
// ceiling getter, a tenant alias stored above the tenant ceiling is served
// clamped, with one WARN naming it; a _global alias keeps its value; without
// a getter nothing is clamped. The getter is read per reload: a lowered
// ceiling takes effect with the next reload.
func TestRegistryClampsTenantAliases(t *testing.T) {
	ctx := context.Background()
	r, mock, buf := newTestRegistry(t)
	rows := func() {
		mock.ExpectQuery(loadQuery).WillReturnRows(mockRows(
			row{"a1", "_global", "chat", "chat", `{"max_n": 8}`, true},
			row{"a2", "acme", "chat", "chat", `{"max_n": 8, "timeout_s": 3600}`, true},
			row{"a3", "acme", "fits", "chat", `{"max_n": 1}`, true},
		))
	}
	rows()
	if err := r.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a, _ := Lookup(r.Snapshot(), "chat", "acme"); a.Policy.MaxN.V != 8 {
		t.Errorf("without a getter: tenant max_n %d, want 8 (no clamp)", a.Policy.MaxN.V)
	}
	ceil := testCeilings()
	r.SetCeilings(func() Ceilings { return ceil })
	rows()
	if err := r.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := r.Snapshot()
	if a, ok := Lookup(s, "chat", "acme"); !ok || a.Policy.MaxN.V != 1 || a.Policy.TimeoutS.V != 600 {
		t.Errorf("tenant chat = %+v %v, want max_n 1, timeout_s 600", a.Policy, ok)
	}
	if a, _ := Lookup(s, "chat", ""); a.Policy.MaxN.V != 8 {
		t.Errorf("_global chat max_n %d, want 8 (the tenant ceiling does not cap _global)", a.Policy.MaxN.V)
	}
	var warns []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "level=WARN") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "alias_id=a2") ||
		!strings.Contains(warns[0], "max_n: 8 → 1") || !strings.Contains(warns[0], "timeout_s: 3600 → 600") {
		t.Fatalf("WARN lines = %q, want one naming a2 and both clamped keys", warns)
	}
	if !strings.Contains(buf.String(), "clamped=1") {
		t.Errorf("reload summary does not count the clamped alias:\n%s", buf.String())
	}
	ceil.Tenant.MaxN, ceil.Tenant.TimeoutS = 8, 3600 // the getter is read per reload
	rows()
	if err := r.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a, _ := Lookup(r.Snapshot(), "chat", "acme"); a.Policy.MaxN.V != 8 || a.Policy.TimeoutS.V != 3600 {
		t.Errorf("after raising the ceiling: %+v, want the stored values back", a.Policy)
	}
}

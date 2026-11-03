package modelalias

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/GottZ/ctx/internal/backends"
)

var aliasColumns = []string{"id", "scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled", "created_at", "updated_at"}

// row is one context_model_aliases row as the load query returns it.
type row struct {
	id, scope, name, kind, policy string
	enabled                       bool
}

func mockRows(rs ...row) *pgxmock.Rows {
	out := pgxmock.NewRows(aliasColumns)
	ts := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, r := range rs {
		out.AddRow(r.id, r.scope, r.name, r.kind, "chat", "credentials", []byte(r.policy), "", r.enabled, ts, ts)
	}
	return out
}

// newTestRegistry is a registry over a pgxmock pool whose log lines land in
// the returned buffer.
func newTestRegistry(t *testing.T) (*Registry, pgxmock.PgxPoolIface, *bytes.Buffer) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	t.Cleanup(mock.Close)
	buf := &bytes.Buffer{}
	r := NewRegistry(mock)
	r.log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return r, mock, buf
}

const loadQuery = `SELECT .+ FROM context_model_aliases`

func names(as []Alias) string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.Scope + "/" + a.Name
	}
	return strings.Join(out, ",")
}

func TestRegistryStartsEmpty(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	if r.Generation() != 0 || len(r.Snapshot().Aliases()) != 0 || len(Offers(r.Snapshot(), "")) != 0 {
		t.Fatalf("fresh registry: generation %d, %d alias(es) — want 0 and none offered (fail-closed until the first load)",
			r.Generation(), len(r.Snapshot().Aliases()))
	}
	if Offers(nil, "acme:main") != nil {
		t.Errorf("Offers(nil) offers something")
	}
	if _, ok := Lookup(nil, "chat", "acme:main"); ok {
		t.Errorf("Lookup(nil) finds something")
	}
}

// TestRegistryReloadFailureKeepsSnapshot: a failed query, a failed scan and
// a failed row iteration each keep the previous snapshot and its generation.
func TestRegistryReloadFailureKeepsSnapshot(t *testing.T) {
	ctx := context.Background()
	r, mock, _ := newTestRegistry(t)
	mock.ExpectQuery(loadQuery).WillReturnRows(mockRows(row{"a1", "_global", "chat", "chat", `{}`, true}))
	if err := r.Reload(ctx); err != nil {
		t.Fatalf("first reload: %v", err)
	}
	good := r.Snapshot()
	if good.Generation() != 1 || names(good.Aliases()) != "_global/chat" {
		t.Fatalf("after the first reload: generation %d, aliases %s", good.Generation(), names(good.Aliases()))
	}

	mock.ExpectQuery(loadQuery).WillReturnError(errors.New("connection refused"))
	mock.ExpectQuery(loadQuery).WillReturnRows(
		mockRows(row{"a2", "_global", "embed", "embedding", `{}`, true}, row{"a3", "_global", "rerank", "rerank", `{}`, true}).
			RowError(1, errors.New("conn closed mid-stream")))
	mock.ExpectQuery(loadQuery).WillReturnRows(
		pgxmock.NewRows(aliasColumns).AddRow("a4", "_global", "x", "chat", "chat", "credentials", []byte(`{}`), "", "not-a-bool",
			time.Time{}, time.Time{}))
	for i, what := range []string{"query error", "row error", "scan error"} {
		if err := r.Reload(ctx); err == nil {
			t.Fatalf("reload %d (%s): no error", i+2, what)
		}
		if r.Snapshot() != good || r.Generation() != 1 {
			t.Errorf("after the failed reload (%s): generation %d, snapshot replaced=%v — want the previous one",
				what, r.Generation(), r.Snapshot() != good)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

// gateQuerier: the first load blocks until released and returns the OLD
// table; every later load returns the NEW one at once.
type gateQuerier struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (g *gateQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if g.calls.Add(1) == 1 {
		close(g.entered)
		<-g.release
		return mockRows(row{"old", "_global", "old", "chat", `{}`, true}).Kind(), nil
	}
	return mockRows(row{"new", "_global", "new", "chat", `{}`, true}).Kind(), nil
}

// TestRegistryReloadsSerialize (Fremd-Review F-10): a reload that read the
// table earlier never publishes over one that read it later. Reload A blocks
// inside its load; reload B starts meanwhile. Serialized, B waits for A and
// the last generation is B's (the newer read). Without the mutex B publishes
// first and A's older read lands on top with the higher generation.
func TestRegistryReloadsSerialize(t *testing.T) {
	ctx := context.Background()
	g := &gateQuerier{entered: make(chan struct{}), release: make(chan struct{})}
	r := NewRegistry(g)
	r.log = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = r.Reload(ctx) }()
	<-g.entered
	go func() { defer wg.Done(); _ = r.Reload(ctx) }()
	time.Sleep(100 * time.Millisecond)
	close(g.release)
	wg.Wait()
	if got := names(r.Snapshot().Aliases()); r.Generation() != 2 || got != "_global/new" {
		t.Fatalf("after two overlapping reloads: generation %d, aliases %s — want 2 and the later read _global/new", r.Generation(), got)
	}
}

func TestRegistryGenerationIsMonotone(t *testing.T) {
	ctx := context.Background()
	r, mock, _ := newTestRegistry(t)
	for want := uint64(1); want <= 3; want++ {
		mock.ExpectQuery(loadQuery).WillReturnRows(mockRows(row{"a1", "_global", "chat", "chat", `{}`, true}))
		if err := r.Reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got := r.Generation(); got != want {
			t.Fatalf("generation %d, want %d", got, want)
		}
	}
}

// TestRegistryInvalidPolicyIsNotOffered: an unknown policy key, a value over
// a static ceiling (max_n 9, tools.max_count 257) — each such row is loaded,
// never offered, and named by exactly one WARN per reload; reading the
// snapshot logs nothing.
func TestRegistryInvalidPolicyIsNotOffered(t *testing.T) {
	ctx := context.Background()
	r, mock, buf := newTestRegistry(t)
	mock.ExpectQuery(loadQuery).WillReturnRows(mockRows(
		row{"a1", "_global", "good", "chat", `{"max_n": 8}`, true},
		row{"a2", "_global", "unknown-key", "chat", `{"max_tokens": 5}`, true},
		row{"a3", "_global", "max-n-9", "chat", `{"max_n": 9}`, true},
		row{"a4", "_global", "tools-257", "chat", `{"tools.max_count": 257}`, true},
	))
	if err := r.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := r.Snapshot()
	for i := 0; i < 3; i++ {
		if got := names(Offers(s, "")); got != "_global/good" {
			t.Fatalf("offered %s, want only _global/good", got)
		}
	}
	for _, n := range []string{"unknown-key", "max-n-9", "tools-257"} {
		if _, ok := Lookup(s, n, ""); ok {
			t.Errorf("Lookup(%s) finds an invalid alias", n)
		}
	}
	if len(s.Aliases()) != 4 {
		t.Errorf("snapshot holds %d alias(es), want all 4 rows (invalid ones flagged, not dropped)", len(s.Aliases()))
	}
	var warns []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.Contains(line, "level=WARN") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 3 {
		t.Fatalf("%d WARN line(s), want exactly one per invalid alias (3):\n%s", len(warns), buf.String())
	}
	for i, want := range []string{"name=unknown-key", "name=max-n-9", "name=tools-257"} {
		if !strings.Contains(warns[i], want) || !strings.Contains(warns[i], "alias_id=") {
			t.Errorf("WARN %d %q does not name %s and its id", i, warns[i], want)
		}
	}
	for i, key := range []string{"max_tokens", "max_n", "tools.max_count"} {
		if !strings.Contains(warns[i], key) {
			t.Errorf("WARN %d %q does not name the key %s", i, warns[i], key)
		}
	}
}

// fixture: _global chat/embed/old, tenant A shadows chat, adds its own;
// tenant B has a disabled alias and one with a broken policy that shadows
// _global "embed"; a row with a reserved scope.
func fixture(t *testing.T) *Snapshot {
	t.Helper()
	mk := func(id, scope, name string, enabled bool, invalid error) Alias {
		return Alias{ID: id, Scope: scope, Name: name, Kind: KindChat, Role: "chat",
			Sensitivity: backends.SensCredentials, Enabled: enabled, Invalid: invalid}
	}
	broken := &PolicyError{Key: "max_tokens", Reason: "unknown key"}
	return newSnapshot(7, []Alias{
		mk("g-chat", "_global", "chat", true, nil),
		mk("g-embed", "_global", "embed", true, nil),
		mk("g-old", "_global", "old", false, nil),
		mk("a-chat", "acme:main", "chat", true, nil),
		mk("a-own", "acme:main", "own", true, nil),
		mk("b-off", "beta:main", "chat", false, nil),
		mk("b-embed", "beta:main", "embed", true, broken),
		mk("b-solo", "beta:main", "solo", true, nil),
		mk("r-res", "_reserved", "chat", true, nil),
	})
}

func TestOffersVisibilityAndShadowing(t *testing.T) {
	s := fixture(t)
	if s.Generation() != 7 {
		t.Fatalf("generation %d, want 7", s.Generation())
	}
	cases := []struct {
		tenant, want string
	}{
		// Empty scope: _global only (fail-closed, design/01 §4.4).
		{"", "_global/chat,_global/embed"},
		// A reserved scope is no tenant: _global only.
		{"_global", "_global/chat,_global/embed"},
		{"_reserved", "_global/chat,_global/embed"},
		// Tenant A: its chat shadows the _global chat; its own alias joins.
		{"acme:main", "acme:main/chat,_global/embed,acme:main/own"},
		// Tenant B: a disabled tenant alias and a broken one still shadow —
		// the name is gone for B instead of falling back to the operator's
		// alias (a shadow may route elsewhere and carry a higher floor).
		{"beta:main", "beta:main/solo"},
		// A tenant without aliases sees _global; B's and A's rows stay foreign.
		{"gamma:main", "_global/chat,_global/embed"},
	}
	for _, c := range cases {
		if got := names(Offers(s, c.tenant)); got != c.want {
			t.Errorf("Offers(%q) = %s, want %s", c.tenant, got, c.want)
		}
	}
	if a, ok := Lookup(s, "chat", "acme:main"); !ok || a.ID != "a-chat" {
		t.Errorf("Lookup(chat, acme) = %+v %v, want the tenant alias", a, ok)
	}
	if a, ok := Lookup(s, "chat", "gamma:main"); !ok || a.ID != "g-chat" {
		t.Errorf("Lookup(chat, gamma) = %+v %v, want the _global alias", a, ok)
	}
	for _, c := range []struct{ name, tenant string }{
		{"chat", "beta:main"}, {"embed", "beta:main"}, {"own", "beta:main"}, {"own", ""}, {"old", ""}, {"nope", "acme:main"},
	} {
		if a, ok := Lookup(s, c.name, c.tenant); ok {
			t.Errorf("Lookup(%s, %q) = %s, want nothing", c.name, c.tenant, a.ID)
		}
	}
}

// TestLookupAgreesWithOffers: for every name and tenant of the fixture,
// Lookup finds exactly what Offers lists — the one rule (V-0102-h), checked
// against a brute-force reference over backends.VisibleTo.
func TestLookupAgreesWithOffers(t *testing.T) {
	s := fixture(t)
	all := s.Aliases()
	for _, tenant := range []string{"", "_global", "_reserved", "acme:main", "beta:main", "gamma:main"} {
		offered := map[string]string{}
		for _, a := range Offers(s, tenant) {
			offered[a.Name] = a.ID
		}
		for _, name := range []string{"chat", "embed", "old", "own", "solo", "nope"} {
			a, ok := Lookup(s, name, tenant)
			if ok != (offered[name] != "") || (ok && a.ID != offered[name]) {
				t.Errorf("tenant %q name %s: Lookup %s/%v, Offers %q", tenant, name, a.ID, ok, offered[name])
			}
			if ref := reference(all, name, tenant); ref != offered[name] {
				t.Errorf("tenant %q name %s: Offers %q, reference %q", tenant, name, offered[name], ref)
			}
		}
	}
}

// reference: the visible rows of name; a non-_global one decides when present
// (shadowing), else the _global one; offered only when enabled and valid.
func reference(all []Alias, name, tenant string) string {
	var global, own *Alias
	for i := range all {
		a := &all[i]
		if a.Name != name || !backends.VisibleTo(a.Scope, tenant) {
			continue
		}
		if a.Scope == backends.GlobalScope {
			global = a
		} else {
			own = a
		}
	}
	pick := global
	if own != nil {
		pick = own
	}
	if pick == nil || !pick.Enabled || pick.Invalid != nil {
		return ""
	}
	return pick.ID
}

func TestOffersIsSortedAndCopies(t *testing.T) {
	s := newSnapshot(1, []Alias{
		{ID: "z", Scope: "_global", Name: "zeta", Kind: KindChat, Sensitivity: backends.SensCredentials, Enabled: true},
		{ID: "a", Scope: "_global", Name: "alpha", Kind: KindChat, Sensitivity: backends.SensCredentials, Enabled: true},
		{ID: "m", Scope: "t:main", Name: "mid", Kind: KindChat, Sensitivity: backends.SensCredentials, Enabled: true},
	})
	got := Offers(s, "t:main")
	if names(got) != "_global/alpha,t:main/mid,_global/zeta" {
		t.Fatalf("Offers = %s, want sorted by name", names(got))
	}
	got[0].Name = "mutated"
	if names(Offers(s, "t:main")) != "_global/alpha,t:main/mid,_global/zeta" {
		t.Errorf("mutating the result changed the snapshot")
	}
	as := s.Aliases()
	as[0].Name = "mutated"
	if s.Aliases()[0].Name == "mutated" {
		t.Errorf("mutating Aliases() changed the snapshot")
	}
}

// TestValidateRow: what the reload flags beyond the policy — an unknown kind
// or sensitivity, an empty or reserved scope. Such rows are never offered and
// never shadow.
func TestValidateRow(t *testing.T) {
	for _, c := range []struct {
		a     Alias
		valid bool
	}{
		{Alias{Scope: "_global", Kind: KindChat, Sensitivity: backends.SensPublic}, true},
		{Alias{Scope: "acme:main", Kind: KindRerank, Sensitivity: backends.SensCredentials}, true},
		{Alias{Scope: "_global", Kind: "image", Sensitivity: backends.SensPublic}, false},
		{Alias{Scope: "_global", Kind: KindChat, Sensitivity: "secret"}, false},
		{Alias{Scope: "_global", Kind: KindChat, Sensitivity: ""}, false},
		{Alias{Scope: "", Kind: KindChat, Sensitivity: backends.SensPublic}, false},
		{Alias{Scope: "_private", Kind: KindChat, Sensitivity: backends.SensPublic}, false},
	} {
		err := validateRow(c.a.Scope, c.a.Kind, c.a.Sensitivity)
		if (err == nil) != c.valid {
			t.Errorf("%+v: err %v, want valid=%v", c.a, err, c.valid)
		}
	}
	s := newSnapshot(1, []Alias{
		{ID: "e", Scope: "", Name: "chat", Kind: KindChat, Sensitivity: backends.SensPublic, Enabled: true},
		{ID: "g", Scope: "_global", Name: "chat", Kind: KindChat, Sensitivity: backends.SensPublic, Enabled: true},
	})
	if got := names(Offers(s, "acme:main")); got != "_global/chat" {
		t.Errorf("an empty-scope row took part: Offers = %s", got)
	}
}

// TestPackageStaysLeaf: the package imports only the standard library,
// backends (the sensitivity type and the visibility predicate) and pgxdb —
// never config, handler, store or events (design/02 §4.1; the registry is
// read by handler and events, so the reverse import would be a cycle or a
// layering break).
func TestPackageStaysLeaf(t *testing.T) {
	allowed := map[string]bool{
		"github.com/GottZ/ctx/internal/backends": true,
		"github.com/GottZ/ctx/internal/pgxdb":    true,
	}
	imports := productionImports(t)
	var bad []string
	for _, imp := range imports {
		if strings.HasPrefix(imp, "github.com/") && !allowed[imp] && !strings.HasPrefix(imp, "github.com/jackc/pgx/") {
			bad = append(bad, imp)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Errorf("internal/modelalias imports %v, allowed beyond the standard library: backends, pgxdb, pgx", bad)
	}
	if len(imports) == 0 {
		t.Fatal("no imports read — the probe is broken")
	}
}

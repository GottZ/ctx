//go:build integration

// Wave gate for A02-W2 — `ctx backends seed` (design/02 §7 W2). Drives the FULL
// cobra tree (cli.RegisterCommands) against the production whoami + secrets +
// manage surfaces on a real PG18 (testcontainers), because the wave's claims are
// wire claims: the trust posture that makes the seeded chains actually serve, the
// per-row idempotency across two separately committing creates, the foreign-row
// guard, the server-admin tier gate and the sealbox abort class.
//
// The gate proper (empty pool → exactly two _global rows with the designed
// roles/priorities/trust, and Chain(synthesis|embed, credentials) NOT empty —
// a function probe, not a row-existence probe) plus the five negative probes:
//
//	(1) second run          → no-op success, 0 new rows
//	(2) foreign row, no --force → exit ≠ 0, 0 new rows
//	(3) tenant-admin key    → abort, 0 rows in EVERY scope
//	(4) api_key + unconfigured sealbox → abort naming CTX_SECRETS_KEY, 0 rows
//	(5) partial seed        → re-run adds exactly the missing row
//
//	cd go && GOTMPDIR=/compose/n8n/.gotmp GOCACHE=/compose/n8n/.gocache \
//	  go test -tags=integration ./internal/handler/ -run TestBackendsSeedCLI -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedTestKeyHex is a fixed 32-byte master key — a fixture, never a real key.
const seedTestKeyHex = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

// seedHarness is one seeded-from-scratch installation: real DB, DB-backed pool,
// the three surfaces the seed talks to, and a CLI pointed at them.
type seedHarness struct {
	t    *testing.T
	pool *pgxpool.Pool
	bp   *backends.Pool
	srv  *httptest.Server
	ctx  context.Context
}

// newSeedHarness builds the installation. sealboxKey "" is the unconfigured
// server (negative probe 4); tenantAdmin swaps the identity to a tenant-admin
// (negative probe 3).
func newSeedHarness(t *testing.T, sealboxKey string, tenantAdmin bool) *seedHarness {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	t.Setenv(sealbox.EnvKey, sealboxKey)
	t.Setenv(sealbox.EnvKeyPrev, "")

	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(envCfg)
	if err := settings.Reload(ctx, pool, cfgStore); err != nil {
		t.Fatalf("settings reload: %v", err)
	}

	bp := backends.NewPool(pool, nil)
	if err := bp.Reload(ctx); err != nil {
		t.Fatalf("pool reload: %v", err)
	}

	// A real key row: whoami resolves the label from context_api_keys, and the
	// 092 audit trigger casts the actor id to uuid on every write.
	// The key row's own home scope is irrelevant here (only a valid UUID actor
	// id and a resolvable label are needed) — and '_global' is reserved for key
	// rows, so it cannot be used. The AuthResult tier below is set separately.
	label := "srv-admin"
	home := "private"
	if tenantAdmin {
		label, home = "ten-admin", "tenant-a"
	}
	key, _, err := store.CreateApiKey(ctx, pool, label, home, nil, "")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar := &auth.AuthResult{
		ApiKeyID: key.ID, HomeScope: "_global", ReadScopes: []string{"_global"},
		IsValid: true, IsAdmin: true,
	}
	if tenantAdmin {
		ar = &auth.AuthResult{
			ApiKeyID: key.ID, HomeScope: "tenant-a", TenantID: "tenant-a",
			TenantRole: auth.RoleAdmin, ReadScopes: []string{"tenant-a"},
			IsValid: true, IsAdmin: false,
		}
	}

	reload := func(ctx context.Context) error { return settings.Reload(ctx, pool, cfgStore) }
	mh := NewManageHandler(pool, cfgStore, nil, bp, nil, reload, nil, nil)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
		})
	})
	r.Get("/api/whoami", NewWhoamiHandler(pool).HandleWhoami)
	MountSecrets(r, NewSecretsHandler(pool, cfgStore))
	r.Post("/api/manage", mh.HandleManage)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	return &seedHarness{t: t, pool: pool, bp: bp, srv: srv, ctx: ctx}
}

// specFile writes a SeedSpec to a 0600 temp file and returns its path.
func (sh *seedHarness) specFile(body string) string {
	sh.t.Helper()
	path := filepath.Join(sh.t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		sh.t.Fatalf("write spec: %v", err)
	}
	return path
}

// seed runs `ctx backends seed` against the harness (reusing the house CLI
// driver) and returns captured stdout plus the exit error.
func (sh *seedHarness) seed(args ...string) (string, error) {
	sh.t.Helper()
	return w5RunCLI(sh.t, sh.srv.URL, "b6112026072800000000000000000000deadbeefdeadbeefdeadbeefdeadbeef",
		append([]string{"backends", "seed"}, args...)...)
}

// rows returns name → scope for the whole table (every scope: probe 3 asserts
// nothing landed ANYWHERE, not just in _global).
func (sh *seedHarness) rows() map[string]string {
	sh.t.Helper()
	out := map[string]string{}
	rs, err := sh.pool.Query(sh.ctx, `SELECT name, scope FROM context_backends`)
	if err != nil {
		sh.t.Fatalf("select backends: %v", err)
	}
	defer rs.Close()
	for rs.Next() {
		var name, scope string
		if err := rs.Scan(&name, &scope); err != nil {
			sh.t.Fatalf("scan: %v", err)
		}
		out[name] = scope
	}
	return out
}

func (sh *seedHarness) secretNames() []string {
	sh.t.Helper()
	rs, err := sh.pool.Query(sh.ctx, `SELECT name FROM context_secrets ORDER BY name`)
	if err != nil {
		sh.t.Fatalf("select secrets: %v", err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var n string
		if err := rs.Scan(&n); err != nil {
			sh.t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	return out
}

const seedSpecTwoHosts = `{
  "chat":  {"host":"http://gpu-host:11434","model":"qwen3","num_ctx":32768,"think":false},
  "embed": {"host":"http://gpu-host:11434","model":"qwen3-embed"}
}`

// TestBackendsSeedCLI_Integration is the wave gate plus negative probes 1 and 2.
func TestBackendsSeedCLI_Integration(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, false)

	// --- Gate: empty pool → exactly the two designed rows, and the chains SERVE.
	out, err := sh.seed("--file", sh.specFile(seedSpecTwoHosts))
	if err != nil {
		t.Fatalf("seed on an empty pool must succeed, got: %v (stdout %s)", err, out)
	}
	rows := sh.rows()
	if len(rows) != 2 {
		t.Fatalf("want exactly 2 rows, got %d: %v", len(rows), rows)
	}
	for _, name := range []string{"chat-primary", "embed-primary"} {
		if rows[name] != "_global" {
			t.Errorf("row %q scope = %q, want _global", name, rows[name])
		}
	}
	assertSeededRow(t, sh, "chat-primary", []string{"synthesis", "translate", "chat", "digest", "dream"})
	assertSeededRow(t, sh, "embed-primary", []string{"embed", "dream-embed"})

	// The functional probe: trust posture + roles must produce non-empty chains
	// for the operator's OWN blocks (default_block_sensitivity = credentials).
	// A public-trust seed would pass every row-existence check and fail here.
	if err := sh.bp.Reload(sh.ctx); err != nil {
		t.Fatalf("pool reload: %v", err)
	}
	for _, role := range []string{backends.RoleSynthesis, backends.RoleEmbed} {
		chain, cerr := sh.bp.Chain(role, backends.SensCredentials, "_global")
		if cerr != nil || len(chain) == 0 {
			t.Errorf("Chain(%s, credentials) is empty (%v) — the seeded pool does not serve its own blocks", role, cerr)
		}
	}

	// --- Negative probe 1: a second run is a no-op success, 0 new rows.
	out, err = sh.seed("--file", sh.specFile(seedSpecTwoHosts))
	if err != nil {
		t.Fatalf("re-run must succeed as a no-op, got: %v", err)
	}
	var res struct {
		Created []string `json:"created"`
		Skipped []string `json:"skipped"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("re-run stdout is not the result envelope: %v (%s)", jerr, out)
	}
	if len(res.Created) != 0 || len(res.Skipped) != 2 {
		t.Errorf("re-run created=%v skipped=%v, want 0 created / 2 skipped", res.Created, res.Skipped)
	}
	if got := len(sh.rows()); got != 2 {
		t.Errorf("re-run changed the row count to %d", got)
	}

	// --- Negative probe 2: a row outside the target set aborts without --force.
	if _, ierr := sh.pool.Exec(sh.ctx,
		`INSERT INTO context_backends (name, base_url, scope, roles, model_map)
		 VALUES ('legacy-chat','http://legacy:11434','_global','{chat}','{"default":"stub-model"}')`); ierr != nil {
		t.Fatalf("insert foreign row: %v", ierr)
	}
	// backend-list renders the pool SNAPSHOT, which a raw SQL insert reaches via
	// the 053 NOTIFY trigger in production; the test process has no listener, so
	// the reload stands in for it.
	if rerr := sh.bp.Reload(sh.ctx); rerr != nil {
		t.Fatalf("pool reload: %v", rerr)
	}
	if _, err = sh.seed("--file", sh.specFile(seedSpecTwoHosts)); err == nil {
		t.Error("a pool with a foreign row must abort without --force")
	} else if !strings.Contains(err.Error(), "legacy-chat") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("the guard must name the foreign row and the escape hatch, got: %v", err)
	}
	if got := len(sh.rows()); got != 3 {
		t.Errorf("the aborted run changed the row count to %d, want the 3 pre-existing", got)
	}
	if _, err = sh.seed("--force", "--file", sh.specFile(seedSpecTwoHosts)); err != nil {
		t.Errorf("--force must skip exactly the foreign-row guard, got: %v", err)
	}
	if got := len(sh.rows()); got != 3 {
		t.Errorf("--force must not duplicate present target rows, row count = %d", got)
	}
}

// assertSeededRow pins the designed shape of one seeded row.
func assertSeededRow(t *testing.T, sh *seedHarness, name string, wantRoles []string) {
	t.Helper()
	var trust string
	var roles []string
	var priority int
	var enabled bool
	var modelMap []byte
	if err := sh.pool.QueryRow(sh.ctx,
		`SELECT trust, roles, priority, enabled, model_map FROM context_backends WHERE name=$1 AND scope='_global'`,
		name).Scan(&trust, &roles, &priority, &enabled, &modelMap); err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if trust != "full-trust" {
		t.Errorf("%s: trust = %q, want full-trust", name, trust)
	}
	if priority != 100 {
		t.Errorf("%s: priority = %d, want 100", name, priority)
	}
	if !enabled {
		t.Errorf("%s: row is disabled", name)
	}
	if len(roles) != len(wantRoles) {
		t.Fatalf("%s: roles = %v, want %v", name, roles, wantRoles)
	}
	for i, r := range wantRoles {
		if roles[i] != r {
			t.Errorf("%s: roles = %v, want %v", name, roles, wantRoles)
			break
		}
	}
	if len(modelMap) == 0 {
		t.Errorf("%s: empty model_map", name)
	}
}

// TestBackendsSeedCLI_TenantAdminRefused is negative probe 3: a tenant-admin key
// must be refused BEFORE any write — its creates would be pinned to its own
// tenant scope, reporting success while the shared pool stays dead.
func TestBackendsSeedCLI_TenantAdminRefused(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, true)
	_, err := sh.seed("--file", sh.specFile(seedSpecTwoHosts))
	if err == nil {
		t.Fatal("a tenant-admin key must not be able to seed")
	}
	if !strings.Contains(err.Error(), "server-admin") {
		t.Errorf("the refusal must name the required tier, got: %v", err)
	}
	if rows := sh.rows(); len(rows) != 0 {
		t.Errorf("0 rows must exist in EVERY scope after the refusal, got %v", rows)
	}
}

// TestBackendsSeedCLI_SealboxUnconfigured is negative probe 4: an api_key on a
// server without CTX_SECRETS_KEY aborts with the fix in the message and writes
// no rows — never a silent plaintext downgrade.
func TestBackendsSeedCLI_SealboxUnconfigured(t *testing.T) {
	sh := newSeedHarness(t, "", false)
	spec := `{
	  "chat":  {"host":"http://gpu-host:11434","model":"qwen3","api_key":"sk-live-value"},
	  "embed": {"host":"http://gpu-host:11434","model":"qwen3-embed"}
	}`
	_, err := sh.seed("--file", sh.specFile(spec))
	if err == nil {
		t.Fatal("an api_key without a configured sealbox must abort")
	}
	for _, want := range []string{"CTX_SECRETS_KEY", "openssl rand -hex 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the abort must name %q, got: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "sk-live-value") {
		t.Error("the abort message leaked the api_key")
	}
	if rows := sh.rows(); len(rows) != 0 {
		t.Errorf("no row may be written when the credential cannot be sealed, got %v", rows)
	}
}

// TestBackendsSeedCLI_PartialSeedCompletes is negative probe 5: the creates
// commit one by one, so a run that died between them must be completable — the
// re-run adds exactly the missing row and leaves the present one alone.
func TestBackendsSeedCLI_PartialSeedCompletes(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, false)
	if _, err := sh.pool.Exec(sh.ctx,
		`INSERT INTO context_backends (name, base_url, scope, roles, model_map, trust, priority)
		 VALUES ('chat-primary','http://gpu-host:11434','_global','{synthesis,chat}','{"default":"qwen3"}','full-trust',100)`); err != nil {
		t.Fatalf("insert partial row: %v", err)
	}
	// See the note in the foreign-row probe: NOTIFY stands in as an explicit
	// reload here, so the seed reaches the skip decision through backend-list
	// (the designed path) and not through the create-time name collision.
	if err := sh.bp.Reload(sh.ctx); err != nil {
		t.Fatalf("pool reload: %v", err)
	}
	out, err := sh.seed("--file", sh.specFile(seedSpecTwoHosts))
	if err != nil {
		t.Fatalf("re-run after a partial seed must complete it, got: %v", err)
	}
	var res struct {
		Created []string `json:"created"`
		Skipped []string `json:"skipped"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("stdout is not the result envelope: %v (%s)", jerr, out)
	}
	if len(res.Created) != 1 || res.Created[0] != "embed-primary" {
		t.Errorf("created = %v, want exactly [embed-primary]", res.Created)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "chat-primary" {
		t.Errorf("skipped = %v, want exactly [chat-primary]", res.Skipped)
	}
	// The pre-existing row keeps its own shape — a seed completes, it never
	// rewrites what it finds.
	var roles []string
	if err := sh.pool.QueryRow(sh.ctx,
		`SELECT roles FROM context_backends WHERE name='chat-primary'`).Scan(&roles); err != nil {
		t.Fatalf("read chat-primary: %v", err)
	}
	if len(roles) != 2 {
		t.Errorf("the present row was rewritten: roles = %v, want the 2 it was inserted with", roles)
	}
}

// TestBackendsSeedCLI_PresentButUnservedFails is negative probe 6: a target row
// whose NAME is taken but which does not serve its leg's lead role — disabled,
// or stripped of the role — must not report a clean no-op. The row is still
// skipped (a second row would be a duplicate, and rewriting the found one would
// silently rotate live topology), but it is named with its repair, the result
// document says success:false, and the run exits non-zero.
func TestBackendsSeedCLI_PresentButUnservedFails(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, false)
	if _, err := sh.pool.Exec(sh.ctx,
		`INSERT INTO context_backends (name, base_url, scope, roles, model_map, trust, priority, enabled)
		 VALUES ('chat-primary','http://gpu-host:11434','_global','{chat}','{"default":"qwen3"}','full-trust',100,true),
		        ('embed-primary','http://gpu-host:11434','_global','{embed,dream-embed}','{"default":"qwen3-embed"}','full-trust',100,false)`,
	); err != nil {
		t.Fatalf("insert unserved rows: %v", err)
	}
	// See the note in the foreign-row probe: the reload stands in for NOTIFY.
	if err := sh.bp.Reload(sh.ctx); err != nil {
		t.Fatalf("pool reload: %v", err)
	}

	out, err := sh.seed("--file", sh.specFile(seedSpecTwoHosts))
	if err == nil {
		t.Fatal("a present-but-unserved target row must not pass as a seeded pool")
	}
	for _, want := range []string{"chat-primary", "embed-primary", "NOT seeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure must mention %q, got: %v", want, err)
		}
	}
	var res struct {
		Success  bool     `json:"success"`
		Created  []string `json:"created"`
		Skipped  []string `json:"skipped"`
		Unserved []string `json:"unserved"`
	}
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("stdout is not the result envelope: %v (%s)", jerr, out)
	}
	if res.Success {
		t.Error("success must be false while a target role stays unserved")
	}
	if len(res.Unserved) != 2 {
		t.Errorf("unserved = %v, want both target rows", res.Unserved)
	}
	if len(res.Created) != 0 || len(res.Skipped) != 0 {
		t.Errorf("an unserved row is neither created nor a clean skip: created=%v skipped=%v", res.Created, res.Skipped)
	}
	if got := len(sh.rows()); got != 2 {
		t.Errorf("the failing run changed the row count to %d — it must not write", got)
	}
	// The rows keep their shape: the seed names the repair, it does not perform it.
	var roles []string
	if err := sh.pool.QueryRow(sh.ctx,
		`SELECT roles FROM context_backends WHERE name='chat-primary'`).Scan(&roles); err != nil {
		t.Fatalf("read chat-primary: %v", err)
	}
	if len(roles) != 1 || roles[0] != "chat" {
		t.Errorf("the unserved row was rewritten: roles = %v", roles)
	}
}

// TestBackendsSeedCLI_SealsAPIKeyIntoSecret proves the credential path end to
// end: the plaintext key travels ONLY to /api/secrets, the row gets the ref,
// and the ref exists before the row referencing it is written.
func TestBackendsSeedCLI_SealsAPIKeyIntoSecret(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, false)
	spec := `{
	  "chat":  {"host":"http://gpu-host:11434","model":"qwen3","api_key":"sk-live-value"},
	  "embed": {"host":"http://gpu-host:11434","model":"qwen3-embed"}
	}`
	if _, err := sh.seed("--file", sh.specFile(spec)); err != nil {
		t.Fatalf("seed with api_key: %v", err)
	}
	names := sh.secretNames()
	if len(names) != 1 || names[0] != "chat-primary-key" {
		t.Fatalf("secrets = %v, want exactly [chat-primary-key]", names)
	}
	var ref *string
	if err := sh.pool.QueryRow(sh.ctx,
		`SELECT api_key_ref FROM context_backends WHERE name='chat-primary'`).Scan(&ref); err != nil {
		t.Fatalf("read chat-primary: %v", err)
	}
	if ref == nil || *ref != "chat-primary-key" {
		t.Errorf("api_key_ref = %v, want chat-primary-key", ref)
	}
	var plain int
	if err := sh.pool.QueryRow(sh.ctx,
		`SELECT count(*) FROM context_backends WHERE base_url LIKE '%sk-live-value%'`).Scan(&plain); err != nil {
		t.Fatalf("plaintext scan: %v", err)
	}
	if plain != 0 {
		t.Error("the plaintext key reached context_backends")
	}
}

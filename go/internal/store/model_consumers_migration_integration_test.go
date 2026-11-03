//go:build integration

// Gates of W01-1 (migration 161; design/01 §3.3–§3.5 with the masterplan
// amendments K16 whitelist audit, K20 lane/weight/peer columns, K24 identity
// pins; design/03 §3.1; design/04 M-F3; W06-7 F1 tenant index):
//
//   - TestModelAuthGateMatrix: the 13 rows of design/01 §7 against
//     ctx_model_auth(hash, ip, secure). The eleven refusals return the one
//     sentinel row byte-identically; the function is STABLE and writes
//     nothing (n_tup_upd of context_model_keys unchanged over the matrix).
//     Mutation probe: CTX_M161_OVERLAY=gate1…gate6 drops exactly one gate and
//     turns exactly that gate's rows red.
//   - K20 contract: the result columns in their fixed order, the sentinel lane
//     'bulk', a bound peer's instance id in the last column.
//   - I-K8: deactivate + reactivate by raw SQL leaves the old key dead — red
//     with CTX_M161_OVERLAY=no-deactivate.
//   - K24/I-K3: moving a consumer to another tenant or a key to another
//     consumer fails with 23514 — red with CTX_M161_OVERLAY=no-identity.
//   - K16 audit: create / revoke / deactivate (one revoke row per key the
//     trigger revoked) / reactivate / update / prune, each one row with
//     metadata.tenant_id and metadata.consumer_id; the last_used_at batch and
//     the SET NULL of a hard-deleted creator key write no row — red with
//     CTX_M161_OVERLAY=no-of-list; old_value/new_value carry the whitelist
//     only — red with CTX_M161_OVERLAY=blacklist.
//   - CHECK and census: plaintext needs private networks, a binding needs a
//     ctx-peer and both binding columns; the only foreign keys into
//     context_api_keys are the actor columns *_by_key with ON DELETE SET NULL
//     — red with CTX_M161_OVERLAY=extra-fk; no policy column on
//     context_model_keys or context_api_keys — red with
//     CTX_M161_OVERLAY=key-policy.
//   - Idempotence: a second run raises nothing and leaves the catalog
//     identical; nothing is backfilled, every tenant gets the cap 64.
//
// CTX_M161_OVERLAY=<name> applies the real 161 with a list of regexp
// replacements; every pattern must match exactly once, else the test fails —
// an overlay can never silently apply the real migration. Without 161 in the
// tree the setup stops at 160: the red run against the base.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModel|TestMigration161' ./internal/store/
package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m161File    = "161_model_consumers.sql"
	m161Version = 161
)

// m161Overlays: the red variants of 161, each a list of (pattern,
// replacement) pairs.
var m161Overlays = map[string][][2]string{
	// Mutation probe, one gate each.
	"gate1": {{`\s+AND k\.revoked_at IS NULL AND \(k\.expires_at IS NULL OR k\.expires_at > now\(\)\);`, `;`}},
	"gate2": {{`WHERE c\.id = v_cons AND c\.active;`, `WHERE c.id = v_cons;`}},
	"gate3": {{`IF v_status IS NULL OR v_status <> 'active' THEN EXIT gates; END IF;`, ``}},
	"gate4": {{`(?s)IF v_scope IS NOT NULL AND NOT EXISTS \(.*?END IF;`, ``}},
	"gate5": {{`(?s)IF v_cidrs IS NOT NULL AND NOT COALESCE.*?END IF;`, ``}},
	"gate6": {{`(?s)IF NOT COALESCE\(p_secure, false\) AND NOT v_plain THEN.*?END IF;`, ``}},
	// model_cidrs_private without its NULL-element clause.
	"cidr-null": {{`WHERE n IS NULL\s+OR NOT`, `WHERE NOT`}},
	// I-K8 without the insert guard (Fremd-Review 2 F-1).
	"no-insert-guard": {{
		`(?s)CREATE TRIGGER trg_model_keys_consumer_active\s.*?EXECUTE FUNCTION model_keys_consumer_active\(\);`,
		`SELECT 1;`,
	}},
	// I-K8 without the trigger.
	"no-deactivate": {{
		`(?s)CREATE TRIGGER trg_model_consumers_deactivate\s.*?EXECUTE FUNCTION model_consumer_deactivate_revokes\(\);`,
		`SELECT 1;`,
	}},
	// K24 without the pins.
	"no-identity": {
		{`(?s)CREATE TRIGGER trg_model_consumers_identity\s.*?EXECUTE FUNCTION model_consumers_identity_immutable\(\);`, `SELECT 1;`},
		{`(?s)CREATE TRIGGER trg_model_keys_identity\s.*?EXECUTE FUNCTION model_keys_identity_immutable\(\);`, `SELECT 1;`},
	},
	// UPDATE triggers without column list and WHEN.
	"no-of-list": {
		{
			`(?s)CREATE TRIGGER trg_model_consumers_audit_upd\s+AFTER UPDATE OF .*?EXECUTE FUNCTION audit_model_consumers_write\(\);`,
			`CREATE TRIGGER trg_model_consumers_audit_upd AFTER UPDATE ON context_model_consumers FOR EACH ROW EXECUTE FUNCTION audit_model_consumers_write();`,
		},
		{
			`(?s)CREATE TRIGGER trg_model_keys_audit_upd\s+AFTER UPDATE OF .*?EXECUTE FUNCTION audit_model_keys_write\(\);`,
			`CREATE TRIGGER trg_model_keys_audit_upd AFTER UPDATE ON context_model_keys FOR EACH ROW EXECUTE FUNCTION audit_model_keys_write();`,
		},
	},
	// The blacklist projection of design/01 §3.5.
	"blacklist": {
		{
			`(?s)(CREATE OR REPLACE FUNCTION audit_state_model_consumer\(c context_model_consumers\)\s+RETURNS JSONB AS \$\$).*?(\$\$ LANGUAGE sql)`,
			"${1}\n    SELECT to_jsonb(c) - 'key_hash'\n${2}",
		},
		{
			`(?s)(CREATE OR REPLACE FUNCTION audit_state_model_key\(k context_model_keys\)\s+RETURNS JSONB AS \$\$).*?(\$\$ LANGUAGE sql)`,
			"${1}\n    SELECT to_jsonb(k) - 'key_hash'\n${2}",
		},
	},
	// Census: a foreign key into context_api_keys that is no actor column.
	"extra-fk": {{
		`(?m)^(\s+revoked_by_principal UUID REFERENCES context_principals\(id\) ON DELETE SET NULL,)$`,
		"${1}\n    owner_key            UUID REFERENCES context_api_keys(id) ON DELETE CASCADE,",
	}},
	// Census: a policy column on the key table.
	"key-policy": {{
		`(?m)^(\s+key_hint\s+VARCHAR\(16\) NOT NULL,)$`,
		"${1}\n    admission_lane       TEXT NOT NULL DEFAULT 'bulk',",
	}},
}

// m161Body returns the body of 161 to apply ("" when the file is not in the
// tree yet) and the overlay name ("" = real).
func m161Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m161File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m161File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m161File, err)
	}
	overlay = os.Getenv("CTX_M161_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m161Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m161Overlays))
		for n := range m161Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M161_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	out := string(raw)
	for i, rep := range reps {
		re := regexp.MustCompile(rep[0])
		if n := len(re.FindAllStringIndex(out, -1)); n != 1 {
			t.Fatalf("CTX_M161_OVERLAY=%s: pattern %d matches %d time(s) in %s, want exactly 1", overlay, i+1, n, m161File)
		}
		out = re.ReplaceAllString(out, rep[1])
	}
	t.Logf("CTX_M161_OVERLAY=%s: 161 applied with the overlay", overlay)
	return out, overlay
}

// m161Apply brings a database at 160 to 161: the runner for the real file on
// a database whose 160 the runner recorded, otherwise (an overlay of 161, or
// 160 itself applied as an overlay) the body in one transaction.
func m161Apply(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, overlay := m161Body(t)
	if body == "" {
		return
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = $1)`, m161Version-1).Scan(&recorded); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if overlay == "" && recorded {
		if err := store.RunMigrationsUpTo(ctx, pool, m161Version); err != nil {
			t.Fatalf("apply 161: %v", err)
		}
		return
	}
	kaExecInTx(ctx, t, pool, body)
}

// mcSetup returns a database at 161 (or at 160 plus the overlay, or at 160
// alone while 161 is not in the tree).
func mcSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDBUpTo(t, m161Version-1)
	m161Apply(context.Background(), t, pool)
	return pool
}

// mcExecer is a pool, a connection or a transaction.
type mcExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// mcTenant inserts a tenant with the given status and registers its scope
// <slug>:main.
func mcTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug, status string) (id, scope string) {
	t.Helper()
	id = freshTenant(t, ctx, pool, slug)
	scope = slug + ":main"
	if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, scope, id); err != nil {
		t.Fatalf("register scope %s: %v", scope, err)
	}
	if status != "active" {
		if _, err := pool.Exec(ctx, `UPDATE context_tenants SET status = $2 WHERE id = $1::uuid`, id, status); err != nil {
			t.Fatalf("tenant %s status %s: %v", slug, status, err)
		}
	}
	return id, scope
}

// mcInsert inserts one row: tenant-free column → SQL expression pairs of the
// test (no caller input), $1/$2 bound to a and b. Returns the id.
func mcInsert(ctx context.Context, t *testing.T, q mcExecer, table string, base [][2]string, set map[string]string, a, b any) string {
	t.Helper()
	var cols, vals []string
	for _, kv := range base {
		cols, vals = append(cols, kv[0]), append(vals, kv[1])
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cols, vals = append(cols, k), append(vals, set[k])
	}
	var id string
	sql := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s) RETURNING id::text`, table, strings.Join(cols, ", "), strings.Join(vals, ", "))
	if err := q.QueryRow(ctx, sql, a, b).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// mcConsumer inserts a consumer of tenantID; set adds columns (SQL expressions).
func mcConsumer(ctx context.Context, t *testing.T, q mcExecer, tenantID, label string, set map[string]string) string {
	t.Helper()
	return mcInsert(ctx, t, q, "context_model_consumers",
		[][2]string{{"tenant_id", "$1::uuid"}, {"label", "$2"}}, set, tenantID, label)
}

// mcHash is the stored form of a model key: SHA-256, lowercase hex.
func mcHash(seed string) string {
	h := sha256.Sum256([]byte("ctxm_" + seed))
	return hex.EncodeToString(h[:])
}

// mcKey inserts a key of consumerID with the hash of seed.
func mcKey(ctx context.Context, t *testing.T, q mcExecer, consumerID, seed string, set map[string]string) (id, hash string) {
	t.Helper()
	hash = mcHash(seed)
	id = mcInsert(ctx, t, q, "context_model_keys",
		[][2]string{{"consumer_id", "$1::uuid"}, {"key_hash", "$2"}, {"key_hint", "'ctxm_…" + hash[60:] + "'"}}, set, consumerID, hash)
	return id, hash
}

func mcSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// mcSentinel is the sentinel row of ctx_model_auth as row text: no key, no
// consumer, no tenant, no scope, kind '', no release, the strictest
// sensitivity, weight 1, no expiry, not valid, lane bulk, no peer.
const mcSentinel = `(,,,,"",f,credentials,1,,f,bulk,)`

// mcAuthResult is one row of ctx_model_auth.
type mcAuthResult struct {
	text                                    string
	keyID, consumerID, tenantID, scope      *string
	kind, sens, lane                        string
	all, valid                              bool
	weight                                  int16
	expires, peer                           *string
}

func mcAuth(ctx context.Context, t *testing.T, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, hash string, ip *string, secure bool) mcAuthResult {
	t.Helper()
	rows, err := q.Query(ctx, `
		SELECT row(f.*)::text, f.model_key_id::text, f.consumer_id::text, f.tenant_id::text,
		       f.tenant_scope::text, f.kind, f.all_models, f.prompt_sensitivity, f.fair_weight,
		       to_char(f.key_expires_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS'),
		       f.is_valid, f.admission_lane, f.peer_instance_id::text
		  FROM ctx_model_auth($1, $2::inet, $3) f`, hash, ip, secure)
	if err != nil {
		t.Fatalf("ctx_model_auth: %v", err)
	}
	defer rows.Close()
	var out []mcAuthResult
	for rows.Next() {
		var r mcAuthResult
		if err := rows.Scan(&r.text, &r.keyID, &r.consumerID, &r.tenantID, &r.scope, &r.kind, &r.all,
			&r.sens, &r.weight, &r.expires, &r.valid, &r.lane, &r.peer); err != nil {
			t.Fatalf("scan ctx_model_auth: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ctx_model_auth rows: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("ctx_model_auth returned %d row(s), want exactly 1", len(out))
	}
	return out[0]
}

func mcStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func mcIP(s string) *string { return &s }

// nTupUpd flushes this backend's pending statistics and reads n_tup_upd of a
// table. The flush lands when the statement's transaction ends, so the read
// is a statement of its own.
func nTupUpd(ctx context.Context, t *testing.T, conn *pgxpool.Conn, table string) int64 {
	t.Helper()
	if _, err := conn.Exec(ctx, `SELECT pg_stat_force_next_flush()`); err != nil {
		t.Fatalf("flush stats: %v", err)
	}
	var n int64
	if err := conn.QueryRow(ctx, `SELECT n_tup_upd FROM pg_stat_user_tables WHERE relname = $1`, table).Scan(&n); err != nil {
		t.Fatalf("read n_tup_upd of %s: %v", table, err)
	}
	return n
}

func TestModelAuthGateMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)

	tid, scope := mcTenant(ctx, t, pool, "mc-gate", "active")
	suspended, _ := mcTenant(ctx, t, pool, "mc-gate-susp", "suspended")
	offboarding, _ := mcTenant(ctx, t, pool, "mc-gate-off", "offboarding")
	other, _ := mcTenant(ctx, t, pool, "mc-gate-other", "active")

	// The valid consumer carries non-default policy everywhere, so the
	// positive row proves each column comes from the consumer.
	good := mcConsumer(ctx, t, pool, tid, "good", map[string]string{
		"tenant_scope": "'" + scope + "'", "all_models": "true", "prompt_sensitivity": "'internal'",
		"fair_weight": "7", "admission_lane": "'urgent'",
	})
	goodKey, goodHash := mcKey(ctx, t, pool, good, "good", map[string]string{"expires_at": "timestamptz '2099-01-02 03:04:05+00'"})
	_, revokedHash := mcKey(ctx, t, pool, good, "revoked", map[string]string{
		"created_at": "now() - interval '1 hour'", "revoked_at": "now() - interval '1 minute'"})
	_, expiredHash := mcKey(ctx, t, pool, good, "expired", map[string]string{"expires_at": "now() - interval '1 minute'"})
	// A live key at an inactive consumer is unreachable on a regular path: the
	// deactivation revokes it, the insert guard refuses a new one. Gate 2 is
	// defence in depth for a state built past them — the key is minted while
	// the consumer is active, then the consumer is deactivated with the
	// revoking trigger disabled.
	inactive := mcConsumer(ctx, t, pool, tid, "inactive", nil)
	_, inactiveHash := mcKey(ctx, t, pool, inactive, "inactive", nil)
	kaExecInTx(ctx, t, pool, fmt.Sprintf(`ALTER TABLE context_model_consumers DISABLE TRIGGER trg_model_consumers_deactivate;
		UPDATE context_model_consumers SET active = false WHERE id = '%s'::uuid;
		ALTER TABLE context_model_consumers ENABLE TRIGGER trg_model_consumers_deactivate;`, inactive))
	_, suspHash := mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, suspended, "susp", nil), "susp", nil)
	_, offHash := mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, offboarding, "off", nil), "off", nil)
	// A scope of its own, then handed to another tenant.
	if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('mc-gate:moved', $1::uuid)`, tid); err != nil {
		t.Fatalf("scope: %v", err)
	}
	_, movedHash := mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, tid, "moved", map[string]string{"tenant_scope": "'mc-gate:moved'"}), "moved", nil)
	if _, err := pool.Exec(ctx, `UPDATE context_tenant_scopes SET tenant_id = $1::uuid WHERE scope = 'mc-gate:moved'`, other); err != nil {
		t.Fatalf("move scope: %v", err)
	}
	cidr := mcConsumer(ctx, t, pool, tid, "cidr", map[string]string{"allowed_cidrs": "'{10.20.0.0/16}'"})
	_, cidrHash := mcKey(ctx, t, pool, cidr, "cidr", nil)
	_, plainNoHash := mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, tid, "no-plain", nil), "no-plain", nil)
	plain := mcConsumer(ctx, t, pool, tid, "plain", map[string]string{
		"allowed_cidrs": "'{10.40.0.0/16}'", "allow_plaintext": "true"})
	plainKey, plainHash := mcKey(ctx, t, pool, plain, "plain", nil)
	nowConsumer := mcConsumer(ctx, t, pool, tid, "expires-now", nil)

	type row struct {
		name   string
		gate   string // the CTX_M161_OVERLAY that must turn exactly this row red
		hash   string
		ip     *string
		secure bool
		// inTx inserts its key inside the call's transaction (expires_at =
		// now() needs the call's now()) and returns its hash.
		inTx  func(t *testing.T, tx pgx.Tx) string
		check func(t *testing.T, r mcAuthResult) // nil = sentinel
	}
	rows := []row{
		{name: "valid", hash: goodHash, ip: mcIP("203.0.113.5"), secure: true, check: func(t *testing.T, r mcAuthResult) {
			got := fmt.Sprintf("%s %s %s %s %s %v %s %d %s %v %s %s", mcStr(r.keyID), mcStr(r.consumerID), mcStr(r.tenantID),
				mcStr(r.scope), r.kind, r.all, r.sens, r.weight, mcStr(r.expires), r.valid, r.lane, mcStr(r.peer))
			want := fmt.Sprintf("%s %s %s %s app true internal 7 2099-01-02 03:04:05 true urgent <nil>", goodKey, good, tid, scope)
			if got != want {
				t.Errorf("valid key:\n got  %s\n want %s", got, want)
			}
		}},
		{name: "unknown_hash", hash: mcHash("never-minted"), ip: mcIP("203.0.113.5"), secure: true},
		{name: "revoked", gate: "gate1", hash: revokedHash, ip: mcIP("203.0.113.5"), secure: true},
		{name: "expired", gate: "gate1", hash: expiredHash, ip: mcIP("203.0.113.5"), secure: true},
		{name: "expires_at_now", gate: "gate1", ip: mcIP("203.0.113.5"), secure: true, inTx: func(t *testing.T, tx pgx.Tx) string {
			_, h := mcKey(ctx, t, tx, nowConsumer, "expires-now", map[string]string{"expires_at": "now()"})
			return h
		}},
		{name: "consumer_inactive", gate: "gate2", hash: inactiveHash, ip: mcIP("203.0.113.5"), secure: true},
		{name: "tenant_suspended", gate: "gate3", hash: suspHash, ip: mcIP("203.0.113.5"), secure: true},
		{name: "tenant_offboarding", gate: "gate3", hash: offHash, ip: mcIP("203.0.113.5"), secure: true},
		{name: "tenant_scope_moved", gate: "gate4", hash: movedHash, ip: mcIP("203.0.113.5"), secure: true},
		{name: "cidr_missed", gate: "gate5", hash: cidrHash, ip: mcIP("10.30.0.1"), secure: true},
		{name: "cidr_set_ip_null", gate: "gate5", hash: cidrHash, ip: nil, secure: true},
		{name: "no_tls_without_plaintext", gate: "gate6", hash: plainNoHash, ip: mcIP("10.40.1.2"), secure: false},
		{name: "no_tls_with_plaintext_private_cidr", hash: plainHash, ip: mcIP("10.40.1.2"), secure: false, check: func(t *testing.T, r mcAuthResult) {
			if !r.valid || mcStr(r.keyID) != plainKey || mcStr(r.consumerID) != plain || r.lane != "bulk" || r.sens != "credentials" || r.weight != 1 {
				t.Errorf("plaintext key on its private network: %s, want valid with the consumer's defaults", r.text)
			}
		}},
	}
	if len(rows) != 13 {
		t.Fatalf("matrix has %d rows, design/01 §7 names 13", len(rows))
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	updBefore := nTupUpd(ctx, t, conn, "context_model_keys")

	t.Logf("CTX_M161_OVERLAY=%q", os.Getenv("CTX_M161_OVERLAY"))
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			hash := r.hash
			if r.inTx != nil {
				hash = r.inTx(t, tx)
			}
			res := mcAuth(ctx, t, tx, hash, r.ip, r.secure)
			if r.check != nil {
				r.check(t, res)
				return
			}
			if res.text != mcSentinel {
				t.Errorf("refusal (gate overlay %q) returned %s, want the sentinel %s", r.gate, res.text, mcSentinel)
			}
		})
	}

	t.Run("reads_only", func(t *testing.T) {
		if d := nTupUpd(ctx, t, conn, "context_model_keys") - updBefore; d != 0 {
			t.Errorf("n_tup_upd of context_model_keys grew by %d over the matrix, want 0", d)
		}
		var vol string
		if err := conn.QueryRow(ctx, `SELECT provolatile::text FROM pg_proc WHERE proname = 'ctx_model_auth'`).Scan(&vol); err != nil || vol != "s" {
			t.Errorf("ctx_model_auth provolatile %q (err %v), want s (STABLE)", vol, err)
		}
	})
}

// K20: the result columns of ctx_model_auth are a contract — new ones last.
func TestModelAuthContract(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)

	var result, args string
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_function_result('ctx_model_auth'::regproc), pg_get_function_identity_arguments('ctx_model_auth'::regproc)`).
		Scan(&result, &args); err != nil {
		t.Fatalf("read ctx_model_auth: %v", err)
	}
	const wantResult = "TABLE(model_key_id uuid, consumer_id uuid, tenant_id uuid, tenant_scope character varying, " +
		"kind text, all_models boolean, prompt_sensitivity text, fair_weight smallint, " +
		"key_expires_at timestamp with time zone, is_valid boolean, admission_lane text, peer_instance_id uuid)"
	if result != wantResult {
		t.Errorf("ctx_model_auth result:\n got  %s\n want %s", result, wantResult)
	}
	if want := "p_key_hash text, p_client_ip inet, p_secure boolean"; args != want {
		t.Errorf("ctx_model_auth arguments %q, want %q", args, want)
	}

	if r := mcAuth(ctx, t, pool, mcHash("nobody"), nil, true); r.text != mcSentinel || r.lane != "bulk" {
		t.Errorf("sentinel %s lane %q, want %s with lane bulk", r.text, r.lane, mcSentinel)
	}

	tid, _ := mcTenant(ctx, t, pool, "mc-contract", "active")
	instance := uuid.NewString()
	peer := mcConsumer(ctx, t, pool, tid, "peer", map[string]string{
		"kind": "'ctx-peer'", "peer_instance_id": "'" + instance + "'::uuid", "peer_bound_at": "now()"})
	_, h := mcKey(ctx, t, pool, peer, "peer", nil)
	if r := mcAuth(ctx, t, pool, h, nil, true); !r.valid || r.kind != "ctx-peer" || mcStr(r.peer) != instance || r.lane != "bulk" {
		t.Errorf("bound peer: %s, want valid ctx-peer with peer_instance_id %s last", r.text, instance)
	}
}

// I-K8 on the SQL path.
func TestModelConsumerDeactivateRevokes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mc-deact-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	tid, _ := mcTenant(ctx, t, pool, "mc-deact", "active")
	c := mcConsumer(ctx, t, pool, tid, "deact", nil)
	k1, h1 := mcKey(ctx, t, pool, c, "deact-1", nil)
	k2, h2 := mcKey(ctx, t, pool, c, "deact-2", nil)
	k3, _ := mcKey(ctx, t, pool, c, "deact-3", map[string]string{
		"created_at": "timestamptz '2026-01-01 00:00:00+00'", "revoked_at": "timestamptz '2026-01-02 00:00:00+00'"})
	if r := mcAuth(ctx, t, pool, h1, nil, true); !r.valid {
		t.Fatalf("live key before the deactivation: %s", r.text)
	}

	// Deactivation stamped with an actor, reactivation by plain psql.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true)`, admin.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, c); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	kaExec(ctx, t, pool, `UPDATE context_model_consumers SET active = true WHERE id = $1::uuid`, c)

	for _, h := range []string{h1, h2} {
		if r := mcAuth(ctx, t, pool, h, nil, true); r.text != mcSentinel {
			t.Errorf("old key after deactivate + reactivate: %s, want the sentinel (I-K8)", r.text)
		}
	}
	for _, k := range []struct {
		id, wantBy string
		wantOld    bool
	}{{k1, admin.ID, false}, {k2, admin.ID, false}, {k3, "<nil>", true}} {
		var revoked, by *string
		var kept bool
		if err := pool.QueryRow(ctx,
			`SELECT revoked_at::text, revoked_by_key::text, COALESCE(revoked_at = timestamptz '2026-01-02 00:00:00+00', false)
			   FROM context_model_keys WHERE id = $1::uuid`, k.id).Scan(&revoked, &by, &kept); err != nil {
			t.Fatalf("read key: %v", err)
		}
		if revoked == nil || mcStr(by) != k.wantBy || kept != k.wantOld {
			t.Errorf("key %s: revoked_at %s revoked_by_key %s kept-old %v, want revoked by %s, kept-old %v",
				k.id, mcStr(revoked), mcStr(by), kept, k.wantBy, k.wantOld)
		}
	}

	// I-K8 must hold on every path: a key committed by another transaction
	// after the deactivating one began has created_at > the deactivation's
	// now(); revoking it at now() would break ck_model_keys_revoke_order and
	// fail the deactivation (Fremd-Review W01-1 #1).
	t.Run("key_minted_after_the_deactivation_began", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tid, "late-mint", nil)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var txStart string
		if err := tx.QueryRow(ctx, `SELECT now()::text`).Scan(&txStart); err != nil {
			t.Fatalf("pin the transaction start: %v", err)
		}
		late, lateHash := mcKey(ctx, t, pool, c, "late-mint", nil)
		if _, err := tx.Exec(ctx, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, c); err != nil {
			t.Fatalf("deactivation behind a later mint: %v (SQLSTATE %s)", err, mcSQLState(err))
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if r := mcAuth(ctx, t, pool, lateHash, nil, true); r.text != mcSentinel {
			t.Errorf("key minted during the deactivation: %s, want the sentinel", r.text)
		}
		var ordered bool
		if err := pool.QueryRow(ctx, `SELECT revoked_at >= created_at FROM context_model_keys WHERE id = $1::uuid`, late).Scan(&ordered); err != nil || !ordered {
			t.Errorf("late key: revoked_at >= created_at is %v (err %v), want true", ordered, err)
		}
	})

	// A stamp naming a key that no longer exists must not stop the
	// revocation: revoked_by_key stays NULL instead of failing the FK.
	t.Run("stamp_naming_a_deleted_key", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tid, "ghost-actor", nil)
		k, h := mcKey(ctx, t, pool, c, "ghost-actor", nil)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true)`, uuid.NewString()); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, c); err != nil {
			t.Fatalf("deactivation with a dangling actor stamp: %v (SQLSTATE %s)", err, mcSQLState(err))
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var by *string
		if err := pool.QueryRow(ctx, `SELECT revoked_by_key::text FROM context_model_keys WHERE id = $1::uuid`, k).Scan(&by); err != nil || by != nil {
			t.Errorf("revoked_by_key %s (err %v), want NULL for an actor that does not exist", mcStr(by), err)
		}
		if r := mcAuth(ctx, t, pool, h, nil, true); r.text != mcSentinel {
			t.Errorf("key of the deactivated consumer: %s, want the sentinel", r.text)
		}
	})
}

// I-K8 under concurrency (Fremd-Review 2 F-1). The foreign key alone takes
// FOR KEY SHARE on the consumer row and the deactivating UPDATE FOR NO KEY
// UPDATE — compatible, so a key insert that overlaps a deactivation used to
// commit a live key at the inactive consumer, valid again after reactivation.
// The insert guard locks the consumer row FOR SHARE and refuses an inactive
// consumer: under READ COMMITTED (ctx's write isolation) either the
// deactivation waits for the mint and revokes the key, or the mint waits for
// the deactivation and fails with 23514. Red with
// CTX_M161_OVERLAY=no-insert-guard.
func TestModelKeyInsertRacesDeactivation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)
	tid, _ := mcTenant(ctx, t, pool, "mc-race", "active")

	begin := func(t *testing.T) pgx.Tx {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		// READ COMMITTED, as every ctx writer; a lost wait fails instead of hanging.
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
			t.Fatalf("isolation: %v", err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '10s'`); err != nil {
			t.Fatalf("lock_timeout: %v", err)
		}
		return tx
	}
	// noLiveKeyAtInactive: whatever committed, no live key of c may wait for a
	// reactivation — reactivate and authenticate the minted hash.
	noLiveKeyAtInactive := func(t *testing.T, c, hash string) {
		t.Helper()
		var active bool
		var live int
		if err := pool.QueryRow(ctx,
			`SELECT c.active, (SELECT count(*) FROM context_model_keys k WHERE k.consumer_id = c.id AND k.revoked_at IS NULL)
			   FROM context_model_consumers c WHERE c.id = $1::uuid`, c).Scan(&active, &live); err != nil {
			t.Fatalf("read consumer: %v", err)
		}
		if active || live != 0 {
			t.Errorf("after the race: consumer active=%v with %d live key(s), want inactive with 0", active, live)
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET active = true WHERE id = $1::uuid`, c)
		if r := mcAuth(ctx, t, pool, hash, nil, true); r.text != mcSentinel {
			t.Errorf("key minted during the deactivation is valid after reactivation: %s (I-K8)", r.text)
		}
	}
	insertSQL := `INSERT INTO context_model_keys (consumer_id, key_hash, key_hint) VALUES ($1::uuid, $2, 'ctxm_…race')`
	deactivateSQL := `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`

	// The mint starts first and commits while the deactivation is under way.
	t.Run("insert_first", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tid, "race-insert-first", nil)
		hash := mcHash("race-insert-first")
		ins := begin(t)
		defer func() { _ = ins.Rollback(ctx) }()
		deact := begin(t)
		defer func() { _ = deact.Rollback(ctx) }()
		if _, err := ins.Exec(ctx, insertSQL, c, hash); err != nil {
			t.Fatalf("insert: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := deact.Exec(ctx, deactivateSQL, c)
			done <- err
		}()
		time.Sleep(300 * time.Millisecond)
		if err := ins.Commit(ctx); err != nil {
			t.Fatalf("commit insert: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("deactivate: %v (SQLSTATE %s)", err, mcSQLState(err))
		}
		if err := deact.Commit(ctx); err != nil {
			t.Fatalf("commit deactivation: %v", err)
		}
		noLiveKeyAtInactive(t, c, hash)
	})

	// The deactivation starts first; the mint overlaps it and must not commit
	// a live key.
	t.Run("deactivate_first", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tid, "race-deactivate-first", nil)
		hash := mcHash("race-deactivate-first")
		deact := begin(t)
		defer func() { _ = deact.Rollback(ctx) }()
		ins := begin(t)
		defer func() { _ = ins.Rollback(ctx) }()
		if _, err := deact.Exec(ctx, deactivateSQL, c); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := ins.Exec(ctx, insertSQL, c, hash)
			done <- err
		}()
		time.Sleep(300 * time.Millisecond)
		if err := deact.Commit(ctx); err != nil {
			t.Fatalf("commit deactivation: %v", err)
		}
		insErr := <-done
		if insErr == nil {
			if err := ins.Commit(ctx); err != nil {
				t.Logf("insert commit: %v (SQLSTATE %s)", err, mcSQLState(err))
			}
		} else if got := mcSQLState(insErr); got != "23514" {
			t.Errorf("insert behind the deactivation: %v (SQLSTATE %s), want 23514 or a revoked key", insErr, got)
		}
		t.Logf("insert behind the deactivation: err %v (SQLSTATE %q)", insErr, mcSQLState(insErr))
		noLiveKeyAtInactive(t, c, hash)
	})

	// Without concurrency: a key for an inactive consumer is refused.
	t.Run("insert_into_inactive_consumer_is_refused", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tid, "race-sequential", map[string]string{"active": "false"})
		_, err := pool.Exec(ctx, insertSQL, c, mcHash("race-sequential"))
		if got := mcSQLState(err); got != "23514" {
			t.Errorf("key insert into an inactive consumer: err %v (SQLSTATE %q), want 23514", err, got)
		}
	})
}

// K24/I-K3: a consumer stays in its tenant, a key with its consumer.
func TestModelIdentityPins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)
	tid, _ := mcTenant(ctx, t, pool, "mc-pin", "active")
	other, _ := mcTenant(ctx, t, pool, "mc-pin-other", "active")
	c := mcConsumer(ctx, t, pool, tid, "pin", nil)
	c2 := mcConsumer(ctx, t, pool, tid, "pin-2", nil)
	k, _ := mcKey(ctx, t, pool, c, "pin", nil)

	for _, p := range []struct{ name, sql, arg, id string }{
		{"consumer_tenant", `UPDATE context_model_consumers SET tenant_id = $2::uuid WHERE id = $1::uuid`, other, c},
		{"key_consumer", `UPDATE context_model_keys SET consumer_id = $2::uuid WHERE id = $1::uuid`, c2, k},
	} {
		t.Run(p.name+"_is_refused", func(t *testing.T) {
			_, err := pool.Exec(ctx, p.sql, p.id, p.arg)
			if got := mcSQLState(err); got != "23514" {
				t.Errorf("%s: err %v (SQLSTATE %q), want 23514", p.sql, err, got)
			}
		})
	}
	t.Run("same_value_passes", func(t *testing.T) {
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET tenant_id = tenant_id, label = label || '-x' WHERE id = $1::uuid`, c)
		kaExec(ctx, t, pool, `UPDATE context_model_keys SET consumer_id = consumer_id WHERE id = $1::uuid`, k)
	})
}

// mcModelRows reads the audit rows of one model entity.
func mcModelRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entity, key string) []kaRow {
	t.Helper()
	return kaRows(ctx, t, pool, entity, key)
}

func TestModelAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mc-audit-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	tid, scope := mcTenant(ctx, t, pool, "mc-audit", "active")

	stamped := func(fn func(tx pgx.Tx)) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true), set_config('ctx.audit_via', 'api', true),
		                                  set_config('ctx.request_id', 'mc-req', true)`, admin.ID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		fn(tx)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Lifecycle of one consumer and three keys.
	var c, k1, k2, k3, h1 string
	stamped(func(tx pgx.Tx) {
		c = mcConsumer(ctx, t, tx, tid, "audit", map[string]string{
			"tenant_scope": "'" + scope + "'", "created_by_key": "'" + admin.ID + "'::uuid"})
		k1, h1 = mcKey(ctx, t, tx, c, "audit-1", map[string]string{"created_by_key": "'" + admin.ID + "'::uuid"})
	})
	stamped(func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE context_model_keys SET revoked_at = now(), revoked_by_key = $2::uuid WHERE id = $1::uuid`, k1, admin.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	})
	k2, _ = mcKey(ctx, t, pool, c, "audit-2", nil)
	k3, _ = mcKey(ctx, t, pool, c, "audit-3", nil)
	stamped(func(tx pgx.Tx) {
		if _, err := tx.Exec(ctx, `UPDATE context_model_consumers SET active = false, updated_at = now() WHERE id = $1::uuid`, c); err != nil {
			t.Fatalf("deactivate: %v", err)
		}
	})
	kaExec(ctx, t, pool, `UPDATE context_model_consumers SET active = true WHERE id = $1::uuid`, c)
	kaExec(ctx, t, pool, `UPDATE context_model_consumers SET fair_weight = 3, admission_lane = 'urgent' WHERE id = $1::uuid`, c)
	// The tenant prune: keys first, then the consumer.
	kaExec(ctx, t, pool, `DELETE FROM context_model_keys WHERE consumer_id = $1::uuid`, c)
	kaExec(ctx, t, pool, `DELETE FROM context_model_consumers WHERE id = $1::uuid`, c)

	t.Run("lifecycle_actions", func(t *testing.T) {
		want := []struct {
			entity, key, actions string
		}{
			{"model_consumer", c, "create,deactivate,reactivate,update,prune"},
			{"model_key", k1, "create,revoke,prune"},
			{"model_key", k2, "create,revoke,prune"},
			{"model_key", k3, "create,revoke,prune"},
		}
		for _, w := range want {
			rs := mcModelRows(ctx, t, pool, w.entity, w.key)
			if got := kaActions(rs); got != w.actions {
				t.Errorf("%s %s: actions [%s], want [%s]", w.entity, w.key, got, w.actions)
			}
			for _, r := range rs {
				if r.Meta["tenant_id"] != tid || r.Meta["consumer_id"] != c || r.Scope != scope {
					t.Errorf("%s %s %s: metadata %v scope %q, want tenant_id %s consumer_id %s scope %s",
						w.entity, w.key, r.Action, r.Meta, r.Scope, tid, c, scope)
				}
			}
		}
		// Actor and channel: stamped rows name the admin, psql rows read sql.
		cr := mcModelRows(ctx, t, pool, "model_consumer", c)
		if len(cr) == 5 {
			if cr[0].APIKeyID == nil || *cr[0].APIKeyID != admin.ID || cr[0].Meta["via"] != "api" || cr[0].Meta["request_id"] != "mc-req" {
				t.Errorf("stamped create: api_key_id %s metadata %v, want the admin, via api, request id", mcStr(cr[0].APIKeyID), cr[0].Meta)
			}
			if cr[2].APIKeyID != nil || cr[2].Meta["via"] != "sql" {
				t.Errorf("psql reactivate: api_key_id %s metadata %v, want no actor, via sql", mcStr(cr[2].APIKeyID), cr[2].Meta)
			}
		}
		// The keys the deactivation revoked name the deactivating actor.
		for _, k := range []string{k2, k3} {
			rs := mcModelRows(ctx, t, pool, "model_key", k)
			if len(rs) == 3 && (rs[1].APIKeyID == nil || *rs[1].APIKeyID != admin.ID) {
				t.Errorf("key %s revoked by the deactivation: actor %s, want %s", k, mcStr(rs[1].APIKeyID), admin.ID)
			}
		}
	})

	t.Run("state_is_the_whitelist", func(t *testing.T) {
		wantKeys := map[string]string{
			"model_consumer": "active,admission_lane,all_models,allow_plaintext,allowed_cidrs,description,fair_weight,kind,label,peer_bound_at,peer_instance_id,prompt_sensitivity,tenant_id,tenant_scope",
			"model_key":      "consumer_id,expires_at,key_hint,revoked_at,rotated_from",
		}
		rows, err := pool.Query(ctx,
			`SELECT entity_type, entity_key, action, COALESCE(old_value::text, ''), COALESCE(new_value::text, ''),
			        concat_ws('|', entity_key, scope, old_value::text, new_value::text, metadata::text, actor_label)
			   FROM context_settings_audit WHERE entity_type IN ('model_consumer', 'model_key')`)
		if err != nil {
			t.Fatalf("read rows: %v", err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var entity, key, action, oldV, newV, all string
			if err := rows.Scan(&entity, &key, &action, &oldV, &newV, &all); err != nil {
				t.Fatalf("scan: %v", err)
			}
			n++
			if strings.Contains(all, h1) {
				t.Errorf("%s %s %s: the key hash appears in the audit row", entity, key, action)
			}
			for _, v := range []string{oldV, newV} {
				if v == "" {
					continue
				}
				var m map[string]any
				if err := json.Unmarshal([]byte(v), &m); err != nil {
					t.Fatalf("decode %s: %v", v, err)
				}
				got := make([]string, 0, len(m))
				for k := range m {
					got = append(got, k)
				}
				sort.Strings(got)
				if strings.Join(got, ",") != wantKeys[entity] {
					t.Errorf("%s %s %s: state keys [%s], want the whitelist [%s]", entity, key, action, strings.Join(got, ","), wantKeys[entity])
				}
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		if n == 0 {
			t.Fatal("no model audit rows to inspect")
		}
	})

	// Bookkeeping writes of their own consumer and keys, after the lifecycle.
	creator, _, err := store.CreateApiKey(ctx, pool, "mc-audit-creator", "private", nil, "")
	if err != nil {
		t.Fatalf("mint creator: %v", err)
	}
	quiet := mcConsumer(ctx, t, pool, tid, "quiet", map[string]string{"created_by_key": "'" + creator.ID + "'::uuid"})
	mcKey(ctx, t, pool, quiet, "quiet-1", map[string]string{"created_by_key": "'" + creator.ID + "'::uuid"})
	mcKey(ctx, t, pool, quiet, "quiet-2", map[string]string{
		"created_at": "now() - interval '1 hour'", "revoked_at": "now() - interval '1 minute'", "revoked_by_key": "'" + creator.ID + "'::uuid"})
	modelRows := func() int {
		return kaCount(ctx, t, pool, `entity_type IN ('model_consumer', 'model_key')`)
	}

	t.Run("last_used_at_batch_writes_no_row", func(t *testing.T) {
		before := modelRows()
		kaExec(ctx, t, pool, `UPDATE context_model_keys SET last_used_at = now() WHERE consumer_id = $1::uuid`, quiet)
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET updated_at = now() WHERE id = $1::uuid`, quiet)
		if d := modelRows() - before; d != 0 {
			t.Errorf("usage flush and updated_at wrote %d audit row(s), want 0", d)
		}
	})

	t.Run("creator_key_hard_delete_writes_no_row", func(t *testing.T) {
		before := modelRows()
		kaExec(ctx, t, pool, `DELETE FROM context_api_keys WHERE id = $1::uuid`, creator.ID)
		var left int
		if err := pool.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM context_model_consumers WHERE created_by_key = $1::uuid)
			      + (SELECT count(*) FROM context_model_keys WHERE created_by_key = $1::uuid OR revoked_by_key = $1::uuid)`,
			creator.ID).Scan(&left); err != nil || left != 0 {
			t.Fatalf("actor columns still name the deleted key: %d (err %v)", left, err)
		}
		if d := modelRows() - before; d != 0 {
			t.Errorf("ON DELETE SET NULL of the actor columns wrote %d audit row(s), want 0", d)
		}
	})
}

func TestModelConsumerChecksAndCensus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mcSetup(t)
	tid, _ := mcTenant(ctx, t, pool, "mc-check", "active")

	insert := func(label, cols, vals string) error {
		_, err := pool.Exec(ctx, fmt.Sprintf(
			`INSERT INTO context_model_consumers (tenant_id, label%s) VALUES ($1::uuid, $2%s)`, cols, vals), tid, label)
		return err
	}
	for _, c := range []struct {
		name, cols, vals, want string
	}{
		{"plaintext_without_cidrs", ", allow_plaintext", ", true", "23514"},
		{"plaintext_with_any_source", ", allow_plaintext, allowed_cidrs", ", true, '{0.0.0.0/0}'", "23514"},
		{"plaintext_with_one_public_net", ", allow_plaintext, allowed_cidrs", ", true, '{10.0.0.0/8,8.8.8.0/24}'", "23514"},
		{"plaintext_with_empty_list", ", allow_plaintext, allowed_cidrs", ", true, '{}'", "23514"},
		{"plaintext_with_null_element", ", allow_plaintext, allowed_cidrs", ", true, '{10.0.0.0/8,NULL}'", "23514"},
		{"plaintext_with_private_nets", ", allow_plaintext, allowed_cidrs", ", true, '{10.0.0.0/8,192.168.1.0/24,fd00::/8,127.0.0.1/32}'", ""},
		{"binding_on_an_app", ", peer_instance_id, peer_bound_at", ", gen_random_uuid(), now()", "23514"},
		{"binding_without_time", ", kind, peer_instance_id", ", 'ctx-peer', gen_random_uuid()", "23514"},
		{"time_without_binding", ", kind, peer_bound_at", ", 'ctx-peer', now()", "23514"},
		{"binding_on_a_peer", ", kind, peer_instance_id, peer_bound_at", ", 'ctx-peer', gen_random_uuid(), now()", ""},
		{"unknown_lane", ", admission_lane", ", 'interactive'", "23514"},
		{"weight_zero", ", fair_weight", ", 0", "23514"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := insert(c.name, c.cols, c.vals)
			if got := mcSQLState(err); got != c.want {
				t.Errorf("insert %s: err %v (SQLSTATE %q), want %q", c.name, err, got, c.want)
			}
		})
	}

	t.Run("defaults_are_fail_closed", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tid, "defaults", nil)
		var got string
		if err := pool.QueryRow(ctx,
			`SELECT concat_ws(' ', kind, all_models, prompt_sensitivity, fair_weight, admission_lane, allow_plaintext, active,
			                  allowed_cidrs IS NULL, tenant_scope IS NULL, peer_instance_id IS NULL)
			   FROM context_model_consumers WHERE id = $1::uuid`, c).Scan(&got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if want := "app f credentials 1 bulk f t t t t"; got != want {
			t.Errorf("defaults %q, want %q", got, want)
		}
	})

	// design/01 §9.5 Nr. 1: the only foreign keys from a /v1 table into
	// context_api_keys are actor columns *_by_key with ON DELETE SET NULL.
	t.Run("census_fk_into_api_keys_is_actor_only", func(t *testing.T) {
		got, err := stringList(ctx, pool,
			`SELECT c.conrelid::regclass::text || '.' || a.attname || ' ' || c.confdeltype::text
			   FROM pg_constraint c
			   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
			  WHERE c.contype = 'f' AND c.confrelid = 'context_api_keys'::regclass
			    AND c.conrelid::regclass::text LIKE 'context\_model\_%'
			  ORDER BY 1`)
		if err != nil {
			t.Fatalf("read foreign keys: %v", err)
		}
		want := []string{
			"context_model_consumers.created_by_key n",
			"context_model_keys.created_by_key n",
			"context_model_keys.revoked_by_key n",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("foreign keys into context_api_keys:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})

	// design/01 §9.5 Nr. 2 (I-K4): no policy column on the key table, no /v1
	// policy column on the knowledge keys.
	t.Run("census_no_policy_on_keys", func(t *testing.T) {
		policy := []string{"tenant_scope", "kind", "all_models", "prompt_sensitivity", "fair_weight", "admission_lane",
			"allowed_cidrs", "allow_plaintext", "max_parallel", "max_concurrency", "models", "peer_instance_id"}
		for _, table := range []string{"context_model_keys", "context_api_keys"} {
			got, err := stringList(ctx, pool,
				`SELECT column_name::text FROM information_schema.columns
				  WHERE table_schema = current_schema() AND table_name = $1 AND column_name = ANY ($2)
				  ORDER BY 1`, table, policy)
			if err != nil {
				t.Fatalf("read columns: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("%s carries policy column(s) %v", table, got)
			}
		}
	})
}

// m161Catalog renders what 161 owns: its functions, the triggers and indexes
// of both tables, their constraints and columns, the tenant cap column and
// the audit-table indexes.
func m161Catalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('model_cidrs_private', 'model_consumer_deactivate_revokes',
		                      'model_keys_consumer_active',
		                      'model_consumers_identity_immutable', 'model_keys_identity_immutable',
		                      'audit_state_model_consumer', 'audit_state_model_key',
		                      'audit_model_consumers_write', 'audit_model_keys_write', 'ctx_model_auth')
		  ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid IN ('context_model_consumers'::regclass, 'context_model_keys'::regclass)
		  ORDER BY tgname`,
		`SELECT 'constraint: ' || conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
		   FROM pg_constraint
		  WHERE conrelid IN ('context_model_consumers'::regclass, 'context_model_keys'::regclass, 'context_tenants'::regclass)
		  ORDER BY 1`,
		`SELECT 'column: ' || table_name || '.' || column_name || ' ' || data_type || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = current_schema()
		    AND (table_name IN ('context_model_consumers', 'context_model_keys')
		         OR (table_name = 'context_tenants' AND column_name = 'max_model_consumers'))
		  ORDER BY table_name, ordinal_position`,
		`SELECT 'index: ' || indexdef FROM pg_indexes
		  WHERE tablename IN ('context_settings_audit', 'context_model_consumers', 'context_model_keys')
		  ORDER BY tablename, indexname`,
	} {
		lines, err := stringList(ctx, pool, q)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// m161WayBack is the rollback the header of 161 and docs/operations.md
// describe, in their order: the projections take the tables' row types, so
// they go first; the CHECK of the consumers calls model_cidrs_private, so it
// goes after the tables; the version row goes last, so a later upgrade
// applies 161 again.
const m161WayBack = `
DROP FUNCTION ctx_model_auth(TEXT, INET, BOOLEAN);
DROP FUNCTION audit_state_model_key(context_model_keys);
DROP FUNCTION audit_state_model_consumer(context_model_consumers);
DROP TABLE context_model_keys, context_model_consumers;
DROP FUNCTION audit_model_keys_write(), audit_model_consumers_write(),
              model_keys_identity_immutable(), model_consumers_identity_immutable(),
              model_keys_consumer_active(), model_consumer_deactivate_revokes(),
              model_cidrs_private(CIDR[]);
ALTER TABLE context_tenants DROP COLUMN max_model_consumers;
DROP INDEX idx_settings_audit_entity_tenant;
DELETE FROM _migrations WHERE version = 161;`

func TestMigration161Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	// A second tenant exists before 161: the cap reaches every tenant.
	pool := testdb.SetupTestDBUpTo(t, m161Version-1)
	freshTenant(t, ctx, pool, "mc-before-161")
	m161Apply(ctx, t, pool)

	t.Run("no_backfill_and_the_cap_everywhere", func(t *testing.T) {
		var consumers, keys, audit, capped, tenants int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_model_consumers),
		                                     (SELECT count(*) FROM context_model_keys),
		                                     (SELECT count(*) FROM context_settings_audit WHERE entity_type LIKE 'model\_%'),
		                                     (SELECT count(*) FROM context_tenants WHERE max_model_consumers = 64),
		                                     (SELECT count(*) FROM context_tenants)`).
			Scan(&consumers, &keys, &audit, &capped, &tenants); err != nil {
			t.Fatalf("read: %v", err)
		}
		if consumers != 0 || keys != 0 || audit != 0 || capped != tenants || tenants < 2 {
			t.Errorf("after 161: consumers %d keys %d model audit rows %d, tenants with cap 64: %d of %d — want 0/0/0 and all",
				consumers, keys, audit, capped, tenants)
		}
	})

	// The way back of the header and docs/operations.md, in a transaction
	// that is rolled back: the statements run in the documented order and
	// leave none of 161's objects behind.
	t.Run("way_back_is_clean", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, m161WayBack); err != nil {
			t.Fatalf("way back: %v", err)
		}
		var left int
		if err := tx.QueryRow(ctx, `SELECT
			  (SELECT count(*) FROM pg_proc WHERE proname IN ('model_cidrs_private', 'model_consumer_deactivate_revokes', 'model_keys_consumer_active',
			      'model_consumers_identity_immutable', 'model_keys_identity_immutable', 'audit_state_model_consumer',
			      'audit_state_model_key', 'audit_model_consumers_write', 'audit_model_keys_write', 'ctx_model_auth'))
			+ (SELECT count(*) FROM pg_class WHERE relname IN ('context_model_consumers', 'context_model_keys',
			      'idx_settings_audit_entity_tenant'))
			+ (SELECT count(*) FROM information_schema.columns WHERE table_name = 'context_tenants' AND column_name = 'max_model_consumers')
			+ (SELECT count(*) FROM _migrations WHERE version = 161)`).
			Scan(&left); err != nil {
			t.Fatalf("count leftovers: %v", err)
		}
		if left != 0 {
			t.Errorf("%d object(s) of 161 survive the documented way back", left)
		}
	})

	t.Run("second_run_keeps_the_catalog", func(t *testing.T) {
		body, _ := m161Body(t)
		if body == "" {
			t.Fatal("161 is not in the tree")
		}
		before := m161Catalog(ctx, t, pool)
		kaExecInTx(ctx, t, pool, body)
		if after := m161Catalog(ctx, t, pool); after != before {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", before, after)
		}
		t.Logf("catalog (identical across both runs, %d lines)", strings.Count(before, "\n")+1)
	})
}

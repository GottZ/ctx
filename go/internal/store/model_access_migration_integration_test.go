//go:build integration

// Gates of W01-8 (migration 165; design/01 §3.7, §4.6 "Widerruf wirkt",
// §5 B10/B19; masterplan R6, K22):
//
//   - TestModelAuthLiveParity: over the rows of the TestModelAuthGateMatrix
//     fixture that carry no network allowlist, plus a deleted tenant scope, a
//     deleted key, a pruned tenant and an id never minted,
//     ctx_model_auth(hash, NULL, true).is_valid holds exactly when the key's
//     id is NOT among ctx_model_auth_live(ids) — every id in ONE call. Red
//     against 164: SQLSTATE 42883; red with CTX_M165_OVERLAY=live-no-gate4
//     (the moved and the deleted scope read live).
//   - TestModelAccessNotify: every write that can end a key's validity sends
//     exactly the notifications the migration names on ctx_model_access_write
//     — key revoke, key expiry change (the new expires_at, RFC 3339), key
//     delete, consumer deactivation (plus one key notification per live key
//     the 161 trigger revokes), consumer delete, tenant suspension, tenant
//     delete (plus its scope, by cascade), scope moved to another tenant,
//     scope delete; inserts and unlisted columns stay silent; no payload
//     carries a key hash or a key hint. Red against 164 and with
//     CTX_M165_OVERLAY=no-notify.
//   - TestModelKeyUsageFlushNoNotify ("Kein NOTIFY-Sturm"): the real
//     ModelKeyUsage flush over 1 000 keys sends no notification; a revoke
//     after it is the first one to arrive. Red with
//     CTX_M165_OVERLAY=no-of-list (one notification per flushed key).
//   - TestMigration165Idempotent: no row written, ctx_model_auth untouched,
//     the way back of the header leaves the catalog of 164, a second run
//     keeps the catalog identical.
//
// CTX_M165_OVERLAY=<name> applies the real 165 with a list of regexp
// replacements; every pattern must match exactly once, else the test fails —
// an overlay can never silently apply the real migration. Without 165 in the
// tree the setup stops at 164: the red run against the base.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelAuthLiveParity|TestModelAccessNotify|TestModelKeyUsageFlushNoNotify|TestMigration165' ./internal/store/
package store_test

import (
	"context"
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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m165File    = "165_model_access_notify.sql"
	m165Version = 165
	m165Channel = "ctx_model_access_write"
)

// m165Overlays: the red variants of 165, each a list of (pattern,
// replacement) pairs.
var m165Overlays = map[string][][2]string{
	// The function without any of its four triggers.
	"no-notify": {
		{`(?s)CREATE TRIGGER trg_model_keys_access_notify\s.*?EXECUTE FUNCTION notify_model_access_write\(\);`, `SELECT 1;`},
		{`(?s)CREATE TRIGGER trg_model_consumers_access_notify\s.*?EXECUTE FUNCTION notify_model_access_write\(\);`, `SELECT 1;`},
		{`(?s)CREATE TRIGGER trg_tenants_access_notify\s.*?EXECUTE FUNCTION notify_model_access_write\(\);`, `SELECT 1;`},
		{`(?s)CREATE TRIGGER trg_tenant_scopes_access_notify\s.*?EXECUTE FUNCTION notify_model_access_write\(\);`, `SELECT 1;`},
	},
	// ctx_model_auth_live without gate 4 (the tenant owns the consumer's
	// tenant_scope); the closing parenthesis of NOT EXISTS stays.
	"live-no-gate4": {{
		`(?s)\s+AND \(c\.tenant_scope IS NULL OR EXISTS \(.*?s\.tenant_id = c\.tenant_id\)\)`,
		``,
	}},
	// A tenant UPDATE notifies although its status did not change (the
	// display_name path of store.UpdateTenant names status always).
	"tenant-status-always": {{
		`(?s)\s+IF TG_OP = 'UPDATE' AND OLD\.status IS NOT DISTINCT FROM NEW\.status THEN.*?END IF;`,
		``,
	}},
	// The key trigger without its column list: every UPDATE fires.
	"no-of-list": {{
		`(CREATE TRIGGER trg_model_keys_access_notify\s+)AFTER UPDATE OF revoked_at, expires_at OR DELETE`,
		`${1}AFTER UPDATE OR DELETE`,
	}},
}

// m165Body returns the body of 165 to apply ("" when the file is not in the
// tree yet) and the overlay name ("" = real).
func m165Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m165File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m165File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m165File, err)
	}
	overlay = os.Getenv("CTX_M165_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m165Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m165Overlays))
		for n := range m165Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M165_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	out := string(raw)
	for i, rep := range reps {
		re := regexp.MustCompile(rep[0])
		if n := len(re.FindAllStringIndex(out, -1)); n != 1 {
			t.Fatalf("CTX_M165_OVERLAY=%s: pattern %d matches %d time(s) in %s, want exactly 1", overlay, i+1, n, m165File)
		}
		out = re.ReplaceAllString(out, rep[1])
	}
	t.Logf("CTX_M165_OVERLAY=%s: 165 applied with the overlay", overlay)
	return out, overlay
}

// m165Apply brings a database at 164 to 165: the runner for the real file on
// a database whose 164 the runner recorded, otherwise (an overlay of 165) the
// body in one transaction.
func m165Apply(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, overlay := m165Body(t)
	if body == "" {
		return
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = $1)`, m165Version-1).Scan(&recorded); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if overlay == "" && recorded {
		if err := store.RunMigrationsUpTo(ctx, pool, m165Version); err != nil {
			t.Fatalf("apply 165: %v", err)
		}
		return
	}
	kaExecInTx(ctx, t, pool, body)
}

// mxSetup returns a database at 165 (or at 164 plus the overlay, or at 164
// alone while 165 is not in the tree).
func mxSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDBUpTo(t, m165Version-1)
	m165Apply(context.Background(), t, pool)
	return pool
}

func TestModelAuthLiveParity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mxSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mx-live-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}

	// The rows of TestModelAuthGateMatrix without a network allowlist, built
	// with its helpers: gates 5 and 6 pass for every one of them under
	// (ip NULL, secure true), so ctx_model_auth decides on gates 1-4 alone.
	tid, scope := mcTenant(ctx, t, pool, "mx-live", "active")
	suspended, _ := mcTenant(ctx, t, pool, "mx-live-susp", "suspended")
	offboarding, _ := mcTenant(ctx, t, pool, "mx-live-off", "offboarding")
	other, _ := mcTenant(ctx, t, pool, "mx-live-other", "active")

	type row struct {
		name, id, hash string
		valid          bool // the fixture's intended verdict
	}
	var rows []row
	add := func(name, id, hash string, valid bool) { rows = append(rows, row{name, id, hash, valid}) }

	good := mcConsumer(ctx, t, pool, tid, "good", map[string]string{"tenant_scope": "'" + scope + "'"})
	id, h := mcKey(ctx, t, pool, good, "mx-good", map[string]string{"expires_at": "timestamptz '2099-01-02 03:04:05+00'"})
	add("valid", id, h, true)
	id, h = mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, tid, "unscoped", nil), "mx-unscoped", nil)
	add("valid_unscoped", id, h, true)
	id, h = mcKey(ctx, t, pool, good, "mx-revoked", map[string]string{
		"created_at": "now() - interval '1 hour'", "revoked_at": "now() - interval '1 minute'"})
	add("revoked", id, h, false)
	id, h = mcKey(ctx, t, pool, good, "mx-expired", map[string]string{"expires_at": "now() - interval '1 minute'"})
	add("expired", id, h, false)
	// A live key at an inactive consumer, built past the revoking trigger as
	// in the gate matrix.
	inactive := mcConsumer(ctx, t, pool, tid, "inactive", nil)
	id, h = mcKey(ctx, t, pool, inactive, "mx-inactive", nil)
	kaExecInTx(ctx, t, pool, fmt.Sprintf(`ALTER TABLE context_model_consumers DISABLE TRIGGER trg_model_consumers_deactivate;
		UPDATE context_model_consumers SET active = false WHERE id = '%s'::uuid;
		ALTER TABLE context_model_consumers ENABLE TRIGGER trg_model_consumers_deactivate;`, inactive))
	add("consumer_inactive", id, h, false)
	id, h = mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, suspended, "susp", nil), "mx-susp", nil)
	add("tenant_suspended", id, h, false)
	id, h = mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, offboarding, "off", nil), "mx-off", nil)
	add("tenant_offboarding", id, h, false)
	kaExec(ctx, t, pool, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('mx-live:moved', $1::uuid)`, tid)
	id, h = mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, tid, "moved", map[string]string{"tenant_scope": "'mx-live:moved'"}), "mx-moved", nil)
	kaExec(ctx, t, pool, `UPDATE context_tenant_scopes SET tenant_id = $1::uuid WHERE scope = 'mx-live:moved'`, other)
	add("tenant_scope_moved", id, h, false)
	// Beyond the matrix: the scope deleted, the key deleted, the tenant
	// pruned, an id never minted.
	kaExec(ctx, t, pool, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('mx-live:gone', $1::uuid)`, tid)
	id, h = mcKey(ctx, t, pool, mcConsumer(ctx, t, pool, tid, "scope-gone", map[string]string{"tenant_scope": "'mx-live:gone'"}), "mx-scope-gone", nil)
	kaExec(ctx, t, pool, `DELETE FROM context_tenant_scopes WHERE scope = 'mx-live:gone'`)
	add("tenant_scope_deleted", id, h, false)
	id, h = mcKey(ctx, t, pool, good, "mx-deleted", nil)
	kaExec(ctx, t, pool, `DELETE FROM context_model_keys WHERE id = $1::uuid`, id)
	add("key_deleted", id, h, false)
	f := mkSeedPruneTenant(ctx, t, pool, "mx-prune", 1)
	pruned := map[string]string{}
	{
		qr, err := pool.Query(ctx, `SELECT id::text, key_hash FROM context_model_keys WHERE id = ANY($1::uuid[]) ORDER BY id`, f.keys)
		if err != nil {
			t.Fatalf("read pruned keys: %v", err)
		}
		for qr.Next() {
			var kid, kh string
			if err := qr.Scan(&kid, &kh); err != nil {
				t.Fatalf("scan: %v", err)
			}
			pruned[kid] = kh
		}
		qr.Close()
		if len(pruned) != 2 {
			t.Fatalf("prune fixture: %d key(s), want 2", len(pruned))
		}
	}
	if err := store.PruneTenant(ctx, pool, f.tenant, store.APIStamp(admin.ID, "mx-prune-req")); err != nil {
		t.Fatalf("PruneTenant: %v (SQLSTATE %s)", err, mcSQLState(err))
	}
	for i, kid := range f.keys {
		add(fmt.Sprintf("tenant_pruned_key%d", i+1), kid, pruned[kid], false)
	}
	add("never_minted", uuid.NewString(), mcHash("mx-never-minted"), false)
	nowConsumer := mcConsumer(ctx, t, pool, tid, "expires-now", nil)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// expires_at = now() needs the transaction's now(), which both functions
	// read.
	id, h = mcKey(ctx, t, tx, nowConsumer, "mx-expires-now", map[string]string{"expires_at": "now()"})
	add("expires_at_now", id, h, false)

	t.Logf("CTX_M165_OVERLAY=%q, %d rows", os.Getenv("CTX_M165_OVERLAY"), len(rows))
	valid := make(map[string]bool, len(rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		res := mcAuth(ctx, t, tx, r.hash, nil, true)
		if res.valid != r.valid {
			t.Fatalf("fixture %s: ctx_model_auth(hash, NULL, true).is_valid = %v, want %v (%s)", r.name, res.valid, r.valid, res.text)
		}
		valid[r.id] = res.valid
		ids = append(ids, r.id)
	}
	deadList, err := stringListTx(ctx, tx, `SELECT d::text FROM ctx_model_auth_live($1::uuid[]) AS d`, ids)
	if err != nil {
		t.Fatalf("ctx_model_auth_live over %d ids: %v (SQLSTATE %s)", len(ids), err, mcSQLState(err))
	}
	dead := make(map[string]bool, len(deadList))
	for _, d := range deadList {
		if _, ok := valid[d]; !ok || dead[d] {
			t.Errorf("ctx_model_auth_live returned %s, not an input id or twice", d)
		}
		dead[d] = true
	}
	divergent := 0
	for _, r := range rows {
		if valid[r.id] == dead[r.id] {
			divergent++
			t.Errorf("%s (key %s): ctx_model_auth is_valid = %v, ctx_model_auth_live dead = %v — want is_valid ⇔ not dead",
				r.name, r.id, valid[r.id], dead[r.id])
		}
	}
	t.Logf("measured: %d row(s), %d dead, %d divergent, one ctx_model_auth_live call", len(rows), len(deadList), divergent)

	t.Run("empty_and_null_input", func(t *testing.T) {
		for _, arg := range []any{[]string{}, nil} {
			got, err := stringListTx(ctx, tx, `SELECT d::text FROM ctx_model_auth_live($1::uuid[]) AS d`, arg)
			if err != nil || len(got) != 0 {
				t.Errorf("ctx_model_auth_live(%#v) = %v, %v — want no row", arg, got, err)
			}
		}
	})
	t.Run("stable", func(t *testing.T) {
		var vol string
		if err := tx.QueryRow(ctx, `SELECT provolatile::text FROM pg_proc WHERE proname = 'ctx_model_auth_live'`).Scan(&vol); err != nil || vol != "s" {
			t.Errorf("ctx_model_auth_live provolatile %q (err %v), want s (STABLE)", vol, err)
		}
	})
}

// mxListener is a dedicated connection LISTENing on ctx_model_access_write;
// every payload it reads is kept in raw for the hash census.
type mxListener struct {
	conn *pgxpool.Conn
	raw  []string
}

func mxListen(ctx context.Context, t *testing.T, pool *pgxpool.Pool) *mxListener {
	t.Helper()
	lc, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	t.Cleanup(lc.Release)
	if _, err := lc.Exec(ctx, "LISTEN "+m165Channel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	return &mxListener{conn: lc}
}

// next waits up to d for the next notification; ok is false on a timeout
// (the connection stays usable: pgx ends the wait with a read deadline).
func (l *mxListener) next(ctx context.Context, t *testing.T, d time.Duration) (string, bool) {
	t.Helper()
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	n, err := l.conn.Conn().WaitForNotification(wctx)
	if err != nil {
		if wctx.Err() != nil && ctx.Err() == nil {
			return "", false
		}
		t.Fatalf("wait for notification: %v", err)
	}
	if n.Channel != m165Channel {
		t.Fatalf("notification on %q, want %q", n.Channel, m165Channel)
	}
	l.raw = append(l.raw, n.Payload)
	return n.Payload, true
}

// mxFirstWait bounds the wait for the first notification of a write; a
// commit's notifications are queued together and sent when it commits, so
// mxQuiet after the last one means the write sent no more.
const (
	mxFirstWait = 5 * time.Second
	mxQuiet     = 500 * time.Millisecond
)

// collect returns the canonical payloads that arrive within first and then
// until quiet passes without one.
func (l *mxListener) collect(ctx context.Context, t *testing.T, first time.Duration) []string {
	t.Helper()
	var out []string
	wait := first
	for {
		p, ok := l.next(ctx, t, wait)
		if !ok {
			return out
		}
		out = append(out, mxCanon(t, p))
		wait = mxQuiet
	}
}

// mxCanon renders a payload with sorted keys and expires_at normalised to
// UTC RFC 3339 — parsed with time.RFC3339Nano, a parse failure fails the test.
func mxCanon(t *testing.T, raw string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("payload %q: %v", raw, err)
	}
	if s, ok := m["expires_at"].(string); ok {
		at, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Errorf("payload %q: expires_at %q does not parse as RFC 3339: %v", raw, s, err)
		} else {
			m["expires_at"] = at.UTC().Format(time.RFC3339Nano)
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// mxWant renders an expected payload the way mxCanon renders a received one;
// an expires_at of type time.Time is formatted, nil stays JSON null.
func mxWant(t *testing.T, kv ...any) string {
	t.Helper()
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		v := kv[i+1]
		if at, ok := v.(time.Time); ok {
			v = at.UTC().Format(time.RFC3339Nano)
		}
		m[kv[i].(string)] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func mxSorted(in []string) string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return strings.Join(out, "\n  ")
}

func TestModelAccessNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mxSetup(t)
	l := mxListen(ctx, t, pool)
	t.Logf("CTX_M165_OVERLAY=%q", os.Getenv("CTX_M165_OVERLAY"))

	const keysT, consT, tenT, scT = "context_model_keys", "context_model_consumers", "context_tenants", "context_tenant_scopes"
	expiry := time.Date(2099, 1, 2, 3, 4, 5, 123456000, time.UTC)
	newExpiry := time.Date(2031, 2, 3, 4, 5, 6, 789000000, time.UTC)

	// Fixture, all of it written after the LISTEN: inserts fire nothing.
	tid, scope := mcTenant(ctx, t, pool, "mx-nt", "active")
	other, _ := mcTenant(ctx, t, pool, "mx-nt-other", "active")
	cons := mcConsumer(ctx, t, pool, tid, "mx-nt-c", map[string]string{"tenant_scope": "'" + scope + "'"})
	revokeKey, revokeHash := mcKey(ctx, t, pool, cons, "mx-nt-revoke", map[string]string{"expires_at": "timestamptz '2099-01-02 03:04:05.123456+00'"})
	expiryKey, expiryHash := mcKey(ctx, t, pool, cons, "mx-nt-expiry", nil)
	deleteKey, deleteHash := mcKey(ctx, t, pool, cons, "mx-nt-delete", nil)
	deact := mcConsumer(ctx, t, pool, tid, "mx-nt-deact", nil)
	live1, live1Hash := mcKey(ctx, t, pool, deact, "mx-nt-live1", nil)
	live2, live2Hash := mcKey(ctx, t, pool, deact, "mx-nt-live2", map[string]string{"expires_at": "timestamptz '2099-01-02 03:04:05.123456+00'"})
	_, deadHash := mcKey(ctx, t, pool, deact, "mx-nt-already-revoked", map[string]string{
		"created_at": "now() - interval '1 hour'", "revoked_at": "now() - interval '1 minute'"})
	gone := mcConsumer(ctx, t, pool, tid, "mx-nt-gone", nil)
	susp, _ := mcTenant(ctx, t, pool, "mx-nt-susp", "active")
	dropped, droppedScope := mcTenant(ctx, t, pool, "mx-nt-drop", "active")
	kaExec(ctx, t, pool, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('mx-nt:moved', $1::uuid), ('mx-nt:deleted', $1::uuid)`, tid)
	hashes := []string{revokeHash, expiryHash, deleteHash, live1Hash, live2Hash, deadHash}

	t.Run("inserts_are_silent", func(t *testing.T) {
		if got := l.collect(ctx, t, mxQuiet); len(got) != 0 {
			t.Errorf("%d notification(s) for the fixture inserts, want 0:\n  %s", len(got), mxSorted(got))
		}
	})

	for _, c := range []struct {
		name, sql string
		args      []any
		want      []string
	}{
		{"key_revoke", `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, []any{revokeKey},
			[]string{mxWant(t, "table", keysT, "op", "UPDATE", "id", revokeKey, "consumer_id", cons, "expires_at", expiry)}},
		{"key_expiry_changed", `UPDATE context_model_keys SET expires_at = $2::timestamptz WHERE id = $1::uuid`, []any{expiryKey, newExpiry},
			[]string{mxWant(t, "table", keysT, "op", "UPDATE", "id", expiryKey, "consumer_id", cons, "expires_at", newExpiry)}},
		{"key_delete", `DELETE FROM context_model_keys WHERE id = $1::uuid`, []any{deleteKey},
			[]string{mxWant(t, "table", keysT, "op", "DELETE", "id", deleteKey, "consumer_id", cons)}},
		// The 161 trigger revokes the consumer's two live keys (not the one
		// already revoked): one key notification each.
		{"consumer_deactivate", `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, []any{deact},
			[]string{
				mxWant(t, "table", consT, "op", "UPDATE", "id", deact, "tenant_id", tid, "scope", nil),
				mxWant(t, "table", keysT, "op", "UPDATE", "id", live1, "consumer_id", deact, "expires_at", nil),
				mxWant(t, "table", keysT, "op", "UPDATE", "id", live2, "consumer_id", deact, "expires_at", expiry),
			}},
		{"consumer_delete", `DELETE FROM context_model_consumers WHERE id = $1::uuid`, []any{gone},
			[]string{mxWant(t, "table", consT, "op", "DELETE", "id", gone, "tenant_id", tid, "scope", nil)}},
		{"tenant_suspend", `UPDATE context_tenants SET status = 'suspended' WHERE id = $1::uuid`, []any{susp},
			[]string{mxWant(t, "table", tenT, "op", "UPDATE", "id", susp)}},
		// The tenant's scope goes with it (ON DELETE CASCADE): its row
		// trigger fires too.
		{"tenant_delete", `DELETE FROM context_tenants WHERE id = $1::uuid`, []any{dropped},
			[]string{
				mxWant(t, "table", tenT, "op", "DELETE", "id", dropped),
				mxWant(t, "table", scT, "op", "DELETE", "scope", droppedScope, "tenant_id", dropped),
			}},
		{"scope_moved", `UPDATE context_tenant_scopes SET tenant_id = $1::uuid WHERE scope = 'mx-nt:moved'`, []any{other},
			[]string{mxWant(t, "table", scT, "op", "UPDATE", "scope", "mx-nt:moved", "tenant_id", other)}},
		{"scope_delete", `DELETE FROM context_tenant_scopes WHERE scope = 'mx-nt:deleted'`, nil,
			[]string{mxWant(t, "table", scT, "op", "DELETE", "scope", "mx-nt:deleted", "tenant_id", tid)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			kaExec(ctx, t, pool, c.sql, c.args...)
			got := l.collect(ctx, t, mxFirstWait)
			t.Logf("%s: %d notification(s)", c.name, len(got))
			if mxSorted(got) != mxSorted(c.want) {
				t.Errorf("%s: notifications\n  %s\nwant exactly\n  %s", c.name, mxSorted(got), mxSorted(c.want))
			}
		})
	}

	t.Run("unlisted_columns_are_silent", func(t *testing.T) {
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET label = 'mx-nt-c-renamed' WHERE id = $1::uuid`, cons)
		kaExec(ctx, t, pool, `UPDATE context_tenants SET display_name = 'mx-nt renamed' WHERE id = $1::uuid`, tid)
		kaExec(ctx, t, pool, `UPDATE context_model_keys SET key_hint = 'ctxm_…zzzz', last_used_at = now() WHERE id = $1::uuid`, expiryKey)
		if got := l.collect(ctx, t, mxQuiet); len(got) != 0 {
			t.Errorf("%d notification(s) for writes of unlisted columns, want 0:\n  %s", len(got), mxSorted(got))
		}
	})

	// store.UpdateTenant writes status = COALESCE(NULLIF($2, ''), status) with
	// every display_name change, so the trigger's column list fires; the
	// function drops an UPDATE whose status stayed. Red with
	// CTX_M165_OVERLAY=tenant-status-always.
	t.Run("tenant_status_unchanged_is_silent", func(t *testing.T) {
		if _, err := store.UpdateTenant(ctx, pool, tid, "", "mx-nt renamed through the store"); err != nil {
			t.Fatalf("UpdateTenant: %v", err)
		}
		kaExec(ctx, t, pool, `UPDATE context_tenants SET status = status WHERE id = $1::uuid`, tid)
		if got := l.collect(ctx, t, mxQuiet); len(got) != 0 {
			t.Errorf("%d notification(s) for tenant updates that keep the status, want 0:\n  %s", len(got), mxSorted(got))
		}
	})

	t.Run("no_hash_on_the_wire", func(t *testing.T) {
		if len(l.raw) == 0 {
			t.Fatal("no payload received — nothing to census")
		}
		for _, raw := range l.raw {
			for _, bad := range append([]string{"key_hash", "key_hint", "ctxm_"}, hashes...) {
				if strings.Contains(raw, bad) {
					t.Errorf("payload %s carries %q", raw, bad)
				}
			}
		}
		t.Logf("measured: %d payload(s) censused, none with a hash, a hint or the key prefix", len(l.raw))
	})
}

func TestModelKeyUsageFlushNoNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const n = 1000
	ctx := context.Background()
	pool := mxSetup(t)
	t.Logf("CTX_M165_OVERLAY=%q", os.Getenv("CTX_M165_OVERLAY"))
	tid, _ := mcTenant(ctx, t, pool, "mx-flush", "active")
	cons := mcConsumer(ctx, t, pool, tid, "mx-flush-c", nil)
	ids, err := stringList(ctx, pool, `INSERT INTO context_model_keys (consumer_id, key_hash, key_hint)
		SELECT $1::uuid, h, 'ctxm_…' || right(h, 4)
		  FROM (SELECT encode(sha256(convert_to('ctxm_mx-flush-' || g, 'UTF8')), 'hex') AS h
		          FROM generate_series(1, $2::int) g) s
		RETURNING id::text`, cons, n)
	if err != nil || len(ids) != n {
		t.Fatalf("mint %d keys: %d, %v", n, len(ids), err)
	}
	l := mxListen(ctx, t, pool)

	u := store.NewModelKeyUsage(pool)
	for _, id := range ids {
		u.Touch(id)
	}
	if err := u.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := mkCount(ctx, t, pool, `SELECT count(*) FROM context_model_keys WHERE id = ANY($1::uuid[]) AND last_used_at IS NOT NULL`, ids); got != n {
		t.Fatalf("last_used_at set on %d of %d keys after the flush", got, n)
	}
	// Window: the flush is one autocommit statement, Flush returns after its
	// commit, and a commit's notifications are sent together — mxQuiet
	// (500 ms) covers their delivery to the listener.
	storm := len(l.collect(ctx, t, mxQuiet))

	// Positive control: notifications arrive in commit order, so whatever the
	// flush sent would arrive before the revoke's.
	kaExec(ctx, t, pool, `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, ids[0])
	revoked := false
	for !revoked {
		p, ok := l.next(ctx, t, mxFirstWait)
		if !ok {
			t.Fatalf("the revoke of %s sent no notification within %s (after %d from the flush) — the channel is dead, the gate proves nothing", ids[0], mxFirstWait, storm)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(p), &m); err != nil {
			t.Fatalf("payload %q: %v", p, err)
		}
		if m["id"] == ids[0] && m["op"] == "UPDATE" && m["table"] == "context_model_keys" {
			revoked = true
			continue
		}
		storm++
	}
	t.Logf("measured: %d notification(s) on %s for a flush of %d keys (window %s), then the revoke", storm, m165Channel, n, mxQuiet)
	if storm != 0 {
		t.Errorf("the last_used_at flush of %d keys sent %d notification(s), want 0", n, storm)
	}
}

// m165Catalog renders what 165 owns: its two functions, the triggers of the
// four tables it hangs them on, and its version row.
func m165Catalog(ctx context.Context, t *testing.T, q func(context.Context, string) ([]string, error)) string {
	t.Helper()
	var parts []string
	for _, sql := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('notify_model_access_write', 'ctx_model_auth_live')
		  ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid IN ('context_model_keys'::regclass, 'context_model_consumers'::regclass,
		                                         'context_tenants'::regclass, 'context_tenant_scopes'::regclass)
		  ORDER BY tgrelid::regclass::text, tgname`,
		`SELECT 'migration: ' || version FROM _migrations WHERE version = 165`,
	} {
		lines, err := q(ctx, sql)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// m165WayBack is the rollback the header of 165 and docs/operations.md
// describe, in their order: the triggers before their function, the version
// row last, so a later upgrade applies 165 again.
const m165WayBack = `
DROP TRIGGER IF EXISTS trg_model_keys_access_notify ON context_model_keys;
DROP TRIGGER IF EXISTS trg_model_consumers_access_notify ON context_model_consumers;
DROP TRIGGER IF EXISTS trg_tenants_access_notify ON context_tenants;
DROP TRIGGER IF EXISTS trg_tenant_scopes_access_notify ON context_tenant_scopes;
DROP FUNCTION notify_model_access_write(), ctx_model_auth_live(UUID[]);
DELETE FROM _migrations WHERE version = 165;`

// m165Rows counts the rows of the tables 165 touches plus the audit trail.
const m165Rows = `SELECT (SELECT count(*) FROM context_model_keys) || ' ' || (SELECT count(*) FROM context_model_consumers)
	|| ' ' || (SELECT count(*) FROM context_tenants) || ' ' || (SELECT count(*) FROM context_tenant_scopes)
	|| ' ' || (SELECT count(*) FROM context_settings_audit)`

func TestMigration165Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m165Version-1)
	onPool := func(ctx context.Context, sql string) ([]string, error) { return stringList(ctx, pool, sql) }
	at164 := m165Catalog(ctx, t, onPool)
	auth164, err := stringList(ctx, pool, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p WHERE p.proname = 'ctx_model_auth'`)
	if err != nil || len(auth164) != 1 {
		t.Fatalf("ctx_model_auth at 164: %d definition(s), %v", len(auth164), err)
	}
	rows164, err := stringList(ctx, pool, m165Rows)
	if err != nil {
		t.Fatalf("rows at 164: %v", err)
	}
	m165Apply(ctx, t, pool)

	t.Run("applied_without_seed", func(t *testing.T) {
		if n := mkCount(ctx, t, pool, `SELECT count(*) FROM _migrations WHERE version = 165`); n != 1 {
			t.Errorf("_migrations holds %d row(s) of 165, want 1", n)
		}
		if got, err := stringList(ctx, pool, m165Rows); err != nil || strings.Join(got, "") != strings.Join(rows164, "") {
			t.Errorf("row counts (keys consumers tenants scopes audit) %v after 165, %v at 164 (err %v) — 165 writes no row", got, rows164, err)
		}
	})
	t.Run("ctx_model_auth_untouched", func(t *testing.T) {
		got, err := stringList(ctx, pool, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p WHERE p.proname = 'ctx_model_auth'`)
		if err != nil || strings.Join(got, "") != auth164[0] {
			t.Errorf("165 changed ctx_model_auth (err %v)", err)
		}
	})
	// The way back of the header and docs/operations.md, in a transaction
	// that is rolled back.
	t.Run("way_back_is_clean", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, m165WayBack); err != nil {
			t.Fatalf("way back: %v (SQLSTATE %s)", err, mcSQLState(err))
		}
		after := m165Catalog(ctx, t, func(ctx context.Context, sql string) ([]string, error) { return stringListTx(ctx, tx, sql) })
		if after != at164 {
			t.Errorf("catalog after the way back differs from 164:\n--- 164\n%s\n--- way back\n%s", at164, after)
		}
	})
	t.Run("second_run_keeps_the_catalog", func(t *testing.T) {
		body, _ := m165Body(t)
		if body == "" {
			t.Fatal("165 is not in the tree")
		}
		before := m165Catalog(ctx, t, onPool)
		kaExecInTx(ctx, t, pool, body)
		if after := m165Catalog(ctx, t, onPool); after != before {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", before, after)
		}
		t.Logf("catalog (identical across both runs, %d lines; %d at 164)", strings.Count(before, "\n")+1, strings.Count(at164, "\n")+1)
	})
}

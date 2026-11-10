//go:build integration

// Issue #199: the embed backfill that runs inside POST /api/query picked any
// pending block in the database, in no stated order. A query therefore spent
// its inline allowance on blocks its caller cannot read, and reached the
// block the caller had just written only after everything ahead of it.
//
// Every probe drives the real request path: the blocks are written through
// POST /api/store by two keys of two tenants (nothing is inserted by hand, so
// the rows carry whatever the write path gives them), and the pick is observed
// through POST /api/query, by which block has a vector afterwards.
//
// RED before the fix, whatever the row order: with the cap off and the arm on
// the request drains the foreign tenant's backlog as well
// (foreign_backlog_untouched, read_scope_beyond_home). The probes with a cap
// (caller_block_first, newest_of_the_caller_first, arm_off_caller_first, the
// two unservable_* ones) are red before the fix only when the unordered
// LIMIT 1 returns another row than the one they want, and that follows the
// physical row order: POST /api/store updates content_times after it has
// answered, so the order of the heap need not be the order of the writes.
// arm_off_still_drains_everything passes before and after: it pins that the
// path stays the embedder of last resort.
//
// The probes from the review of #214 follow the eight above. They pin what
// the pick does at the cap and across scopes when the newest pending blocks
// have no eligible embed backend (unservable_at_the_cap and the five after
// it), and what it does with the background arm off, where this loop is the
// only embedder and has to work the old end of the queue as well (the
// arm_off_* probes at the end). arm_off_scoped_pick_runs_once counts the pick
// statements of one request and passes before and after.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryBackfillCallerScope -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	bfsForeignScope = "bfsforeign"
	bfsForeignTitle = "bfs foreign tenant block"
	bfsForeignNewer = "bfs foreign tenant block, written last"
	bfsCallerShared = "bfs caller block in its second read scope"
	bfsCallerSecret = "bfs caller block no embed backend may receive"
	bfsCallerTitle  = "bfs caller block"
	bfsCallerNewer  = "bfs caller block, written later"
	bfsCallerOldest = "bfs caller block, written first of three"
	bfsCallerMiddle = "bfs caller block, written second of three"
	bfsCallerLatest = "bfs caller block, written third of three"
)

// bfsPickCounter is a pgx QueryTracer that counts the pick statements of the
// inline backfill (the one statement of the query path that reads
// "embedding IS NULL" with "LIMIT 1"), and among them the ones bound to the
// caller's read scopes. Same seam as guardQueryCounter.
type bfsPickCounter struct {
	picks  atomic.Int64
	scoped atomic.Int64
}

func (c *bfsPickCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "embedding IS NULL") && strings.Contains(data.SQL, "LIMIT 1") {
		c.picks.Add(1)
		if strings.Contains(data.SQL, "AND scope = ANY(") {
			c.scoped.Add(1)
		}
	}
	return ctx
}

func (c *bfsPickCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// bfsFixture is one database with two tenants, one key each, and the two
// production handlers the probes drive.
type bfsFixture struct {
	pool       *pgxpool.Pool
	dsn        string
	storeH     *StoreHandler
	reg        *blocktype.Registry
	backend    *bw2Backend
	callerCtx  context.Context
	foreignCtx context.Context
	// embedTrust is the trust of the one embed backend the query handler
	// gets, querySens the sensitivity the query request names ("" = the
	// setting). The unservable_* probes lower both.
	embedTrust backends.Trust
	querySens  string
	// floor is the query handler's pool.scope_sensitivity_floor (nil = none).
	floor config.ScopeFloor
	// privateEmbed, when set, adds a second embed backend of that trust that
	// only the scope `private` can use.
	privateEmbed backends.Trust
	// queryPool, when set by countPicks, is the pool the query handler gets
	// in place of pool: the same database behind a statement counter.
	queryPool *pgxpool.Pool
}

func bfsSetup(t *testing.T) *bfsFixture {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	key := func(label, homeScope string, allowed []string, tenantID string) context.Context {
		t.Helper()
		_, plain, err := store.CreateApiKey(ctx, pool, label, homeScope, allowed, tenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", label, err)
		}
		return context.WithValue(ctx, authResultKey, ar)
	}

	foreignTenant := mintGateTenant(t, pool, "bfs-foreign")
	mintGateMapScope(t, pool, bfsForeignScope, foreignTenant)

	f := &bfsFixture{
		pool: pool,
		dsn:  dsn,
		reg:  reg,
		// Default sensitivity public: the production default (credentials)
		// would bring the credentials detector into probes about the pick.
		storeH: NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{
			Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		}}, reg),
		backend: bw2NewBackend(t),
		// The caller reads a second scope next to its home scope.
		callerCtx:  key("bfs-caller", "private", []string{"shared"}, store.DefaultTenantID),
		foreignCtx: key("bfs-foreign", bfsForeignScope, nil, foreignTenant),
		embedTrust: backends.TrustFull,
	}

	// Premise, asserted rather than assumed: the caller cannot read the
	// foreign scope. Otherwise every probe below would be about nothing.
	if ar := AuthResultFromContext(f.callerCtx); contains(ar.ReadScopes, bfsForeignScope) {
		t.Fatalf("caller read scopes %v include the foreign scope", ar.ReadScopes)
	}
	// And it reads `shared` next to its home scope (read_scope_beyond_home).
	if ar := AuthResultFromContext(f.callerCtx); ar.HomeScope != "private" || !contains(ar.ReadScopes, "shared") {
		t.Fatalf("caller home scope %q, read scopes %v: want home private and shared readable", ar.HomeScope, ar.ReadScopes)
	}
	return f
}

// write stores one block through POST /api/store as the given key, in the
// key's home scope and with the default sensitivity.
func (f *bfsFixture) write(t *testing.T, keyCtx context.Context, title string) {
	t.Helper()
	f.writeAs(t, keyCtx, title, "", "")
}

// writeAs is write with a named scope and sensitivity ("" leaves the field
// out of the request).
func (f *bfsFixture) writeAs(t *testing.T, keyCtx context.Context, title, scope, sens string) {
	t.Helper()
	body := map[string]any{
		"category": "test", "title": title,
		"content": "body of " + title + ", stored through the write path",
	}
	if scope != "" {
		body["scope"] = scope
	}
	if sens != "" {
		body["sensitivity"] = sens
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(keyCtx)
	rec := httptest.NewRecorder()
	f.storeH.HandleStore(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("store %q: status %d, body %s", title, rec.Code, rec.Body.String())
	}
	// Premise of every probe: the write path leaves the block pending.
	if f.embedded(t, title) {
		t.Fatalf("block %q carries a vector right after the write; the probes need it pending", title)
	}
}

// query runs one POST /api/query as the caller, with the given inline cap and
// background-arm interval (0 = arm off).
func (f *bfsFixture) query(t *testing.T, syncCap int, interval time.Duration) {
	t.Helper()
	cfg := bw2Config()
	cfg.EmbedBackfill.SyncCap = syncCap
	cfg.EmbedBackfill.Interval = interval
	cfg.Pool.ScopeSensitivityFloor = f.floor
	// bw2Pool with the embed backend's trust as a parameter.
	rows := []backends.Backend{
		{ID: "e", Name: "bfs-embed", Host: f.backend.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: f.embedTrust, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
		{ID: "t", Name: "bfs-chat", Host: f.backend.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-chat", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleTranslate}},
	}
	if f.privateEmbed != "" {
		rows = append(rows, backends.Backend{ID: "p", Name: "bfs-embed-private", Scope: "private",
			Host: f.backend.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: f.privateEmbed, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}})
	}
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest(rows)
	dbPool := f.pool
	if f.queryPool != nil {
		dbPool = f.queryPool
	}
	h := NewQueryHandler(dbPool, config.NewStore(cfg), bpool, nil, f.reg, snapshotTestAdmitter(t))

	body := map[string]any{"query": bw2GoldenQuery, "synthesize": false}
	if f.querySens != "" {
		body["sensitivity"] = f.querySens
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(string(raw))).WithContext(f.callerCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// countPicks puts the query handler on a second pool of the same database
// that counts the backfill's pick statements.
func (f *bfsFixture) countPicks(t *testing.T) *bfsPickCounter {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(f.dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	ctr := &bfsPickCounter{}
	cfg.ConnConfig.Tracer = ctr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("counting pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f.queryPool = pool
	return ctr
}

// embedded reports whether the block with this title has a vector.
func (f *bfsFixture) embedded(t *testing.T, title string) bool {
	t.Helper()
	var present bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT embedding IS NOT NULL FROM context_blocks WHERE title = $1`, title).Scan(&present); err != nil {
		t.Fatalf("read embedding state of %q: %v", title, err)
	}
	return present
}

func TestQueryBackfillCallerScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	const armOn = 15 * time.Second

	// One inline pick, with a pending block of another tenant on either side
	// of the caller's write: neither the oldest nor the newest pending block
	// is the caller's, so the pick is the caller's by scope and not by order.
	t.Run("caller_block_first", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.write(t, f.foreignCtx, bfsForeignNewer)

		f.query(t, 1, armOn)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the caller's own fresh block is still pending after its query")
		}
		if f.embedded(t, bfsForeignTitle) || f.embedded(t, bfsForeignNewer) {
			t.Error("the query embedded a block in a scope its caller cannot read")
		}
	})

	// No cap at all: while the background arm runs, the request still leaves
	// the other tenant's backlog to that arm.
	t.Run("foreign_backlog_untouched", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)

		f.query(t, 0, armOn)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the caller's own block is still pending after an uncapped query")
		}
		if f.embedded(t, bfsForeignTitle) {
			t.Error("an uncapped query drained a scope its caller cannot read while the background arm runs")
		}
	})

	// The scopes are the ones the search reads, not the home scope alone: a
	// block the caller stored to its second read scope is picked, and the
	// other tenant's stays pending. No cap, so the row order plays no part.
	t.Run("read_scope_beyond_home", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.writeAs(t, f.callerCtx, bfsCallerShared, "shared", "")

		f.query(t, 0, armOn)

		if !f.embedded(t, bfsCallerShared) {
			t.Error("the caller's block in its second read scope is still pending after its query")
		}
		if f.embedded(t, bfsForeignTitle) {
			t.Error("an uncapped query drained a scope its caller cannot read while the background arm runs")
		}
	})

	// Two pending blocks of the caller, one pick: the later write goes first.
	t.Run("newest_of_the_caller_first", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.write(t, f.callerCtx, bfsCallerNewer)

		f.query(t, 1, armOn)

		if !f.embedded(t, bfsCallerNewer) {
			t.Error("the caller's latest write is still pending after its query")
		}
		if f.embedded(t, bfsCallerTitle) {
			t.Error("the single pick went to the caller's older block")
		}
	})

	// Arm off: the path is the only embedder, but the caller's block still
	// goes ahead of the other tenant's, the older and the newer one alike.
	t.Run("arm_off_caller_first", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.write(t, f.foreignCtx, bfsForeignNewer)

		f.query(t, 1, 0)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("arm off: the caller's own fresh block is still pending after its query")
		}
		if f.embedded(t, bfsForeignTitle) || f.embedded(t, bfsForeignNewer) {
			t.Error("arm off: the single pick went to a foreign scope ahead of the caller's block")
		}
	})

	// Arm off and no cap: nothing else would ever embed the foreign block, so
	// the request does, after the caller's own.
	t.Run("arm_off_still_drains_everything", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)

		f.query(t, 0, 0)

		if !f.embedded(t, bfsCallerTitle) || !f.embedded(t, bfsForeignTitle) {
			t.Errorf("arm off, no cap: caller embedded=%v, foreign embedded=%v, want both (the query path is the only embedder)",
				f.embedded(t, bfsCallerTitle), f.embedded(t, bfsForeignTitle))
		}
	})

	// The caller's newest pending block has no eligible embed backend (its
	// sensitivity is above the backend's trust). It gets no memo, so it stays
	// the newest: the request sets it aside and embeds the block behind it.
	t.Run("unservable_newest_does_not_hold_the_line", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.writeAs(t, f.callerCtx, bfsCallerSecret, "", string(backends.SensCredentials))

		f.query(t, 2, armOn)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the block behind an unservable one is still pending after the query")
		}
		if f.embedded(t, bfsCallerSecret) {
			t.Error("a credentials block was embedded on a no-credentials backend")
		}
	})

	// Arm off: an unservable block in the caller's scopes does not keep the
	// request from the fallback, which is all the other tenant's block has.
	t.Run("arm_off_unservable_does_not_stop_the_fallback", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.writeAs(t, f.callerCtx, bfsCallerSecret, "", string(backends.SensCredentials))

		f.query(t, 0, 0)

		if !f.embedded(t, bfsForeignTitle) {
			t.Error("arm off: the other tenant's block is still pending behind the caller's unservable one")
		}
		if f.embedded(t, bfsCallerSecret) {
			t.Error("a credentials block was embedded on a no-credentials backend")
		}
	})

	// The probes below come from the review of #214.

	// As many unservable newest blocks as the cap allows picks, one servable
	// block behind them, one query. A block no embed backend may receive must
	// cost the request no sync_cap slot, or the servable block is never
	// reached: the unservable ones get no memo and stay the newest.
	t.Run("unservable_at_the_cap", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			syncCap    int
			unservable int
			interval   time.Duration
		}{
			{"cap_2_arm_on", 2, 2, armOn},
			{"cap_4_arm_on", 4, 4, armOn},
			{"cap_2_arm_off", 2, 2, 0},
			{"cap_1_arm_on", 1, 1, armOn},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := bfsSetup(t)
				f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
				f.write(t, f.callerCtx, bfsCallerTitle)
				secrets := make([]string, tc.unservable)
				for i := range secrets {
					secrets[i] = fmt.Sprintf("%s, number %d", bfsCallerSecret, i+1)
					f.writeAs(t, f.callerCtx, secrets[i], "", string(backends.SensCredentials))
				}

				f.query(t, tc.syncCap, tc.interval)

				if !f.embedded(t, bfsCallerTitle) {
					t.Errorf("sync_cap %d: the servable block behind %d unservable ones is still pending after the query",
						tc.syncCap, tc.unservable)
				}
				for _, title := range secrets {
					if f.embedded(t, title) {
						t.Errorf("credentials block %q was embedded on a no-credentials backend", title)
					}
				}
			})
		}
	})

	// Arm off, the caller's newest blocks unservable up to the cap: the
	// request still reaches the caller's servable block and, behind the
	// caller's scopes, the other tenant's block, which nothing else embeds.
	t.Run("arm_off_unservable_at_the_cap_reaches_the_other_tenant", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		for i := 1; i <= 4; i++ {
			f.writeAs(t, f.callerCtx, fmt.Sprintf("%s, number %d", bfsCallerSecret, i), "", string(backends.SensCredentials))
		}

		f.query(t, 4, 0)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("arm off: the caller's servable block is still pending behind four unservable ones")
		}
		if !f.embedded(t, bfsForeignTitle) {
			t.Error("arm off: the other tenant's block is still pending behind the caller's four unservable ones")
		}
	})

	// The cap still bounds what a request embeds: two unservable newest
	// blocks, three servable ones behind them, sync_cap 2. The two newest
	// servable blocks are embedded and the third waits.
	t.Run("cap_bounds_the_embeds_behind_unservable_blocks", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.write(t, f.callerCtx, bfsCallerOldest)
		f.write(t, f.callerCtx, bfsCallerMiddle)
		f.write(t, f.callerCtx, bfsCallerLatest)
		for i := 1; i <= 2; i++ {
			f.writeAs(t, f.callerCtx, fmt.Sprintf("%s, number %d", bfsCallerSecret, i), "", string(backends.SensCredentials))
		}

		f.query(t, 2, armOn)

		if !f.embedded(t, bfsCallerLatest) || !f.embedded(t, bfsCallerMiddle) {
			t.Errorf("sync_cap 2 behind two unservable blocks: latest embedded=%v, middle embedded=%v, want both",
				f.embedded(t, bfsCallerLatest), f.embedded(t, bfsCallerMiddle))
		}
		if f.embedded(t, bfsCallerOldest) {
			t.Error("sync_cap 2: the request embedded a third block")
		}
	})

	// Whether a block is servable depends on its scope as well as on its
	// sensitivity: `private` has an embed backend of its own that takes
	// credentials, `shared` has the no-credentials one only. The newest
	// block is a credentials block in `shared`; the single pick goes to the
	// older credentials block in `private`.
	t.Run("class_is_per_scope", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.privateEmbed = backends.TrustFull
		f.writeAs(t, f.callerCtx, bfsCallerTitle, "", string(backends.SensCredentials))
		f.writeAs(t, f.callerCtx, bfsCallerShared, "shared", string(backends.SensCredentials))

		f.query(t, 1, armOn)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the credentials block in the scope with a full-trust embed backend is still pending behind an unservable one in another scope")
		}
		if f.embedded(t, bfsCallerShared) {
			t.Error("a credentials block in `shared` was embedded although only the no-credentials backend serves that scope")
		}
	})

	// A sensitivity floor lifts every block of a scope: with `shared` floored
	// at credentials on a no-credentials backend, its public blocks are
	// unservable. They are the newest; the single pick goes to the older
	// public block in `private`.
	t.Run("floor_lifts_a_whole_scope", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.floor = config.ScopeFloor{"shared": backends.SensCredentials}
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.writeAs(t, f.callerCtx, bfsCallerShared, "shared", "")
		f.writeAs(t, f.callerCtx, bfsCallerShared+", the second one", "shared", "")

		f.query(t, 1, armOn)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the block in the scope without a floor is still pending behind the floored scope's blocks")
		}
		if f.embedded(t, bfsCallerShared) || f.embedded(t, bfsCallerShared+", the second one") {
			t.Error("a block of a scope floored at credentials was embedded on a no-credentials backend")
		}
	})

	// No cap and no budget: an unservable backlog must not cost the request
	// one pick per block. Fifty unservable blocks in front of one servable
	// one; the request makes the pick that returns the servable block and the
	// one that comes back empty.
	t.Run("uncapped_unservable_backlog_costs_no_pick", func(t *testing.T) {
		f := bfsSetup(t)
		f.embedTrust, f.querySens = backends.TrustNoCredentials, string(backends.SensInternal)
		f.write(t, f.callerCtx, bfsCallerTitle)
		for i := 1; i <= 50; i++ {
			f.writeAs(t, f.callerCtx, fmt.Sprintf("%s, number %d", bfsCallerSecret, i), "", string(backends.SensCredentials))
		}
		ctr := f.countPicks(t)

		f.query(t, 0, armOn)

		if !f.embedded(t, bfsCallerTitle) {
			t.Error("the servable block behind fifty unservable ones is still pending after an uncapped query")
		}
		if got := ctr.picks.Load(); got < 1 || got > 2 {
			t.Errorf("the request ran %d pick statements, want 2 at most (one per embedded block and the empty one), however many blocks are unservable", got)
		}
	})

	// Arm off, nothing pending in the caller's scopes: the request drains the
	// other tenant's blocks and asks for the caller's scopes once, not once
	// per block it embeds (fbd1324f). Passes before and after the review fix.
	t.Run("arm_off_scoped_pick_runs_once", func(t *testing.T) {
		f := bfsSetup(t)
		titles := []string{bfsForeignTitle, bfsForeignTitle + ", the second one", bfsForeignNewer}
		for _, title := range titles {
			f.write(t, f.foreignCtx, title)
		}
		ctr := f.countPicks(t)

		f.query(t, 0, 0)

		for _, title := range titles {
			if !f.embedded(t, title) {
				t.Errorf("arm off, no cap: %q is still pending (the query path is the only embedder)", title)
			}
		}
		if got := ctr.scoped.Load(); got != 1 {
			t.Errorf("the request ran the pick bound to the caller's scopes %d times, want once (%d pick statements in all)",
				got, ctr.picks.Load())
		}
	})

	// Arm off, one write and one query per round, sync_cap 1: this loop is
	// the only embedder, so the one pick has to reach the old block. Newest
	// first embeds the fresh write of every round and never the old block.
	t.Run("arm_off_steady_writes_reach_the_old_block", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.callerCtx, bfsCallerOldest)

		reached := 0
		for round := 1; round <= 3; round++ {
			f.write(t, f.callerCtx, fmt.Sprintf("%s, round %d", bfsCallerNewer, round))
			f.query(t, 1, 0)
			if reached == 0 && f.embedded(t, bfsCallerOldest) {
				reached = round
			}
		}

		if reached != 1 {
			t.Errorf("arm off, sync_cap 1, one write per query: the old block got its vector in round %d, want round 1 (0 = in none of three rounds)", reached)
		}
	})

	// Arm off, sync_cap 2, three pending blocks of the caller: one pick for
	// the newest (the caller's fresh write) and one for the oldest (nothing
	// else works the old end). The one in the middle waits.
	t.Run("arm_off_takes_both_ends", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.callerCtx, bfsCallerOldest)
		f.write(t, f.callerCtx, bfsCallerMiddle)
		f.write(t, f.callerCtx, bfsCallerLatest)

		f.query(t, 2, 0)

		if !f.embedded(t, bfsCallerLatest) {
			t.Error("arm off, sync_cap 2: the caller's latest write is still pending")
		}
		if !f.embedded(t, bfsCallerOldest) {
			t.Error("arm off, sync_cap 2: the oldest pending block is still pending")
		}
		if f.embedded(t, bfsCallerMiddle) {
			t.Error("arm off, sync_cap 2: the middle block was embedded, so one end went without")
		}
	})

	// Arm off: a caller that writes as many blocks per query as sync_cap
	// allows never drains its own scopes, so the old end must not be bound to
	// them, or the other tenant's block waits for good.
	t.Run("arm_off_steady_caller_does_not_starve_the_other_tenant", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)
		f.write(t, f.callerCtx, bfsCallerNewer)

		f.query(t, 2, 0)

		if !f.embedded(t, bfsForeignTitle) {
			t.Error("arm off, sync_cap 2: the other tenant's older block is still pending behind the caller's two fresh writes")
		}
		if !f.embedded(t, bfsCallerNewer) {
			t.Error("arm off, sync_cap 2: the caller's latest write is still pending")
		}
	})

	// Arm off, nothing pending in the caller's scopes, one pick: it takes the
	// other tenant's oldest block, not its newest.
	t.Run("arm_off_old_end_takes_the_oldest", func(t *testing.T) {
		f := bfsSetup(t)
		f.write(t, f.foreignCtx, bfsForeignTitle)
		f.write(t, f.foreignCtx, bfsForeignNewer)

		f.query(t, 1, 0)

		if !f.embedded(t, bfsForeignTitle) {
			t.Error("arm off: the oldest pending block is still pending after the single pick")
		}
		if f.embedded(t, bfsForeignNewer) {
			t.Error("arm off: the single pick of the old end went to the newest block")
		}
	})
}

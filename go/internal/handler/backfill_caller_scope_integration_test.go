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
//	go test -tags=integration ./internal/handler/ -run TestQueryBackfillCallerScope -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
)

// bfsFixture is one database with two tenants, one key each, and the two
// production handlers the probes drive.
type bfsFixture struct {
	pool       *pgxpool.Pool
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
}

func bfsSetup(t *testing.T) *bfsFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
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
	// bw2Pool with the embed backend's trust as a parameter.
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "bfs-embed", Host: f.backend.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: f.embedTrust, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
		{ID: "t", Name: "bfs-chat", Host: f.backend.srv.URL, Protocol: backends.ProtocolOllama,
			Model: "test-chat", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleTranslate}},
	})
	h := NewQueryHandler(f.pool, config.NewStore(cfg), bpool, nil, f.reg, snapshotTestAdmitter(t))

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
}

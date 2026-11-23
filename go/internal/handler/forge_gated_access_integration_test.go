//go:build integration

// Issue #509, the gated list of a remote resolution's push gate
// (forge-conflict-list gated:true) and the local resolution that opens the
// gate (forge-conflict-resolve direction local on a gated row), through the
// production HandleManage chain against PG18.
//
// TestTheGatedListAndTheGateOpeningRefuseOutsiders: a tenant-admin of another
// tenant, the own admin naming another project of the tenant with the gated
// row's block, and a member key of the owning tenant get the answer every
// other call of theirs gets — the uniform 404 "Project not found", the 404 of
// a block without a mapping in the named project, the tier's 403, each with
// the body of the same call on an absent target — and leave the
// gate closed, the mappings, blocks, cursors and the audit table as they
// were. The own admin's local resolution then opens the gate (the control
// that the refused payloads were the opening ones).
//
// TestTheGatedListPagesOnceThroughWhileGatesOpen: one project with 100,000
// mappings, 300 of them gated with a live block in the project's scope and
// 20 gated with an archived block. The gated list is paged through by its
// next_cursor while two goroutines open gated rows by local resolutions:
// no block appears twice, every row gated at the start and not opened by
// the end appears exactly once, every listed row was gated with a live
// block at the start, and every opened row is open and audited once.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestTheGatedListAndTheGateOpeningRefuseOutsiders|TestTheGatedListPagesOnceThroughWhileGatesOpen' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"maps"
	"math/rand"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// gatedOf reads whether blockID's mapping is behind the push gate.
func (e *fcmEnv) gatedOf(t *testing.T, blockID string) bool {
	t.Helper()
	var gated bool
	if err := e.pool.QueryRow(context.Background(),
		`SELECT synced_at = 'infinity'::timestamptz FROM context_project_sync_map WHERE block_id = $1::uuid`, blockID).Scan(&gated); err != nil {
		t.Fatalf("gate of %s: %v", blockID, err)
	}
	return gated
}

func TestTheGatedListAndTheGateOpeningRefuseOutsiders(t *testing.T) {
	e := newFCMEnv(t)
	proj, other := e.project(t, "gated-own"), e.project(t, "gated-other")
	id, otherID := e.flag(t, proj, 5), e.flag(t, other, 5)
	for _, c := range []struct{ project, block string }{{proj.ID, id}, {other.ID, otherID}} {
		if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": c.project, "block_id": c.block, "direction": "remote"}); code != http.StatusOK || !resolveResult(t, raw).Changed {
			t.Fatalf("resolve remote of %s: %d %s", c.block, code, raw)
		}
	}
	if !e.gatedOf(t, id) || !e.gatedOf(t, otherID) {
		t.Fatal("a remote resolution left no push gate")
	}
	gp, _ := e.list(t, proj, map[string]any{"gated": true})
	if len(gp.Conflicts) != 1 || gp.Conflicts[0].BlockID != id || !gp.Conflicts[0].Gated {
		t.Fatalf("the own admin's gated list: %+v, want the gated row", gp.Conflicts)
	}
	flag := gp.Conflicts[0].Flag
	before := e.snapshot(t, []string{id, otherID}, []string{proj.ID, other.ID})
	auditBefore := len(e.auditRows(t, id))

	// The answer a caller gets for the same action on an absent target.
	refusal := func(t *testing.T, name string, code int, raw string, wantCode int, wantRaw string) {
		t.Helper()
		if code != wantCode || raw != wantRaw {
			t.Errorf("%s: %d %s, want %d %s — the answer of the same call on an absent target", name, code, raw, wantCode, wantRaw)
		}
	}
	listGated := map[string]any{"project_id": proj.ID, "gated": true}
	openGate := map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"}
	openGateExpect := map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local", "expect": flag}

	t.Run("ForeignTenantAdmin", func(t *testing.T) {
		_, _, absent := e.do(t, e.h, e.adminB, "forge-conflict-list", map[string]any{"project_id": uuid.NewString(), "gated": true})
		if !strings.Contains(absent, `"Project not found"`) {
			t.Fatalf("an absent project: %s, want the uniform 404", absent)
		}
		for name, c := range map[string]struct {
			action string
			data   map[string]any
		}{
			"list gated":            {"forge-conflict-list", listGated},
			"resolve local":         {"forge-conflict-resolve", openGate},
			"resolve local, expect": {"forge-conflict-resolve", openGateExpect},
		} {
			code, _, raw := e.do(t, e.h, e.adminB, c.action, c.data)
			refusal(t, name, code, raw, http.StatusNotFound, absent)
		}
	})

	t.Run("OtherProjectOfTheTenant", func(t *testing.T) {
		// The other project's gated list carries its own row, never the
		// named project's.
		if p, raw := e.list(t, other, map[string]any{"gated": true}); len(p.Conflicts) != 1 || p.Conflicts[0].BlockID != otherID || strings.Contains(raw, id) {
			t.Errorf("the other project's gated list: %s — want its own gated row alone", raw)
		}
		_, _, absent := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": other.ID, "block_id": uuid.NewString(), "direction": "local"})
		if !strings.Contains(absent, forgeNoMappingError) {
			t.Fatalf("a block without a mapping: %s, want the 404 %q", absent, forgeNoMappingError)
		}
		for name, data := range map[string]map[string]any{
			"resolve local":         {"project_id": other.ID, "block_id": id, "direction": "local"},
			"resolve local, expect": {"project_id": other.ID, "block_id": id, "direction": "local", "expect": flag},
		} {
			code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", data)
			refusal(t, name, code, raw, http.StatusNotFound, absent)
		}
	})

	t.Run("MemberOfTheOwningTenant", func(t *testing.T) {
		_, _, absent := e.do(t, e.h, e.memberA, "forge-conflict-list", map[string]any{"project_id": uuid.NewString()})
		if !strings.Contains(absent, "admin key required") {
			t.Fatalf("a member on an absent project: %s, want the tier's 403", absent)
		}
		for name, c := range map[string]struct {
			action string
			data   map[string]any
		}{
			"list gated":            {"forge-conflict-list", listGated},
			"resolve local":         {"forge-conflict-resolve", openGate},
			"resolve local, expect": {"forge-conflict-resolve", openGateExpect},
		} {
			code, _, raw := e.do(t, e.h, e.memberA, c.action, c.data)
			refusal(t, name, code, raw, http.StatusForbidden, absent)
		}
	})

	if !e.gatedOf(t, id) || !e.gatedOf(t, otherID) {
		t.Errorf("a refused call opened a gate: own %v, other %v (want both gated)", e.gatedOf(t, id), e.gatedOf(t, otherID))
	}
	if n := len(e.auditRows(t, id)); n != auditBefore {
		t.Errorf("forge_sync audit rows of the gated row: %d, want %d (the remote resolution alone)", n, auditBefore)
	}
	if after := e.snapshot(t, []string{id, otherID}, []string{proj.ID, other.ID}); after != before {
		t.Errorf("a refused call changed state:\nbefore %s\nafter  %s", before, after)
	}

	// The control: the own admin's same call opens the gate.
	code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", openGateExpect)
	if r := resolveResult(t, raw); code != http.StatusOK || !r.Changed || r.Basis != "gate" {
		t.Fatalf("the own admin's local resolution: %d %s, want the gate opened", code, raw)
	}
	if e.gatedOf(t, id) || !e.gatedOf(t, otherID) {
		t.Errorf("after the own admin's resolution: own gated %v, other gated %v — want the own row open, the other's gated", e.gatedOf(t, id), e.gatedOf(t, otherID))
	}
}

func TestTheGatedListPagesOnceThroughWhileGatesOpen(t *testing.T) {
	const (
		mappings      = 100_000 // of the one project
		gated         = 300     // gated rows with a live block in the project's scope
		archivedGated = 20      // gated rows whose block is archived: never listed
		pageLimit     = 25
		openers       = 2
		openCap       = gated / 2 // at most this many gates open during the paging
	)
	e := newFCMEnv(t)
	ctx := context.Background()
	proj := e.project(t, "gated-pages")
	src := e.flag(t, proj, 1)
	if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": src, "direction": "remote"}); code != http.StatusOK {
		t.Fatalf("resolve remote: %d %s", code, raw)
	}
	// The other mappings in bulk: issue blocks like the pulled one, each
	// mapped, not gated.
	if _, err := e.pool.Exec(ctx, `
		WITH nb AS (
		  INSERT INTO context_blocks (category, title, content, metadata, scope, type_name, workflow_status)
		  SELECT b.category, '#' || g || ' bulk', 'bulk body ' || g, b.metadata, b.scope, b.type_name, b.workflow_status
		    FROM context_blocks b, generate_series(2, $3::int) g WHERE b.id = $1::uuid
		  RETURNING id, title)
		INSERT INTO context_project_sync_map (project_id, entity_kind, forge_id, block_id, base_hash, forge_updated_at, synced_at, metadata)
		SELECT $2::uuid, 'issue', split_part(substr(nb.title, 2), ' ', 1)::bigint + 10, nb.id, md5(nb.id::text), now(), now(), '{"base_fields":{"body":"x"}}'::jsonb FROM nb`,
		src, proj.ID, mappings); err != nil {
		t.Fatalf("bulk mappings: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE context_project_sync_map SET synced_at = 'infinity'
		 WHERE block_id IN (SELECT block_id FROM context_project_sync_map
		                     WHERE project_id = $1::uuid AND synced_at <> 'infinity'::timestamptz
		                     ORDER BY random() LIMIT $2)`, proj.ID, gated-1+archivedGated); err != nil {
		t.Fatalf("gate rows: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `
		UPDATE context_blocks SET is_archived = true
		 WHERE id IN (SELECT block_id FROM context_project_sync_map
		               WHERE project_id = $1::uuid AND synced_at = 'infinity'::timestamptz AND block_id <> $2::uuid
		               ORDER BY random() LIMIT $3)`, proj.ID, src, archivedGated); err != nil {
		t.Fatalf("archive gated blocks: %v", err)
	}
	if _, err := e.pool.Exec(ctx, `VACUUM ANALYZE context_project_sync_map`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM context_project_sync_map WHERE project_id = $1::uuid`, proj.ID).Scan(&total); err != nil || total != mappings {
		t.Fatalf("mappings of the project: %d %v, want %d", total, err, mappings)
	}
	initial := map[string]bool{}
	rows, err := e.pool.Query(ctx, `
		SELECT m.block_id::text FROM context_project_sync_map m JOIN context_blocks b ON b.id = m.block_id
		 WHERE m.project_id = $1::uuid AND m.synced_at = 'infinity'::timestamptz AND b.scope = $2 AND NOT b.is_archived`, proj.ID, proj.Scope)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		initial[id] = true
	}
	rows.Close()
	if len(initial) != gated {
		t.Fatalf("gated rows with a live block at the start: %d, want %d", len(initial), gated)
	}

	// Two openers resolve gated rows local, each row once, in random order.
	queue := make([]string, 0, len(initial))
	for id := range initial {
		queue = append(queue, id)
	}
	rand.Shuffle(len(queue), func(i, j int) { queue[i], queue[j] = queue[j], queue[i] })
	var (
		next     atomic.Int64
		mu       sync.Mutex
		opened   = map[string]bool{}
		failures []string
		stop     = make(chan struct{})
		started  = make(chan struct{}, openers)
		wg       sync.WaitGroup
	)
	for o := 0; o < openers; o++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			first := true
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := next.Add(1) - 1
				if i >= openCap {
					return
				}
				id := queue[i]
				code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"})
				var ans struct {
					Result struct {
						Changed bool   `json:"changed"`
						Basis   string `json:"basis"`
					} `json:"result"`
				}
				decoded := json.Unmarshal([]byte(raw), &ans) == nil // no Fatal off the test's goroutine
				mu.Lock()
				if code == http.StatusOK && decoded && ans.Result.Changed && ans.Result.Basis == "gate" {
					opened[id] = true
				} else {
					failures = append(failures, id+": "+raw)
				}
				mu.Unlock()
				if first {
					started <- struct{}{}
					first = false
				}
				time.Sleep(time.Duration(rng.Intn(4000)) * time.Microsecond)
			}
		}(int64(o) + time.Now().UnixNano())
	}
	for o := 0; o < openers; o++ {
		<-started
	}

	seen := map[string]int{}
	var durations []time.Duration
	var notGatedOnPage int
	after := ""
	for {
		extra := map[string]any{"gated": true, "limit": pageLimit}
		if after != "" {
			extra["after"] = after
		}
		t0 := time.Now()
		p, _ := e.list(t, proj, extra)
		durations = append(durations, time.Since(t0))
		for _, c := range p.Conflicts {
			seen[c.BlockID]++
			if !c.Gated {
				notGatedOnPage++
			}
		}
		if p.NextCursor == nil {
			break
		}
		after = *p.NextCursor
	}
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	for _, f := range failures {
		t.Errorf("a local resolution of a gated row did not open it: %s", f)
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("block %s listed %d times", id, n)
		}
		if !initial[id] {
			t.Errorf("block %s listed, but it was not gated with a live block at the start", id)
		}
	}
	if notGatedOnPage > 0 {
		t.Errorf("%d listed row(s) without gated:true", notGatedOnPage)
	}
	var missing []string
	for id := range initial {
		if !opened[id] && seen[id] != 1 {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d row(s) gated through the whole paging were not listed exactly once: %v", len(missing), missing)
	}

	// Every opened row is open and audited once; the others stay gated.
	var stillGated, audited int
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) FROM context_project_sync_map m JOIN context_blocks b ON b.id = m.block_id
		 WHERE m.project_id = $1::uuid AND m.synced_at = 'infinity'::timestamptz AND b.scope = $2 AND NOT b.is_archived`, proj.ID, proj.Scope).Scan(&stillGated); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `
		SELECT count(*) FROM context_settings_audit
		 WHERE entity_type = 'forge_sync' AND action = 'resolve-local' AND entity_key = ANY($1::text[])`, slices.Collect(maps.Keys(opened))).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if stillGated != gated-len(opened) || audited != len(opened) {
		t.Errorf("after the paging: %d gated (want %d), %d resolve-local audit rows (want %d, one per opened row)", stillGated, gated-len(opened), audited, len(opened))
	}
	if len(opened) == 0 {
		t.Error("no gate opened during the paging: the paging ran alone")
	}

	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	var openedListed int // opened after the paging passed them; the rest went before it got there
	for id := range opened {
		if seen[id] == 1 {
			openedListed++
		}
	}
	t.Logf("gated list at %d mappings, %d gated (+%d archived): %d pages of limit %d, %d rows listed, %d gates opened by %d openers (%d of them listed before they opened); page median %v, max %v",
		mappings, gated, archivedGated, len(durations), pageLimit, len(seen), len(opened), openers, openedListed, sorted[len(sorted)/2], sorted[len(sorted)-1])
}

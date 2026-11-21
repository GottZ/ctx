//go:build integration

package forge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// #474 point 2, the pull end to end: the real client against a forge whose
// Link header uses another RFC 8288 spelling than GitHub's, the real Applier,
// the real cursor. Five issues share one updated_at and the forge serves two
// per page: a pull that stops after the first page commits since = that
// updated_at, and every later run fetches the same first page again — three
// issues never arrive while each run reports success.
func TestSyncPullReachesEveryPage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var issues []fakeIssue
	for i := int64(1); i <= 5; i++ {
		issues = append(issues, fakeIssue{Number: i, Updated: t0})
	}

	run := func(t *testing.T, slug string, link func(string) []string) (store.ProjectRow, SyncStatus) {
		t.Helper()
		gh := newFakeGitHub(t, 2, link, issues...)
		proj := seedIFProject(t, pool, slug)
		// The fake forge listens on loopback, which validateForge refuses as
		// an api_base on every API path; the row is pointed at it directly.
		if _, err := pool.Exec(context.Background(), `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`,
			proj.ID, `{"kind":"github","owner":"o","repo":"r","api_base":"`+gh.srv.URL+`"}`); err != nil {
			t.Fatal(err)
		}
		if p, err := store.GetProjectByID(context.Background(), pool, proj.ID); err != nil {
			t.Fatal(err)
		} else {
			proj = *p
		}
		m := mgr(pool, okTenant, okPolicy, &githubClient{http: gh.srv.Client()}, applierWithRegistry(pool).ApplyIssues)
		if _, err := m.StartSync(context.Background(), proj, false); err != nil {
			t.Fatalf("start: %v", err)
		}
		st := waitDone(t, m, proj.ID)
		got, err := store.GetProjectByID(context.Background(), pool, proj.ID)
		if err != nil {
			t.Fatal(err)
		}
		return *got, st
	}
	mapped := func(t *testing.T, projectID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_project_sync_map WHERE project_id = $1::uuid AND entity_kind = 'issue'`,
			projectID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("unquoted-rel", func(t *testing.T) {
		got, st := run(t, "p474unq", func(n string) []string { return []string{`<` + n + `>; rel=next`} })
		if n := mapped(t, got.ID); n != 5 || st.Aborted || st.Pages != 3 {
			t.Fatalf("one run mapped %d of 5 issues over %d pages (aborted %v, last_error %q); want 5 over 3", n, st.Pages, st.Aborted, st.LastError)
		}
	})

	// An unparseable header naming next ends the run as an error the
	// operator reads in last_error. The page that carries it fails as a
	// whole, as a next page on another origin does (#459): nothing is
	// applied and the cursor stays, so the next run asks again.
	t.Run("unparseable", func(t *testing.T) {
		got, st := run(t, "p474bad", func(n string) []string { return []string{`<` + n + `; rel="next"`} })
		if !st.Aborted || got.SyncStatus != "error" {
			t.Fatalf("run aborted %v, sync_status %q; want an aborted run in error", st.Aborted, got.SyncStatus)
		}
		if got.LastError == nil || !strings.Contains(*got.LastError, "Link header") {
			t.Fatalf("last_error = %v; want it to name the Link header", got.LastError)
		}
		if n := mapped(t, got.ID); n != 0 {
			t.Fatalf("mapped %d issues; want none — the page carrying the header fails as a whole", n)
		}
		if _, since, _ := readCursor(got.SyncCursor); !since.IsZero() {
			t.Fatalf("cursor advanced to %v on a failed page", since)
		}
	})
}

// #474 point 1, end to end: a project whose forge.api_base is http on a host
// that redirects to https. The API refuses such an api_base on create, PATCH
// and provision (handler.validateForge) — a row written past it, the only way
// one exists, is set here directly. The run fails on the first request and
// last_error tells the operator what to set.
func TestSyncAPIBaseUpgradeNamesTheFix(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	rig := newUpgradeRig(t, 301)
	proj := seedIFProject(t, pool, "p474upg")
	if _, err := pool.Exec(context.Background(), `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`,
		proj.ID, `{"kind":"github","owner":"o","repo":"r","api_base":"http://`+upgradeHost+`/api/v3"}`); err != nil {
		t.Fatal(err)
	}
	p, err := store.GetProjectByID(context.Background(), pool, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := mgr(pool, okTenant, okPolicy, &githubClient{http: rig.client, token: "pat-474"}, applierWithRegistry(pool).ApplyIssues)
	if _, err := m.StartSync(context.Background(), *p, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, proj.ID)
	got, _ := store.GetProjectByID(context.Background(), pool, proj.ID)
	if seen := rig.tlsSeen(); seen != "" {
		t.Fatalf("the sync followed the upgrade: %s", seen)
	}
	if !st.Aborted || got.LastError == nil || !strings.Contains(*got.LastError, "forge.api_base") || !strings.Contains(*got.LastError, "https") {
		t.Fatalf("aborted %v, last_error %v; want the run to fail with the advice to set forge.api_base to https", st.Aborted, got.LastError)
	}
	if strings.Contains(*got.LastError, upgradeHost) || strings.Contains(*got.LastError, "pat-474") {
		t.Fatalf("last_error %q carries the host or the token", *got.LastError)
	}
}

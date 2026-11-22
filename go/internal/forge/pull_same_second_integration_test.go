//go:build integration

// Issue #491 (review of the first round, F1): a pull must not write an older
// forge version over a newer one another instance pulled.
//
// GitHub's updated_at has whole seconds, so two versions written within one
// second carry the same stamp, and the row's forge_updated_at cannot order a
// fetch of one against a pull of the other. A pull that finds the base moved
// since its read (another instance pulled in between) and its fetch at the
// same stamp as the row flags the mapping instead of writing — unless it
// fetched the very version the other instance pulled. Before, the pull wrote
// its fetch whenever it was not strictly older: the older version replaced
// the newer one in the block and the base, unflagged, and once the cursor of
// the instance that pulled the newer version answered 304, a local edit was
// pushed over the newer version on the forge.
//
// A fetch strictly older than the row's observation writes nothing, also
// when the other instance's pull committed before this pull read the mapping.
//
// The windows are opened by a hold on the applier's pool (the other pull runs
// while this pull's write transaction waits at its BEGIN) or by a page fetched
// before the other pull ran; the writes are the production ones, the pages
// come from the fake forge through the production client.
//
//	go test -tags=integration ./internal/forge/ -run 'TestPullOfASameSecondFetch|TestPullOfAnOlderFetchReadAfterANewerPull' -count=1 -v
package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i491Page lists what the forge holds since since, through the production
// client, with an If-None-Match of etag.
func (e i491Env) i491Page(t *testing.T, since time.Time, etag string) IssuePage {
	t.Helper()
	page, err := e.gh.client().ListIssuesSince(context.Background(), e.gh.repo(), since, etag)
	if err != nil {
		t.Fatalf("listing since %s: %v", since, err)
	}
	return page
}

// applyPage applies a fetched page on the environment's pool (another
// instance's pull).
func (e i491Env) applyPage(t *testing.T, issues []IssueRemote) ApplyResult {
	t.Helper()
	r, err := e.applier(e.pool).ApplyIssues(context.Background(), e.proj, issues)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	return r
}

func (e i491Env) body(t *testing.T) string {
	t.Helper()
	return getBlock(t, e.pool, e.proj.Scope, e.id(t)).Content
}

func TestPullOfASameSecondFetch(t *testing.T) {
	const first, second = "body edited on the forge, first", "body edited on the forge, second"

	// A fetched the first version; B fetches the second one of the same
	// second, pulls it and commits its cursor; then A writes.
	t.Run("older fetch, the newer version pulled meanwhile", func(t *testing.T) {
		e := i491Setup(t, "i491ssolder", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.gh.setETags()
		since := e.gh.issue(t, 5).Updated
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		pageA := e.i491Page(t, since, "")
		var pageB IssuePage
		res := e.pullHeldOnPage(t, pageA.Issues, func() {
			e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = second })
			pageB = e.i491Page(t, since, "")
			if r := e.applyPage(t, pageB.Issues); r.Applied != 1 {
				t.Fatalf("B's pull of the second version: %+v", r)
			}
		})
		if res.Applied != 0 || res.Conflicts != 1 {
			t.Errorf("A's pull of the first version: %+v, want nothing written and the mapping flagged", res)
		}
		if got := e.body(t); got != second {
			t.Errorf("the block holds %q, want B's version %q kept", got, second)
		}
		if _, conflict := e.mapping(t); !conflict {
			t.Fatalf("the mapping is not flagged")
		}
		// B's cursor (since the stamp of its page, its ETag) answers 304: no
		// later pull reads the issue again. A local edit is not pushed over
		// the version the forge holds.
		if _, err := e.gh.client().ListIssuesSince(context.Background(), e.gh.repo(), e.gh.issue(t, 5).Updated, pageB.ETag); !errors.Is(err, ErrNotModified) {
			t.Fatalf("precondition: B's cursor answers %v, want 304", err)
		}
		e.edit(t, store.IssueUpdate{Content: str("body edited locally")})
		landed := len(e.gh.landed())
		e.pushOn(t, e.pool)
		if n := len(e.gh.landed()) - landed; n != 0 {
			t.Errorf("the push sent %d PATCH(es) over the flagged mapping", n)
		}
		if it := e.gh.issue(t, 5); it.Body != second {
			t.Errorf("the forge holds %q, want %q", it.Body, second)
		}
	})

	// The other order: B fetched the second version, A the first one; A's
	// pull commits while B waits, then B writes.
	t.Run("newer fetch, the older version pulled meanwhile", func(t *testing.T) {
		e := i491Setup(t, "i491ssnewer", "", fakeIssue{Title: "Crane", Body: "forge body"})
		since := e.gh.issue(t, 5).Updated
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		pageA := e.i491Page(t, since, "")
		e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = second })
		pageB := e.i491Page(t, since, "")
		res := e.pullHeldOnPage(t, pageB.Issues, func() {
			if r := e.applyPage(t, pageA.Issues); r.Applied != 1 {
				t.Fatalf("A's pull of the first version: %+v", r)
			}
		})
		if res.Applied != 0 || res.Conflicts != 1 {
			t.Errorf("B's pull of the second version: %+v, want nothing written and the mapping flagged", res)
		}
		if got := e.body(t); got != first {
			t.Errorf("the block holds %q, want A's version %q kept", got, first)
		}
		// The next pull lists the forge again (no ETag of a listing that
		// still matches): the block is at the base, the forge ahead of it.
		if r := e.pullOn(t, e.pool); r.Applied != 1 || r.Conflicts != 0 {
			t.Errorf("the next pull: %+v, want the second version applied", r)
		}
		if got := e.body(t); got != second {
			t.Errorf("after the next pull the block holds %q, want %q", got, second)
		}
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs landed %d PATCH(es), want 0", n)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
		}
	})

	// Both instances fetched the same version: nothing to do for the second.
	t.Run("the same version pulled meanwhile", func(t *testing.T) {
		e := i491Setup(t, "i491sssame", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		page := e.fetch(t)
		res := e.pullHeldOnPage(t, page, func() {
			if r := e.applyPage(t, page); r.Applied != 1 {
				t.Fatalf("B's pull: %+v", r)
			}
		})
		if res.Applied != 0 || res.Conflicts != 0 {
			t.Errorf("A's pull of the version B pulled: %+v, want nothing", res)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || e.body(t) != first {
			t.Errorf("mapping flagged %v, base is the forge's %v, block %q", conflict, base == e.forgeH(t), e.body(t))
		}
	})

	// One instance, run after run: the forge changed twice within a second,
	// and a run pulled between the two writes. The next run fetches the
	// second version at the stamp the row records; nothing moved the base
	// since its read, so it is the next version and is pulled.
	t.Run("the next version of one second, run after run", func(t *testing.T) {
		e := i491Setup(t, "i491ssnext", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		if r := e.pullOn(t, e.pool); r.Applied != 1 {
			t.Fatalf("the first run: %+v", r)
		}
		e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = second })
		if r := e.pullOn(t, e.pool); r.Applied != 1 || r.Conflicts != 0 {
			t.Errorf("the next run: %+v, want the second version applied", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) || e.body(t) != second {
			t.Errorf("mapping flagged %v, base is the forge's %v, block %q", conflict, base == e.forgeH(t), e.body(t))
		}
	})

	// A mapping another instance flagged after it moved the base: the same
	// stamp is decided as above, but the flag is not counted a second time.
	t.Run("older fetch, the mapping flagged since", func(t *testing.T) {
		e := i491Setup(t, "i491ssflagged", "", fakeIssue{Title: "Crane", Body: "forge body"})
		since := e.gh.issue(t, 5).Updated
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		pageA := e.i491Page(t, since, "")
		res := e.pullHeldOnPage(t, pageA.Issues, func() {
			e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = second })
			if r := e.applyPage(t, e.i491Page(t, since, "").Issues); r.Applied != 1 {
				t.Fatalf("B's pull of the second version: %+v", r)
			}
			// Both sides move on and a third pull flags it; the flag does
			// not touch forge_updated_at.
			e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
			e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, third" })
			if r := e.applyPage(t, e.i491Page(t, since, "").Issues); r.Conflicts != 1 {
				t.Fatalf("the third pull: %+v, want the conflict flagged", r)
			}
		})
		if res.Applied != 0 || res.Conflicts != 0 {
			t.Errorf("A's pull of the first version: %+v, want nothing written and the conflict not counted again", res)
		}
		if _, conflict := e.mapping(t); !conflict {
			t.Errorf("the flag is gone")
		}
	})

	// KNOWN LIMIT, an open point (issue #491): another instance pulls the
	// version this pull fetched and pushes a local edit over it before this
	// pull writes. The push leaves forge_updated_at at the stamp of the pulled
	// version, so the fetch carries the stamp the row records while the base
	// moved: it is flagged like an unordered same-second version, although
	// block, base and forge all hold the pushed edit. Such a flag stays — the
	// noop cell writes nothing, no surface resolves a flag, and a later local
	// edit is not pushed; a remote edit clears it only while the block has not
	// moved. Telling this case apart needs a marker of which writer set the
	// base. This pins today's behaviour.
	t.Run("known limit: the same version pulled and pushed over meanwhile", func(t *testing.T) {
		e := i491Setup(t, "i491sspushed", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		page := e.fetch(t)
		res := e.pullHeldOnPage(t, page, func() {
			if r := e.applyPage(t, page); r.Applied != 1 {
				t.Fatalf("B's pull: %+v", r)
			}
			e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
			if r := e.pushOn(t, e.pool); r.Pushed != 1 {
				t.Fatalf("B's push: %+v", r)
			}
		})
		if res.Applied != 0 || res.Conflicts != 1 {
			t.Errorf("A's pull: %+v — the pin expects the mapping flagged (the known limit)", res)
		}
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs landed %d PATCH(es), want 0", n)
		}
		b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
		_, conflict := e.mapping(t)
		if !conflict || b.Title != "#5 Crane, edited locally" || e.gh.issue(t, 5).Title != "Crane, edited locally" {
			t.Errorf("after three runs: flagged %v, block %q, forge %q — the pin expects the flag kept while both sides hold the pushed edit", conflict, b.Title, e.gh.issue(t, 5).Title)
		}
	})

	// KNOWN LIMIT, an open point (issue #491): another instance pulls the
	// second version of the same second after this pull fetched the first one
	// and before this pull reads the mapping. The read decides on the second
	// version as the base, nothing moves the base before the write, and the
	// row's stamp is the fetch's: that reads exactly like the run-after-run
	// case above, and the first version is written over the second one. This
	// pins today's behaviour; a fix needs an order the forge's clock does not
	// give (a version counter of the forge, or the moment of the fetch against
	// the moment of the other pull's write).
	t.Run("known limit: the same stamp pulled before this pull read the mapping", func(t *testing.T) {
		e := i491Setup(t, "i491sslimit", "", fakeIssue{Title: "Crane", Body: "forge body"})
		since := e.gh.issue(t, 5).Updated
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = first })
		stale := e.i491Page(t, since, "")
		e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = second })
		if r := e.pullOn(t, e.pool); r.Applied != 1 {
			t.Fatalf("other instance's pull of the second version: %+v", r)
		}
		if r := e.applyPage(t, stale.Issues); r.Applied != 1 || r.Conflicts != 0 || e.body(t) != first {
			t.Errorf("the pull of the first version: %+v, block %q — the pin expects the first version written (the known limit); if this changed, update the limit in docs/architecture.md", r, e.body(t))
		}
	})
}

// TestPullOfAnOlderFetchReadAfterANewerPull: this pull fetched the issue; the
// forge changes again and another instance pulls the newer version before
// this pull reads the mapping. Its read then decides on the newer base (the
// block equals it, the fetch differs: forge-ahead) and the base does not move
// before its write — but the row records a newer observation than the fetch,
// and nothing is written.
func TestPullOfAnOlderFetchReadAfterANewerPull(t *testing.T) {
	e := i491Setup(t, "i491olderread", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, first" })
	stale := e.fetch(t)
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, second" })
	if r := e.pullOn(t, e.pool); r.Applied != 1 {
		t.Fatalf("other instance's pull of the newer version: %+v", r)
	}
	if r := e.applyPage(t, stale); r.Applied != 0 || r.Conflicts != 0 {
		t.Errorf("the pull of the older fetch: %+v, want nothing written", r)
	}
	if got := e.body(t); got != "body edited on the forge, second" {
		t.Errorf("the block holds %q, want the newer version kept", got)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
}

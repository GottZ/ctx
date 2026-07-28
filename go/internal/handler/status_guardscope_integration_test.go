//go:build integration

// Integration probes for RC-1 wave S6 — the WIRE half of the /guard live
// channel (design/05 §4.5, Weg A).
//
// The defect these pin: the /guard LIST filters on `b.scope = ANY(ReadScopes)`
// (store.GuardList, blocks.go) while the guard_review COUNTER counts the home
// scope alone (status_guard.go guardReviewForScope). Two predicates over one
// surface — so every guard decision on a block in a NON-HOME read scope moves
// the list without moving the counter, and a live channel watching only the
// counter misses it silently. guard_review_by_scope closes the gap ADDITIVELY:
// guard_review keeps its documented home-scope meaning, the new section carries
// one row per readable scope out of the SAME per-tick generation (0 queries).
//
// Every probe drives the real request path (HandleStatus + AuthResult), not the
// collector directly: the ReadScopes plumbing between auth and the section is
// exactly what the wave adds, so a collector-level probe would test past it.
//
//	go test -tags=integration ./internal/handler/ -run TestGuardReviewByScope -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// guardScopeSection is the decoded guard_review_by_scope wire section: scope →
// the same four-tuple guard_review carries. Decoded from raw JSON rather than
// from statusResponse so the probe reads what a CLIENT reads.
type guardScopeWire struct {
	GuardReview        *guardReviewStatus            `json:"guard_review"`
	GuardReviewByScope map[string]*guardReviewStatus `json:"guard_review_by_scope"`
}

func TestGuardReviewByScopeCarriesReadScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	// Three scopes: the caller's HOME, a non-home scope it may READ (the live
	// `shared`/`work` case — 24 of 4231 blocks), and a scope it may not read.
	seedGuardScope(t, pool, "gs-home", map[string]int{"needs_review": 2})
	seedGuardScope(t, pool, "gs-shared", map[string]int{"needs_review": 1, "near_duplicate": 3})
	seedGuardScope(t, pool, "gs-foreign", map[string]int{"needs_review": 7})
	// A read scope with ZERO flagged blocks must still render {0,0,0,null} —
	// present, not missing (the S1(d) posture carried into the vector).
	seedGuardScope(t, pool, "gs-empty", nil)

	col := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{},
		config.NewStore(&config.Config{}), nil, nil)
	h := NewStatusHandler(col)

	serve := func(ar *auth.AuthResult) guardScopeWire {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("HandleStatus: status %d, body %s", rec.Code, rec.Body.String())
		}
		var got guardScopeWire
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
		}
		return got
	}

	tenant := &auth.AuthResult{
		IsValid: true, HomeScope: "gs-home", TenantID: "tid-a", TenantRole: auth.RoleAdmin,
		AllowedScopes: []string{"gs-shared"},
		ReadScopes:    []string{"gs-home", "gs-shared", "gs-empty"},
	}

	t.Run("carries_every_read_scope", func(t *testing.T) {
		got := serve(tenant)
		if got.GuardReviewByScope == nil {
			t.Fatalf("guard_review_by_scope absent — the /guard live channel has no read-scope vector to compare")
		}
		for _, want := range tenant.ReadScopes {
			if _, ok := got.GuardReviewByScope[want]; !ok {
				t.Errorf("read scope %q missing from guard_review_by_scope: %v", want, guardScopeKeys(got.GuardReviewByScope))
			}
		}
		// The NON-HOME row is the whole point: it must carry that scope's real
		// counts, which the home-scope counter can never show.
		sh := got.GuardReviewByScope["gs-shared"]
		if sh == nil || sh.NeedsReview != 1 || sh.NearDuplicate != 3 {
			t.Errorf("non-home read scope gs-shared: got %+v, want {needs_review:1 near_duplicate:3}", sh)
		}
		// A readable scope with nothing flagged is PRESENT with zeros — the
		// client must be able to tell "queue is clear" from "no data" (B10).
		empty := got.GuardReviewByScope["gs-empty"]
		if empty == nil {
			t.Errorf("read scope gs-empty vanished from the vector — 0 flagged is an answer, not a gap")
		} else if empty.NeedsReview != 0 || empty.NearDuplicate != 0 || empty.PossibleDuplicate != 0 || empty.OldestUpdatedAt != nil {
			t.Errorf("gs-empty should read {0,0,0,null}, got %+v", empty)
		}
	})

	t.Run("unreadable_scope_absent", func(t *testing.T) {
		got := serve(tenant)
		if _, leaked := got.GuardReviewByScope["gs-foreign"]; leaked {
			t.Errorf("guard_review_by_scope leaked a scope outside ReadScopes: %v", guardScopeKeys(got.GuardReviewByScope))
		}
	})

	t.Run("guard_review_stays_home_scope", func(t *testing.T) {
		got := serve(tenant)
		// The additive contract: guard_review keeps counting the HOME scope, so
		// the wire comment on statusResponse.GuardReview stays true. Weg (B) —
		// widening that predicate to ReadScopes — would show 3 here.
		if got.GuardReview == nil {
			t.Fatalf("guard_review section disappeared")
		}
		if got.GuardReview.NeedsReview != 2 || got.GuardReview.NearDuplicate != 0 {
			t.Errorf("guard_review is no longer home-scope-only: got %+v, want {needs_review:2 near_duplicate:0}", got.GuardReview)
		}
		home := got.GuardReviewByScope["gs-home"]
		if home == nil || home.NeedsReview != got.GuardReview.NeedsReview ||
			home.NearDuplicate != got.GuardReview.NearDuplicate ||
			home.PossibleDuplicate != got.GuardReview.PossibleDuplicate {
			t.Errorf("the home slot of the vector disagrees with guard_review: %+v vs %+v", home, got.GuardReview)
		}
	})

	t.Run("no_read_scopes_no_section", func(t *testing.T) {
		// Fail closed: a caller without read scopes gets NO section (omitempty),
		// never an empty object that renders as "queue clear".
		got := serve(&auth.AuthResult{IsValid: true, HomeScope: "gs-home", TenantRole: auth.RoleAdmin})
		if len(got.GuardReviewByScope) != 0 {
			t.Errorf("a caller with no ReadScopes must get no vector, got %v", guardScopeKeys(got.GuardReviewByScope))
		}
	})

	t.Run("server_admin_path_carries_it_too", func(t *testing.T) {
		// handleGuardList filters on ar.ReadScopes for EVERY caller, server-admins
		// included (context_manage.go), while a server-admin's guard_review is the
		// GLOBAL total. Same predicate divergence, same answer — otherwise a
		// server-admin /guard tab reloads on foreign tenants' decisions and can
		// miss its own when two scopes' movements cancel in the global count.
		got := serve(&auth.AuthResult{
			IsValid: true, IsAdmin: true,
			ReadScopes: []string{"gs-home", "gs-shared"},
		})
		if got.GuardReviewByScope == nil {
			t.Fatalf("server-admin path carries no guard_review_by_scope")
		}
		if sh := got.GuardReviewByScope["gs-shared"]; sh == nil || sh.NearDuplicate != 3 {
			t.Errorf("server-admin vector row gs-shared wrong: %+v", sh)
		}
		if _, leaked := got.GuardReviewByScope["gs-foreign"]; leaked {
			t.Errorf("server-admin vector carries a scope outside its ReadScopes: %v", guardScopeKeys(got.GuardReviewByScope))
		}
		// guard_review stays the GLOBAL total on this path (2+1+7 = 10 needs_review).
		if got.GuardReview == nil || got.GuardReview.NeedsReview != 10 {
			t.Errorf("server-admin guard_review is no longer the global total: %+v", got.GuardReview)
		}
	})
}

func guardScopeKeys(m map[string]*guardReviewStatus) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

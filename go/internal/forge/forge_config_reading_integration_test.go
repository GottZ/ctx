//go:build integration

// Issue #474: the run reads a project's forge config the way every other
// reader of the row does (store.ReadForgeConfig). A row the API took before
// it refused a second spelling of a key carries "api_base" and "API_BASE" as
// two JSONB keys; encoding/json read the one that sorts last, so the run
// synced against one value while GET /api/project served the other, its
// credentials included. Such a row now does not sync: the run ends before its
// first request and names what to change.
//
//	go test -tags=integration ./internal/forge/ -run TestARunRefusesAForgeKeyInTwoSpellings -count=1 -v
package forge

import (
	"context"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestARunRefusesAForgeKeyInTwoSpellings(t *testing.T) {
	e := i491Setup(t, "rtw", "", fakeIssue{Title: "Crane", Body: "forge body"})
	withUser := strings.Replace(e.gh.srv.URL, "http://", "http://user:secret@", 1)
	raw := `{"kind":"github","owner":"o","repo":"r","api_base":"` + e.gh.srv.URL + `","API_BASE":"` + withUser + `"}`
	if _, err := e.pool.Exec(context.Background(), `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`, e.proj.ID, raw); err != nil {
		t.Fatal(err)
	}
	// The row as ctxd starts a run from it (JSONB orders the keys).
	proj, err := store.GetProjectByID(context.Background(), e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	m := e.i509Mgr(e.pool)
	e.gh.mu.Lock()
	e.gh.auths = nil // the setup's pull
	e.gh.mu.Unlock()
	if _, err := m.StartSync(context.Background(), *proj, false); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, m, proj.ID)
	e.gh.mu.Lock()
	n := len(e.gh.auths)
	e.gh.mu.Unlock()
	if n != 0 {
		t.Errorf("the run made %d request(s) with api_base in two spellings, want none", n)
	}
	if !strings.Contains(st.LastError, "spelling") || strings.Contains(st.LastError, "secret") {
		t.Errorf("the run's last_error %q — want it to name the key in two spellings, without its values", st.LastError)
	}
}

// TestARunRefusesAnAPIBaseThatDoesNotParse: a row written before the API
// refused it, whose api_base does not parse as a URL. The project API
// leaves such an api_base out of its answers; the run checked for
// credentials only where the URL parsed, took the value and ended every run
// in an unnamed wire error with a growing backoff. It now ends before its
// first request and names what to change (review round of PR #529).
func TestARunRefusesAnAPIBaseThatDoesNotParse(t *testing.T) {
	e := i491Setup(t, "rup", "", fakeIssue{Title: "Crane", Body: "forge body"})
	raw := `{"kind":"github","owner":"o","repo":"r","api_base":"https://user:secret@ghe.example.com%zz"}`
	if _, err := e.pool.Exec(context.Background(), `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`, e.proj.ID, raw); err != nil {
		t.Fatal(err)
	}
	proj, err := store.GetProjectByID(context.Background(), e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	m := e.i509Mgr(e.pool)
	if _, err := m.StartSync(context.Background(), *proj, false); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, m, proj.ID)
	if !strings.Contains(st.LastError, "forge.api_base") || strings.Contains(st.LastError, "secret") {
		t.Errorf("the run's last_error %q — want it to name forge.api_base, without its value", st.LastError)
	}
}

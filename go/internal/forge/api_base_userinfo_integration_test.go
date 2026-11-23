//go:build integration

// Issue #474: a project row whose forge.api_base carries userinfo — written
// before the API refused it — does not sync. Before, a run without a token
// sent the userinfo to the forge as Basic credentials on every request
// (net/http sets them from the URL when no Authorization header is set);
// with a token the Bearer header won and the userinfo stayed unused. The
// run now ends before its first request and names what to change.
//
//	go test -tags=integration ./internal/forge/ -run TestARunRefusesAnAPIBaseWithCredentials -count=1 -v
package forge

import (
	"context"
	"strings"
	"testing"
)

func TestARunRefusesAnAPIBaseWithCredentials(t *testing.T) {
	e := i491Setup(t, "rui", "", fakeIssue{Title: "Crane", Body: "forge body"})
	withUser := strings.Replace(e.gh.srv.URL, "http://", "http://user:secret@", 1)
	m := e.i509Mgr(e.pool)
	if _, err := e.pool.Exec(context.Background(), `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`,
		e.proj.ID, `{"kind":"github","owner":"o","repo":"r","api_base":"`+withUser+`"}`); err != nil {
		t.Fatal(err)
	}
	proj := e.proj
	proj.Forge = []byte(`{"kind":"github","owner":"o","repo":"r","api_base":"` + withUser + `"}`)
	e.gh.mu.Lock()
	e.gh.auths = nil // the setup's pull
	e.gh.mu.Unlock()
	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatal(err)
	}
	st := waitDone(t, m, proj.ID)
	e.gh.mu.Lock()
	auths := append([]string(nil), e.gh.auths...)
	e.gh.mu.Unlock()
	for _, a := range auths {
		if strings.HasPrefix(a, "Basic ") {
			t.Errorf("the run sent the api_base's userinfo to the forge as %q (%d request(s))", a, len(auths))
			break
		}
	}
	if len(auths) != 0 {
		t.Errorf("the run made %d request(s) with credentials in its api_base, want none", len(auths))
	}
	if !strings.Contains(st.LastError, "credentials") || strings.Contains(st.LastError, "secret") {
		t.Errorf("the run's last_error %q — want it to name the credentials in forge.api_base, without them", st.LastError)
	}
}

//go:build integration

package handler

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// TestOAuthTokenBasicUsernameWithNUL (issue #519, second review round of PR
// #553): the request's NUL guard reads the Authorization header as it came —
// base64 text — so a NUL in the DECODED Basic username passed it, reached the
// client lookup as the client id, and the database's refusal (22021) came
// back as 500 with an ERROR line. Such an id names no client: 401
// invalid_client, as every unknown client id answers.
func TestOAuthTokenBasicUsernameWithNUL(t *testing.T) {
	oh := NewOAuthHandler(testdb.SetupTestDB(t))
	form := url.Values{"grant_type": {"authorization_code"}, "code": {"c"}, "code_verifier": {"v"}}
	for _, user := range []string{"cli\x00ent", "\x00"} {
		status, body, _ := caToken(t, oh, form, user, "secret")
		if status != http.StatusUnauthorized || body["error"] != "invalid_client" {
			t.Errorf("Basic username %q: %d %v, want 401 invalid_client", user, status, body)
		}
	}
}

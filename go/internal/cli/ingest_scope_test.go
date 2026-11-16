package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// `ctx ingest --scope` sent `scope` with a default of "private", and the server
// never read it (#322). The flag now goes out as `target_scope`, only when it is
// set; the summary names the scope from the server's answer, and a run whose
// --scope the server did not confirm exits 1. These tests run the command
// against fake servers; internal/handler runs it against the real handler.

// scopeServer records the raw bodies of POST /api/ingest and answers each
// with success; answerScope is the answer's `scope`, "" leaves the field out
// (a server from before target_scope). deny answers every request 403
// scope_denied instead. GET /api/whoami answers home_scope whoamiHome, or 404
// when it is "".
type scopeServer struct {
	mu          sync.Mutex
	bodies      []map[string]any
	whoamis     int
	answerScope string
	whoamiHome  string
	deny        bool
}

func (s *scopeServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/whoami" {
		s.mu.Lock()
		s.whoamis++
		s.mu.Unlock()
		if s.whoamiHome == "" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "home_scope": s.whoamiHome})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"success":false,"error":"Invalid request body"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.bodies = append(s.bodies, body)
	s.mu.Unlock()
	if s.deny {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false,
			"error": "Cannot write to requested scope", "code": "scope_denied"})
		return
	}
	chunks, _ := body["chunks"].([]any)
	answer := map[string]any{"success": true, "total": len(chunks), "updated": len(chunks)}
	if s.answerScope != "" {
		answer["scope"] = s.answerScope
	}
	_ = json.NewEncoder(w).Encode(answer)
}

func scopeVault(t *testing.T) string {
	t.Helper()
	vault := t.TempDir()
	writeIngestFixture(t, vault, "a.md", ingestNote("MARKER-scope-a"))
	return vault
}

// threeFileVault holds three one-chunk notes; with --batch 1 a run sends
// three requests unless it stops.
func threeFileVault(t *testing.T) string {
	t.Helper()
	vault := t.TempDir()
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		writeIngestFixture(t, vault, name, ingestNote("MARKER-scope-"+name))
	}
	return vault
}

func TestIngestScopeOnTheWire(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantField any // target_scope in the request; nil = absent
	}{
		{"no_flag_sends_no_scope", nil, nil},
		{"flag_sends_target_scope", []string{"--scope", "shared"}, "shared"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scopeServer{answerScope: "shared"}
			srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
			defer srv.Close()

			_, _ = runIngestCmd(t, srv.URL, scopeVault(t), tc.args...)

			if len(s.bodies) != 1 {
				t.Fatalf("%d requests, want 1", len(s.bodies))
			}
			body := s.bodies[0]
			if got := body["target_scope"]; got != tc.wantField {
				t.Errorf("target_scope = %v, want %v", got, tc.wantField)
			}
			// The old field is ignored by every server; sending it again would
			// only invite a server to honour the old CLI default.
			if got, ok := body["scope"]; ok {
				t.Errorf("request carries scope = %v, want no such field", got)
			}
		})
	}
}

func TestIngestReportsTheServersScope(t *testing.T) {
	s := &scopeServer{answerScope: "work"}
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	defer srv.Close()

	out, err := runIngestCmd(t, srv.URL, scopeVault(t))
	if err != nil {
		t.Fatalf("ctx ingest: %v", err)
	}
	if !strings.Contains(out, "Scope:    work\n") {
		t.Errorf("summary lacks the server's scope:\n%s", out)
	}
}

// A server from before target_scope answers without `scope` and writes to the
// key's home scope. Without --scope that is what was asked. With --scope
// naming the home scope (GET /api/whoami tells) it is too. With any other
// --scope the run did not do what it said: it stops after the first answer,
// so one batch, not the whole vault, lands in the home scope, and exits 1.
func TestIngestScopeNotReported(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		whoamiHome string
		wantSent   int
		wantErr    bool
		wantOut    []string
	}{
		{"no_flag", nil, "", 3, false,
			[]string{"Scope:    not reported by the server"}},
		{"flag_not_the_home_scope", []string{"--scope", "shared"}, "private", 1, true,
			[]string{"Scope:    not reported by the server", "Not sent: 2"}},
		{"flag_and_no_whoami", []string{"--scope", "shared"}, "", 1, true,
			[]string{"Scope:    not reported by the server", "Not sent: 2"}},
		{"flag_naming_the_home_scope", []string{"--scope", "shared"}, "shared", 3, false,
			[]string{"Scope:    shared (the key's home scope; the server did not report it)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &scopeServer{whoamiHome: tc.whoamiHome}
			srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
			defer srv.Close()

			out, err := runIngestCmd(t, srv.URL, threeFileVault(t), append([]string{"--batch", "1"}, tc.args...)...)
			if len(s.bodies) != tc.wantSent {
				t.Errorf("%d batches sent, want %d", len(s.bodies), tc.wantSent)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("summary lacks %q:\n%s", want, out)
				}
			}
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), `"shared"`)) {
				t.Errorf("err = %v, want an error naming the scope that was not confirmed", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ctx ingest: %v", err)
			}
			if tc.args == nil && s.whoamis != 0 {
				t.Errorf("a run without --scope asked /api/whoami %d time(s)", s.whoamis)
			}
		})
	}
}

// A server that names a scope other than --scope (none does today:
// resolveWriteScope answers the requested scope or refuses) is not taken for
// a confirmation.
func TestIngestScopeOtherThanAsked(t *testing.T) {
	s := &scopeServer{answerScope: "private"}
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	defer srv.Close()

	out, err := runIngestCmd(t, srv.URL, threeFileVault(t), "--batch", "1", "--scope", "shared")
	if err == nil || !strings.Contains(err.Error(), "the server wrote to private") {
		t.Errorf("err = %v, want the scope the server wrote to", err)
	}
	if len(s.bodies) != 1 || !strings.Contains(out, "Not sent: 2") {
		t.Errorf("%d batches sent, want 1 and two not sent:\n%s", len(s.bodies), out)
	}
}

// A scope the key may not write is refused for every batch alike, so the run
// stops after the first refusal instead of sending the rest to be refused.
func TestIngestStopsOnScopeDenied(t *testing.T) {
	s := &scopeServer{deny: true}
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	defer srv.Close()

	out, err := runIngestCmd(t, srv.URL, threeFileVault(t), "--batch", "1", "--scope", "shared")
	if len(s.bodies) != 1 {
		t.Errorf("%d batches sent, want 1", len(s.bodies))
	}
	for _, want := range []string{"batch-0: server: Cannot write to requested scope", "Failed:   1", "Not sent: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "3 chunk(s) not written") {
		t.Errorf("err = %v, want three chunks not written", err)
	}
}

// A dry run does not contact the server, so it cannot know whether the key may
// write the scope; it says so instead of implying a check.
func TestIngestDryRunNamesTheScopeUnchecked(t *testing.T) {
	s := &scopeServer{answerScope: "shared"}
	srv := httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	defer srv.Close()

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--dry-run", "--scope", "shared"}, "Scope:      shared (not checked: a dry run does not contact the server)"},
		{[]string{"--dry-run"}, "Scope:      the key's home scope (not checked: a dry run does not contact the server)"},
	} {
		out, err := runIngestCmd(t, srv.URL, scopeVault(t), tc.args...)
		if err != nil {
			t.Fatalf("ctx ingest %v: %v", tc.args, err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("ctx ingest %v: output lacks %q:\n%s", tc.args, tc.want, out)
		}
	}
	if len(s.bodies) != 0 {
		t.Errorf("dry run sent %d request(s)", len(s.bodies))
	}
}

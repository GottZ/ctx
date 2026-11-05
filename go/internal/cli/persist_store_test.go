package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// persistHookStdin is a SubagentStop payload carrying three marker blocks.
const persistHookStdin = `{"cwd":"/work/proj","session_id":"s1","agent_type":"general","agent_id":"a1",` +
	`"last_assistant_message":"done\n[PERSIST:learnings:One]\nfirst\n[PERSIST:learnings:Two]\nsecond\n[PERSIST:decisions:Three]\nthird\n"}`

// storeAnswer is one canned /api/store response.
type storeAnswer struct {
	status int
	body   string
}

// runPersistHookAgainst drives `ctx persist --hook` through the cobra command
// against an httptest server that answers the n-th POST /api/store with
// answers[n] (the last answer repeats). It returns what the command wrote to
// stderr, how many store requests reached the server, and the command error.
func runPersistHookAgainst(t *testing.T, answers []storeAnswer) (string, int32, error) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/store" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n := int(hits.Add(1)) - 1
		if n >= len(answers) {
			n = len(answers) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answers[n].status)
		_, _ = w.Write([]byte(answers[n].body))
	}))
	defer srv.Close()

	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	stderr, err := execPersistHook(t, getClient)
	return stderr, hits.Load(), err
}

// execPersistHook runs `persist --hook` with persistHookStdin on stdin and
// os.Stderr redirected to a temp file; it returns the captured stderr and the
// command error.
func execPersistHook(t *testing.T, getClient func() (*Client, error)) (string, error) {
	t.Helper()
	stderrPath := filepath.Join(t.TempDir(), "stderr.txt")
	errFile, err := os.Create(stderrPath)
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	defer func() { _ = errFile.Close() }()
	oldErr := os.Stderr
	os.Stderr = errFile
	defer func() { os.Stderr = oldErr }()

	cmd := persistCmd(getClient)
	cmd.SetArgs([]string{"--hook"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var runErr error
	withStdin(t, persistHookStdin, func() { runErr = cmd.Execute() })
	b, err := os.ReadFile(stderrPath)
	if err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}
	return string(b), runErr
}

// TestPersistHookCountsOnlyAcceptedStores pins issue #59: a block counts as
// saved only when the server accepted it (2xx AND success:true). A rejected
// write is dropped exactly like an unreachable server — no error, exit 0, and
// never reported as saved.
func TestPersistHookCountsOnlyAcceptedStores(t *testing.T) {
	ok := storeAnswer{http.StatusOK, `{"success":true,"id":"b1","action":"created"}`}
	cases := []struct {
		name     string
		answers  []storeAnswer
		want     string // expected stderr, exactly
		wantHits int32
	}{
		{"all accepted", []storeAnswer{ok}, "ctx persist: 3 block(s) saved\n", 3},
		{"403 envelope", []storeAnswer{{http.StatusForbidden, `{"success":false,"error":"scope not writable"}`}}, "", 3},
		{"413 envelope", []storeAnswer{{http.StatusRequestEntityTooLarge, `{"success":false,"error":"content too large"}`}}, "", 3},
		{"429 envelope", []storeAnswer{{http.StatusTooManyRequests, `{"success":false,"error":"quota_exceeded"}`}}, "", 1},
		{"500 plain text", []storeAnswer{{http.StatusInternalServerError, "internal server error"}}, "", 3},
		{"500 with success:true body", []storeAnswer{{http.StatusInternalServerError, `{"success":true}`}}, "", 3},
		{"200 success:false", []storeAnswer{{http.StatusOK, `{"success":false,"error":"validation: category"}`}}, "", 3},
		{"200 frameless body", []storeAnswer{{http.StatusOK, `[]`}}, "", 3},
		{"mixed: one of three accepted", []storeAnswer{
			ok,
			{http.StatusForbidden, `{"success":false,"error":"forbidden"}`},
			{http.StatusOK, `{"success":false,"error":"validation: title"}`},
		}, "ctx persist: 1 block(s) saved\n", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stderr, hits, err := runPersistHookAgainst(t, tc.answers)
			if err != nil {
				t.Fatalf("persist --hook returned %v; the hook must never fail", err)
			}
			if hits != tc.wantHits {
				t.Fatalf("server saw %d store requests, want %d", hits, tc.wantHits)
			}
			if stderr != tc.want {
				t.Errorf("stderr = %q, want %q", stderr, tc.want)
			}
		})
	}
}

// TestPersistHookStopsAfterUnauthorized pins the key-wide 401 case from
// issue #65: after the first rejection, later blocks must not be sent.
func TestPersistHookStopsAfterUnauthorized(t *testing.T) {
	stderr, hits, err := runPersistHookAgainst(t, []storeAnswer{
		{http.StatusUnauthorized, `{"success":false,"error":"invalid key"}`},
	})
	if err != nil {
		t.Fatalf("persist --hook returned %v; the hook must never fail", err)
	}
	if hits != 1 {
		t.Fatalf("server saw %d store requests, want 1", hits)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestPersistHookStopsAfterRateLimit pins the key-wide 429 case from issue
// #65: within the rate-limit window, later writes get the same rejection.
func TestPersistHookStopsAfterRateLimit(t *testing.T) {
	stderr, hits, err := runPersistHookAgainst(t, []storeAnswer{
		{http.StatusTooManyRequests, `{"success":false,"error":"rate_limit"}`},
	})
	if err != nil {
		t.Fatalf("persist --hook returned %v; the hook must never fail", err)
	}
	if hits != 1 {
		t.Fatalf("server saw %d store requests, want 1", hits)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestPersistHookUnreachableIsSilent keeps the transport-error branch as it
// was: nothing printed, no error.
func TestPersistHookUnreachableIsSilent(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	getClient := func() (*Client, error) {
		return &Client{BaseURL: url, Key: "test-key", HTTPClient: &http.Client{}}, nil
	}
	got, err := execPersistHook(t, getClient)
	if err != nil {
		t.Fatalf("persist --hook returned %v; the hook must never fail", err)
	}
	if strings.TrimSpace(got) != "" {
		t.Errorf("stderr = %q, want empty", got)
	}
}

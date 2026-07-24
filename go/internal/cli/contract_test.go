// Evokoa-Clean-Room design/03 §7 W03-4 Gate 5: `ctx contract` exit codes.
//
// G-ROT-1 companion (captured before ExitCodeError/contractCmd existed):
// a naive RunE that just returned `err` (or nothing) for a non-200 status
// would let cobra collapse a 403/dead-daemon response to the SAME exit 1 a
// real "drift" result produces — exactly the fail-open state.sh/test.sh
// must never see. This file's Test...TransportOrAuth cases are the direct
// rot-evidence: they assert exit 3, not exit 1, for those cases.
package cli

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

// newContractRoot builds a minimal `ctx contract` cobra tree whose client
// points at srv (mirrors newKanbanRoot, kanban_test.go).
func newContractRoot(srv *httptest.Server) *cobra.Command {
	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(contractCmd(getClient))
	return root
}

// contractExitCode runs `ctx contract` against root and extracts the
// process exit code an ExitCodeError would produce (cmd/ctx/main.go's own
// unwrap logic, duplicated here in test form so the assertion is on the
// SAME contract main.go relies on) — nil error ⇒ exit 0, any other error
// (should never happen for this command) fails the test loudly rather than
// silently reporting 1.
func contractExitCode(t *testing.T, root *cobra.Command, args ...string) int {
	t.Helper()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return 0
	}
	var ece *ExitCodeError
	if errors.As(err, &ece) {
		return ece.Code
	}
	t.Fatalf("root.Execute() returned a non-ExitCodeError: %v (%T) — contractCmd must always return *ExitCodeError or nil", err, err)
	return -1
}

func TestContractCLI_OK_Exit0(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refresh") != "1" {
			t.Errorf("request without --cached must default to ?refresh=1, got query=%q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"status":"ok","mode":"warn","mode_source":"default","manifest_max":112,"live_max":112,"excluded_snapshot_tables":0,"drifts":[]}`))
	}))
	defer srv.Close()

	code := contractExitCode(t, newContractRoot(srv), "contract")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestContractCLI_Drift_Exit1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"status":"drift","mode":"warn","mode_source":"default","manifest_max":112,"live_max":112,"excluded_snapshot_tables":0,` +
			`"drifts":[{"class":"definition_drift","severity":"param","object":"index:idx_embedding_hnsw","detail":"reloptions ef_construction: manifest=128 live=64"}]}`))
	}))
	defer srv.Close()

	code := contractExitCode(t, newContractRoot(srv), "contract")
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}

func TestContractCLI_Unchecked_Exit2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"status":"unchecked","mode":"warn","mode_source":"default","manifest_max":0,"live_max":0,"excluded_snapshot_tables":0,"drifts":[]}`))
	}))
	defer srv.Close()

	code := contractExitCode(t, newContractRoot(srv), "contract")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

// TestContractCLI_Forbidden_Exit3NeverZeroOrOne is the 403 rot-evidence: a
// non-admin key (or an admin-gate misconfiguration) must NEVER read as
// "drift" (1) or, worse, silently succeed (0) — state.sh/test.sh gate on
// this exit code and must not fail-open on an auth failure.
func TestContractCLI_Forbidden_Exit3NeverZeroOrOne(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"success":false,"error":"admin key required"}`))
	}))
	defer srv.Close()

	code := contractExitCode(t, newContractRoot(srv), "contract")
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (403 must never be 0 or 1)", code)
	}
}

// TestContractCLI_DeadServer_Exit3NeverZero is the dead-daemon rot-evidence:
// a transport failure (connection refused) must never read as 0 — the exact
// failure mode design/03 §7 W03-4 Gate 5 names explicitly ("test.sh/state.sh
// konsumieren sonst fail-open").
func TestContractCLI_DeadServer_Exit3NeverZero(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := "http://" + l.Addr().String()
	_ = l.Close() // now nothing listens — every request refuses the connection

	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(contractCmd(func() (*Client, error) {
		return &Client{BaseURL: addr, Key: "test-key", HTTPClient: http.DefaultClient}, nil
	}))

	code := contractExitCode(t, root, "contract")
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (a dead daemon must never read as 0)", code)
	}
}

// TestContractCLI_GetClientError_Exit3 covers the "no config" path
// (getClient itself fails, e.g. no CTX_BASE_URL/CTX_KEY) — same fail-closed
// requirement as the transport/auth cases above.
func TestContractCLI_GetClientError_Exit3(t *testing.T) {
	root := &cobra.Command{Use: "ctx", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(contractCmd(func() (*Client, error) {
		return nil, errBoom
	}))

	code := contractExitCode(t, root, "contract")
	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

var errBoom = errors.New("no config found")

// TestContractCLI_CachedFlag_NoRefreshQueryParam proves --cached opts out
// of the refresh-by-default (design/03 §4.6 Revision: "ein CI-Gate darf nie
// einen gecachten Boot-Report bestätigen" — refresh IS the default, --cached
// is the deliberate opt-out).
func TestContractCLI_CachedFlag_NoRefreshQueryParam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("refresh") == "1" {
			t.Errorf("--cached must NOT send ?refresh=1, got query=%q", r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"status":"ok","mode":"warn","mode_source":"default","manifest_max":112,"live_max":112,"excluded_snapshot_tables":0,"drifts":[]}`))
	}))
	defer srv.Close()

	code := contractExitCode(t, newContractRoot(srv), "contract", "--cached")
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

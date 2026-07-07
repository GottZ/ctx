package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// newEjectTestCmd wires the eject command against a test server and logs each
// /api/manage body. Mirrors newTenantTestCmd (tenant_test.go).
func newEjectTestCmd(t *testing.T, status int, respBody string) (*cobra.Command, *[]map[string]any) {
	t.Helper()
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("test server: unparseable body: %v", err)
		}
		calls = append(calls, body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)

	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	cmd := ejectCmd(getClient)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd, &calls
}

func runEjectCmd(t *testing.T, cmd *cobra.Command, args ...string) error {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer func() { _ = devnull.Close() }()
	old := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = old }()

	cmd.SetArgs(args)
	return cmd.Execute()
}

// TestEjectSendsCanonicalAction is the U01-W8 CLI functional gate (RED against
// Ist: `ctx eject` was an unknown command). The canonical command must POST the
// CANONICAL manage action `eject-mode` (AM-7) — never the legacy `gaming-mode`.
func TestEjectSendsCanonicalAction(t *testing.T) {
	ok := `{"success":true,"gaming":{"active":true,"disabled_backends":["herbert-chat"],"note":"x"}}`
	cases := []struct {
		name     string
		args     []string
		wantMode any // nil = status read ({} data, no mode key)
	}{
		{"status", nil, nil},
		{"on", []string{"on"}, "on"},
		{"off", []string{"off"}, "off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, calls := newEjectTestCmd(t, http.StatusOK, ok)
			if err := runEjectCmd(t, cmd, tc.args...); err != nil {
				t.Fatalf("eject %v: err = %v, want nil", tc.args, err)
			}
			if len(*calls) != 1 {
				t.Fatalf("want 1 manage call, got %d", len(*calls))
			}
			body := (*calls)[0]
			if body["action"] != "eject-mode" {
				t.Errorf("action = %v, want %q (canonical, NOT gaming-mode)", body["action"], "eject-mode")
			}
			data, _ := body["data"].(map[string]any)
			if tc.wantMode == nil {
				if len(data) != 0 {
					t.Errorf("status read data = %v, want empty {}", data)
				}
			} else if data["mode"] != tc.wantMode {
				t.Errorf("data.mode = %v, want %v", data["mode"], tc.wantMode)
			}
		})
	}
}

// TestGamingAliasStillWorks pins N19: the legacy `ctx gaming` name keeps working
// (cobra alias on the eject command) and is shape-compatible — it resolves to
// the SAME canonical eject-mode action and mode payload.
func TestGamingAliasStillWorks(t *testing.T) {
	ok := `{"success":true,"gaming":{"active":true,"disabled_backends":[],"note":"x"}}`
	// The alias is resolved by the parent command tree, so drive it through a
	// root that carries the eject command with its "gaming" alias.
	root := &cobra.Command{Use: "ctx"}
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ok))
	}))
	t.Cleanup(srv.Close)
	getClient := func() (*Client, error) {
		return &Client{BaseURL: srv.URL, Key: "test-key", HTTPClient: srv.Client()}, nil
	}
	root.AddCommand(ejectCmd(getClient))
	root.SilenceUsage = true
	root.SilenceErrors = true

	if err := runEjectCmd(t, root, "gaming", "on"); err != nil {
		t.Fatalf("ctx gaming on: err = %v, want nil (alias must keep working)", err)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 manage call via alias, got %d", len(calls))
	}
	if calls[0]["action"] != "eject-mode" {
		t.Errorf("alias action = %v, want %q (shape-compatible, canonical)", calls[0]["action"], "eject-mode")
	}
	data, _ := calls[0]["data"].(map[string]any)
	if data["mode"] != "on" {
		t.Errorf("alias data.mode = %v, want %q", data["mode"], "on")
	}
}

// TestEjectBadMode rejects a non on/off argument before any client call.
func TestEjectBadMode(t *testing.T) {
	getClient := func() (*Client, error) {
		t.Fatal("client must never be built for a bad mode")
		return nil, nil
	}
	cmd := ejectCmd(getClient)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := runEjectCmd(t, cmd, "sideways"); err == nil {
		t.Fatal("eject sideways: err = nil, want a validation error")
	}
}

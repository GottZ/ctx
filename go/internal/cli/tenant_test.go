package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTenantTestCmd wires the tenant command family against a test server and
// silences cobra's own stderr chatter. Returned alongside is the request log
// (one JSON body per /api/manage call).
func newTenantTestCmd(t *testing.T, status int, respBody string) (*cobra.Command, *[]map[string]any) {
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
	cmd := tenantCmd(getClient)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	return cmd, &calls
}

// runTenant executes the command with args, capturing the RunE error while
// keeping test output clean (stdout swapped to /dev/null — the pipe path
// prints JSON, which is behaviour under test elsewhere, noise here).
func runTenant(t *testing.T, cmd *cobra.Command, args ...string) error {
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

// TestTenant403MappedToCommandError is the W14 error-path gate: the manage
// API answers the server-admin tier gate with 403 {"success":false,"error":
// "admin key required"} (handler/context_manage.go requireAdminAction). The
// CLI must surface that as a command error (cobra → stderr, main.go → exit 1)
// carrying the server's reason — never a raw-JSON print with exit 0.
//
// RED state proven before the envelope gate existed in tenantManage
// (checkSettingsEnvelope temporarily removed): every subtest failed with
// "err = nil, want the server's 403 reason". GREEN with the gate in place.
func TestTenant403MappedToCommandError(t *testing.T) {
	deny := `{"success":false,"error":"admin key required"}`
	cases := [][]string{
		{"list"},
		{"get", "0197aaaa-0000-7000-8000-000000000001"},
		{"create", "friend", "Friend Corp"},
		{"update", "0197aaaa-0000-7000-8000-000000000001", "--status", "suspended"},
		{"delete", "0197aaaa-0000-7000-8000-000000000001"},
		{"usage"},
		{"limit", "set", "0197aaaa-0000-7000-8000-000000000001", "--max-scopes", "5", "--max-keys", "5"},
		{"grant", "create", "0197aaaa-0000-7000-8000-000000000002", "work"},
		{"grant", "list"},
		{"grant", "delete", "0197aaaa-0000-7000-8000-000000000003"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd, _ := newTenantTestCmd(t, http.StatusForbidden, deny)
			err := runTenant(t, cmd, args...)
			if err == nil {
				t.Fatal("err = nil, want the server's 403 reason as a command error")
			}
			if !strings.Contains(err.Error(), "admin key required") {
				t.Fatalf("err = %q, want it to carry the server reason %q", err, "admin key required")
			}
		})
	}
}

// TestTenantSuccessEnvelopePasses is the positive counterpart of the 403 gate:
// a success:true envelope must NOT produce a command error.
func TestTenantSuccessEnvelopePasses(t *testing.T) {
	cmd, _ := newTenantTestCmd(t, http.StatusOK, `{"success":true,"tenants":[]}`)
	if err := runTenant(t, cmd, "list"); err != nil {
		t.Fatalf("err = %v, want nil on a success envelope", err)
	}
}

// TestTenantWirePayloads pins each subcommand onto the EXACT manage-API
// contract (handler/tenant_manage.go): action names, top-level id/status,
// nested data fields, and the null-=-unlimited limit convention. These are
// the mutation-path proofs — live probes stay read-only by policy.
func TestTenantWirePayloads(t *testing.T) {
	ok := `{"success":true,"tenant":{},"tenants":[],"grants":[],"usage":{}}`
	cases := []struct {
		name string
		args []string
		want string // canonical JSON of the expected request body
	}{
		{"list", []string{"list"},
			`{"action":"tenant-list"}`},
		{"default_is_list", []string{},
			`{"action":"tenant-list"}`},
		{"get", []string{"get", "tid-1"},
			`{"action":"tenant-get","id":"tid-1"}`},
		{"create_plain", []string{"create", "friend", "Friend", "Corp"},
			`{"action":"tenant-create","data":{"display_name":"Friend Corp","slug":"friend"}}`},
		{"create_seeded_limits", []string{"create", "friend", "Friend", "--max-scopes", "10", "--max-keys", "-1"},
			`{"action":"tenant-create","data":{"display_name":"Friend","max_keys":null,"max_scopes":10,"slug":"friend"}}`},
		{"update_status", []string{"update", "tid-1", "--status", "suspended"},
			`{"action":"tenant-update","id":"tid-1","status":"suspended"}`},
		{"update_display_name", []string{"update", "tid-1", "--display-name", "New Name"},
			`{"action":"tenant-update","data":{"display_name":"New Name"},"id":"tid-1"}`},
		{"delete", []string{"delete", "tid-1"},
			`{"action":"tenant-delete","id":"tid-1"}`},
		{"usage_own", []string{"usage"},
			`{"action":"tenant-usage-get"}`},
		{"usage_foreign", []string{"usage", "tid-2"},
			`{"action":"tenant-usage-get","id":"tid-2"}`},
		{"limit_set", []string{"limit", "set", "tid-1", "--max-scopes", "5", "--max-keys", "-1"},
			`{"action":"tenant-limit-set","data":{"max_keys":null,"max_scopes":5},"id":"tid-1"}`},
		{"grant_create", []string{"grant", "create", "tid-2", "work"},
			`{"action":"tenant-grant-create","data":{"granted_scope":"work","grantee_tenant":"tid-2"}}`},
		{"grant_list_all", []string{"grant", "list"},
			`{"action":"tenant-grant-list"}`},
		{"grant_list_filtered", []string{"grant", "list", "tid-2"},
			`{"action":"tenant-grant-list","id":"tid-2"}`},
		{"grant_delete", []string{"grant", "delete", "gid-1"},
			`{"action":"tenant-grant-delete","id":"gid-1"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, calls := newTenantTestCmd(t, http.StatusOK, ok)
			if err := runTenant(t, cmd, c.args...); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if len(*calls) != 1 {
				t.Fatalf("calls = %d, want exactly 1", len(*calls))
			}
			got, _ := json.Marshal((*calls)[0])
			var want any
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatalf("bad want fixture: %v", err)
			}
			wantJSON, _ := json.Marshal(want)
			if string(got) != string(wantJSON) {
				t.Errorf("wire body = %s, want %s", got, wantJSON)
			}
		})
	}
}

// TestTenantClientSideValidation: argument gates that must fail BEFORE any
// HTTP call (no request may reach the server on a malformed invocation).
func TestTenantClientSideValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"limit_set_needs_both_flags", []string{"limit", "set", "tid-1", "--max-scopes", "5"},
			"both --max-scopes and --max-keys are required"},
		{"update_needs_a_field", []string{"update", "tid-1"},
			"nothing to update"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, calls := newTenantTestCmd(t, http.StatusOK, `{"success":true}`)
			err := runTenant(t, cmd, c.args...)
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
			}
			if len(*calls) != 0 {
				t.Errorf("calls = %d, want 0 (validation must precede HTTP)", len(*calls))
			}
		})
	}
}

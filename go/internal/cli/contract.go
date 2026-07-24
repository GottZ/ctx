// ctx contract — CLI for the admin-gated GET /api/contract surface
// (Evokoa-Clean-Room design/03 §4.6, W03-4).
//
//	ctx contract           # forces a fresh check (?refresh=1, the default)
//	ctx contract --cached  # serves the last stored report instead
//
// Exit codes (design/03 §7 W03-4 Gate 5 — NEVER 0 on a transport/auth
// failure; state.sh/test.sh consume this exit code as their gate signal and
// must not fail-open on a dead daemon or a 403):
//
//	0 = ok         1 = drift        2 = unchecked        3 = transport/auth failure
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// Report status wire values (mirrors schemacontract.StatusOK/StatusDrift/
// StatusUnchecked as plain string literals — internal/cli deliberately does
// NOT import internal/schemacontract, the same decoupling every other CLI
// command already applies to its own wire structs; importing the server
// package here would also pull pgx into the ctx CLI binary for three
// string constants).
const (
	contractStatusOK    = "ok"
	contractStatusDrift = "drift"
)

// ExitCodeError carries a specific process exit code out of a cobra RunE.
// cobra's/this CLI's own convention collapses every RunE error to exit 1
// via cmd/ctx/main.go's `if err != nil { os.Exit(1) }` — differentiated
// exit codes (design/03 §4.6/§7 Gate 5) need their own carrier main.go can
// unwrap. This is an ADDITIVE change: main.go gets ONE new branch in its
// existing error-handling path; every command that does not return this
// type keeps the prior uniform exit(1) — no other command's behavior
// changes, no existing signature in internal/cli or internal/handler is
// touched (Evokoa K4 status-merge-slot rule for this wave).
type ExitCodeError struct {
	Code int
	Msg  string
}

func (e *ExitCodeError) Error() string { return e.Msg }

func exitErr(code int, format string, args ...any) *ExitCodeError {
	return &ExitCodeError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// contractDriftRow mirrors schemacontract.Drift on the wire.
type contractDriftRow struct {
	Class    string `json:"class"`
	Severity string `json:"severity"`
	Object   string `json:"object"`
	Detail   string `json:"detail"`
}

// contractReportBody mirrors the GET /api/contract wire shape
// (handler.contractResponse: {success, <schemacontract.Report fields
// flattened>}) — the CLI stays decoupled from the server's internal
// package the same way every other CLI command mirrors its own wire
// structs (queryResult, settingRow, ...) rather than importing
// internal/schemacontract's Report type directly.
type contractReportBody struct {
	Success                bool               `json:"success"`
	Status                 string             `json:"status"`
	CheckedAt              string             `json:"checked_at"`
	ManifestMax            int                `json:"manifest_max"`
	LiveMax                int                `json:"live_max"`
	Mode                   string             `json:"mode"`
	ModeSource             string             `json:"mode_source"`
	ExcludedSnapshotTables int                `json:"excluded_snapshot_tables"`
	Drifts                 []contractDriftRow `json:"drifts"`
}

func contractCmd(getClient func() (*Client, error)) *cobra.Command {
	var cached bool
	cmd := &cobra.Command{
		Use:   "contract",
		Short: "Schema-contract check (drift, mode, migrations integrity)",
		Long: "Fetches GET /api/contract (admin-gated). Default forces a fresh check " +
			"(?refresh=1) — a CI/state.sh gate must never confirm a cached boot report. " +
			"--cached serves the last stored report instead (max one re-check interval old).",
		Example: `  ctx contract
  ctx contract --cached`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return exitErr(3, "contract check: %v", err)
			}

			path := "/api/contract"
			if !cached {
				path += "?refresh=1"
			}
			body, status, err := c.Do(http.MethodGet, path, nil)
			if err != nil {
				return exitErr(3, "contract check: transport error: %v", err)
			}
			// Rot-Beleg (design/03 §7 W03-4 Gate 5): a naive handler that
			// returned RunE's plain error (or nil on a non-200) here would
			// let cobra collapse a 403/dead-daemon response to the SAME
			// exit 1 a real "drift" result produces (or worse, exit 0 if
			// the body still happened to parse) — exactly the fail-open
			// state.sh/test.sh must never see. Status 200 is required
			// BEFORE the body is trusted at all.
			if status != http.StatusOK {
				return exitErr(3, "contract check: HTTP %d (want 200): %s", status, truncateForError(body))
			}

			var report contractReportBody
			if err := json.Unmarshal(body, &report); err != nil {
				return exitErr(3, "contract check: unparseable response: %v", err)
			}
			if !report.Success {
				return exitErr(3, "contract check: server reported success=false")
			}

			renderContract(cmd, report)

			switch report.Status {
			case contractStatusOK:
				return nil
			case contractStatusDrift:
				return exitErr(1, "contract: drift detected")
			default: // unchecked (and any future/unrecognized value — fail-closed, never exit 0)
				return exitErr(2, "contract: status unchecked")
			}
		},
	}
	cmd.Flags().BoolVar(&cached, "cached", false, "Serve the last stored report instead of forcing a fresh check")
	return cmd
}

// renderContract writes the tabular rendering design/03 §4.6 asks for:
// status line (status/mode/mode_source), then the drift table
// (Class/Severity/Object/Detail), then excluded_snapshot_tables.
func renderContract(cmd *cobra.Command, r contractReportBody) {
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Schema Contract: %s  (mode=%s source=%s)\n", r.Status, r.Mode, r.ModeSource)
	_, _ = fmt.Fprintf(out, "  checked_at=%s manifest_max=%d live_max=%d excluded_snapshot_tables=%d\n",
		r.CheckedAt, r.ManifestMax, r.LiveMax, r.ExcludedSnapshotTables)

	if len(r.Drifts) == 0 {
		_, _ = fmt.Fprintln(out, "  no drift findings")
		return
	}
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "  CLASS\tSEVERITY\tOBJECT\tDETAIL")
	for _, d := range r.Drifts {
		_, _ = fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", d.Class, d.Severity, d.Object, d.Detail)
	}
	_ = w.Flush()
}

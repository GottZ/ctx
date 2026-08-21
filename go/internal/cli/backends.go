// ctx backends — CLI for the admin-gated backend pool manage actions (F3-P1).
//
//	ctx backends              # = list; TTY: table, pipe: raw JSON
//	ctx backends list
//	ctx backends create -     # JSON spec via stdin (or as argument)
//	ctx backends update <id> '{"enabled":false}'
//	ctx backends delete <id>
//	ctx backends test <id> [--probe-chat]
//	ctx backends seed --file seed.json   # first seed of an empty pool
//
// Every subcommand parses the response envelope: success:false (422
// validation, 403 non-admin, 404 unknown id) reaches stderr with exit
// code 1 — the settings/secrets CLI discipline, not PrintJSON-and-exit-0.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// backendRow mirrors the server's backendView wire shape (subset for the
// table; raw JSON carries everything).
type backendRow struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	// Scope separates the shared _global pool from tenant-owned rows; the init
	// wizard needs it to tell "a role is served" from "a role is served for
	// some tenant" (the table below stays scope-free — a tenant-admin sees only
	// its own rows anyway).
	Scope          string   `json:"scope"`
	Trust          string   `json:"trust"`
	Locality       string   `json:"locality"`
	Roles          []string `json:"roles"`
	Priority       int      `json:"priority"`
	Enabled        bool     `json:"enabled"`
	EffectiveState string   `json:"effective_state"`
	CooldownS      int      `json:"cooldown_remaining_s"`
	LastError      string   `json:"last_error"`
	// DisabledByProfiles names the ACTIVE disable-profiles holding this row
	// (backends_manage.go handleBackendList, omitted when none). It is what
	// turns "profile-disabled" from a state into a repair instruction.
	DisabledByProfiles []string `json:"disabled_by_profiles"`
}

// The effective_state values backends.Pool.Status() emits (pool.go). Only these
// two mean "this row is out of every chain"; "cooldown" merely reorders it and
// "active" is the normal case.
const (
	backendStateDisabled        = "disabled"
	backendStateProfileDisabled = "profile-disabled"
)

// servingEligible reports whether a row would take part in a chain at all —
// the SAME qualification Chain and PrimaryModel apply (pool.go): enabled AND
// not held by an ACTIVE disable-profile. Trust and cooldown deliberately stay
// out: trust depends on the caller's sensitivity, and cooldown only demotes a
// row inside the chain, it never removes it.
//
// An empty effective_state means the list carried no live status for the row
// (an older server, or a row that left the snapshot between rows and status);
// the enabled column then decides alone, which is the pre-status behaviour.
func (r backendRow) servingEligible() bool {
	if !r.Enabled {
		return false
	}
	return r.EffectiveState != backendStateDisabled && r.EffectiveState != backendStateProfileDisabled
}

func (r backendRow) hasRole(role string) bool {
	for _, have := range r.Roles {
		if have == role {
			return true
		}
	}
	return false
}

func backendsCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backends",
		Short: "LLM backend pool (admin key required)",
		Long: "Manage the declarative backend pool (context_backends): role-routed,\n" +
			"priority-ordered, trust-gated chains. All subcommands need an ADMIN key.",
		Example: `  ctx backends                       # table (TTY) or JSON (pipe)
  echo '{"name":"openrouter",...}' | ctx backends create -
  ctx backends update 019e… '{"enabled":false}'
  ctx backends test 019e… --probe-chat`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackendsList(getClient)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List pool rows merged with live status",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackendsList(getClient)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "create [json|-]",
		Short: "Create a backend from a JSON spec (argument or stdin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := backendSpecArg(args, 0)
			if err != nil {
				return err
			}
			return runBackendsMutate(getClient, "backend-create", "", spec)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "update <id> [json|-]",
		Short: "Patch a backend (single-field updates; trust elevation needs confirm_trust_elevation, rerank score_domain changes need confirm_score_domain_change)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := backendSpecArg(args, 1)
			if err != nil {
				return err
			}
			return runBackendsMutate(getClient, "backend-update", args[0], spec)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a backend (hard delete; llmlog history stays readable)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackendsMutate(getClient, "backend-delete", args[0], nil)
		},
	})
	var probeChat bool
	testCmd := &cobra.Command{
		Use:   "test <id>",
		Short: "Probe reachability (and optionally a 1-token chat) without settings effect",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var data json.RawMessage
			if probeChat {
				data = json.RawMessage(`{"probe":"chat"}`)
			}
			return runBackendsMutate(getClient, "backend-test", args[0], data)
		},
	}
	testCmd.Flags().BoolVar(&probeChat, "probe-chat", false, "additionally run a 1-token chat against the default model")
	cmd.AddCommand(testCmd)
	cmd.AddCommand(backendsSeedCmd(getClient))
	return cmd
}

// backendSpecArg reads the JSON spec from argv or stdin ("-" or empty).
func backendSpecArg(args []string, idx int) (json.RawMessage, error) {
	raw := ""
	if len(args) > idx && args[idx] != "-" {
		raw = args[idx]
	} else if stdin, ok := ReadStdin(); ok {
		raw = stdin
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("JSON spec required (argument or stdin)")
	}
	if !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("spec is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

func runBackendsList(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "backend-list"})
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Backends []backendRow `json:"backends"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSTATE\tTRUST\tLOCALITY\tPRIO\tROLES\tID")
	for _, b := range payload.Backends {
		state := b.EffectiveState
		if b.CooldownS > 0 {
			state = fmt.Sprintf("%s(%ds)", state, b.CooldownS)
		}
		if b.LastError != "" {
			state += " !" + b.LastError
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			b.Name, state, b.Trust, b.Locality, b.Priority, strings.Join(b.Roles, ","), b.ID)
	}
	_ = w.Flush()
	return nil
}

func runBackendsMutate(getClient func() (*Client, error), action, id string, data json.RawMessage) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	body := map[string]any{"action": action}
	if id != "" {
		body["id"] = id
	}
	if len(data) > 0 {
		body["data"] = data
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", body)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	PrintJSON(resp)
	return nil
}

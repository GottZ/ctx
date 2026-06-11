// ctx settings — CLI for the admin-gated /api/settings surface (F2-W7).
//
//	ctx settings              # = list; TTY: table, pipe: raw JSON
//	ctx settings get <key>    # single key + source + recent audit rows
//	ctx settings set <key> <value>   # or: echo '0.7' | ctx settings set <key>
//	ctx settings unset <key>  # drop the override, revert to env/default
//
// Every subcommand parses the response envelope: success:false (422
// validation, 409 mutability, 403 non-admin, 404 unknown key) reaches stderr
// with exit code 1 — these commands must not inherit the PrintJSON-and-exit-0
// trap of the older endpoint commands, because settings writes feed scripts
// and CI gates that branch on the exit code.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"os"

	"github.com/spf13/cobra"
)

// settingsEnvelope is the shared success/error frame of all settings
// responses; the payload fields stay raw for per-command parsing.
type settingsEnvelope struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// settingRow mirrors the server's settingView wire shape.
type settingRow struct {
	Key        string          `json:"key"`
	EnvVar     string          `json:"env_var"`
	Type       string          `json:"type"`
	Mutability string          `json:"mutability"`
	Value      any             `json:"value"`
	Source     string          `json:"source"`
	Default    any             `json:"default"`
	Sensitive  bool            `json:"sensitive"`
}

// settingAuditRow mirrors store.SettingAudit on the wire.
type settingAuditRow struct {
	Action     string          `json:"action"`
	OldValue   json.RawMessage `json:"old_value"`
	NewValue   json.RawMessage `json:"new_value"`
	ActorLabel *string         `json:"actor_label"`
	Via        string          `json:"via"`
	CreatedAt  string          `json:"created_at"`
}

// checkSettingsEnvelope surfaces an API-level failure as a command error
// (cobra: stderr + exit 1). The raw body is the error detail — the server
// messages are already caller-ready ("validation: …", "… is restart-only").
func checkSettingsEnvelope(resp []byte) error {
	var env settingsEnvelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return fmt.Errorf("unparseable response: %s", truncateForError(resp))
	}
	if !env.Success {
		if env.Error == "" {
			return fmt.Errorf("request failed: %s", truncateForError(resp))
		}
		return fmt.Errorf("%s", env.Error)
	}
	return nil
}

func truncateForError(resp []byte) string {
	s := strings.TrimSpace(string(resp))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// toJSONValue wraps a CLI argument as the PUT body value: literal JSON
// scalars (numbers, true/false, quoted strings) pass through, everything
// else becomes a JSON string. The server normalizes to the registry type
// either way — this only decides how the raw text is transported.
func toJSONValue(raw string) json.RawMessage {
	t := strings.TrimSpace(raw)
	if json.Valid([]byte(t)) {
		var probe any
		if err := json.Unmarshal([]byte(t), &probe); err == nil {
			switch probe.(type) {
			case float64, bool, string:
				return json.RawMessage(t)
			}
		}
	}
	quoted, _ := json.Marshal(raw)
	return quoted
}

func settingsCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "settings",
		Aliases: []string{"cfg"},
		Short:   "Runtime settings overrides (admin key required)",
		Long: "List and edit the context_settings runtime override layer.\n" +
			"All subcommands need an ADMIN key (reads included) — non-admin keys get 403.",
		Example: `  ctx settings                       # table (TTY) or JSON (pipe)
  ctx settings get rerank.blend_weight
  ctx settings set rerank.blend_weight 0.7
  echo '0.7' | ctx settings set rerank.blend_weight
  ctx settings unset rerank.blend_weight`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettingsList(getClient)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List every settings key with value, source and mutability",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettingsList(getClient)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get <key>",
		Short: "Show one key with source and recent audit entries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettingsGet(getClient, args[0])
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "set <key> [value]",
		Short: "Set a runtime override (value as argument or via stdin)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			value := ""
			if len(args) == 2 {
				value = args[1]
			} else if stdin, ok := ReadStdin(); ok {
				value = stdin
			}
			if value == "" {
				return fmt.Errorf("usage: ctx settings set <key> <value>  (or pipe the value via stdin)")
			}
			return runSettingsSet(getClient, args[0], value)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "unset <key>",
		Short: "Remove the override — the key reverts to env/default",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettingsUnset(getClient, args[0])
		},
	})
	return cmd
}

func runSettingsList(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/settings", nil)
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
		Settings []settingRow `json:"settings"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	printSettingsTable(payload.Settings)
	return nil
}

// printSettingsTable renders the TTY view: overrides first matters less than
// scanability, so the rows keep registry order (stable, grouped by prefix).
func printSettingsTable(rows []settingRow) {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "KEY\tVALUE\tSOURCE\tMUTABILITY")
	for _, r := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%v\t%s\t%s\n", r.Key, renderCell(r.Value), r.Source, r.Mutability)
	}
	_ = w.Flush()
}

// renderCell flattens one value for the table; slices join with commas.
func renderCell(v any) string {
	if list, ok := v.([]any); ok {
		parts := make([]string, 0, len(list))
		for _, item := range list {
			parts = append(parts, fmt.Sprintf("%v", item))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprintf("%v", v)
}

func runSettingsGet(getClient func() (*Client, error), key string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/settings/"+key, nil)
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
		Setting settingRow        `json:"setting"`
		Audit   []settingAuditRow `json:"audit"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	s := payload.Setting
	fmt.Printf("%s\n", s.Key)
	fmt.Printf("  value:      %v\n", renderCell(s.Value))
	fmt.Printf("  source:     %s\n", s.Source)
	fmt.Printf("  default:    %v\n", renderCell(s.Default))
	fmt.Printf("  type:       %s\n", s.Type)
	fmt.Printf("  mutability: %s\n", s.Mutability)
	if s.EnvVar != "" {
		fmt.Printf("  env var:    %s\n", s.EnvVar)
	}
	if s.Sensitive {
		fmt.Printf("  sensitive:  true (values masked everywhere)\n")
	}
	if len(payload.Audit) > 0 {
		fmt.Println("  audit:")
		for _, a := range payload.Audit {
			actor := "(sql)"
			if a.ActorLabel != nil {
				actor = *a.ActorLabel
			}
			detail := ""
			if a.Action == "set" {
				detail = fmt.Sprintf(" %s → %s", compactRaw(a.OldValue), compactRaw(a.NewValue))
			}
			fmt.Printf("    %s  %s%s  by %s (%s)\n", a.CreatedAt, a.Action, detail, actor, a.Via)
		}
	}
	return nil
}

// compactRaw renders an audit JSONB value; NULL (create/unset side) as "∅".
func compactRaw(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "∅"
	}
	return string(raw)
}

func runSettingsSet(getClient func() (*Client, error), key, value string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPut, "/api/settings/"+key,
		map[string]any{"value": toJSONValue(value)})
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
		Key      string `json:"key"`
		Value    any    `json:"value"`
		Source   string `json:"source"`
		Previous struct {
			Value  any    `json:"value"`
			Source string `json:"source"`
		} `json:"previous"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("%s = %v (%s; was %v from %s)\n",
		payload.Key, renderCell(payload.Value), payload.Source,
		renderCell(payload.Previous.Value), payload.Previous.Source)
	for _, warn := range payload.Warnings {
		Errorf("warning: %s", warn)
	}
	return nil
}

func runSettingsUnset(getClient func() (*Client, error), key string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodDelete, "/api/settings/"+key, nil)
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
		Key    string `json:"key"`
		Value  any    `json:"value"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("%s reverted to %v (%s)\n", payload.Key, renderCell(payload.Value), payload.Source)
	return nil
}

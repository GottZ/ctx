// ctx types — CLI for the /api/types block-type registry surface (workflow
// W1 reads + W2 writes, design/03 §4.7).
//
//	ctx types                 # = list; TTY: table, pipe: raw JSON
//	ctx types list            # same as above
//	ctx types get <name>      # one type incl. its policy config + source
//	ctx types set <name>      # create/update a type (upsert)
//	ctx types rm <name>       # delete a type
//
// Reads are a member surface (any valid key). Writes require the admin-or-
// tenant-admin tier: a tenant-admin writes its OWN tenant namespace, the server
// operator writes the shipped _global namespace (the scope is pinned by role,
// never a flag). Every subcommand parses the response envelope: success:false
// (403 tier, 404 unknown type, 409 in-use/builtin, 401 unauthorized) reaches
// stderr with exit code 1 — these commands must not inherit the
// PrintJSON-and-exit-0 trap of the older endpoint commands, because `ctx types`
// output feeds scripts and CI gates.

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// typeRow mirrors the server's typeView wire shape (handler/types.go, pinned by
// TestTypesGoldenShape). config stays raw for pass-through printing.
type typeRow struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Scope       string          `json:"scope"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	Builtin     bool            `json:"builtin"`
	IsDefault   bool            `json:"is_default"`
	Config      json.RawMessage `json:"config"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	UpdatedBy   *string         `json:"updated_by,omitempty"`
	Source      string          `json:"source"`
}

func typesCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "types",
		Short: "Block-type registry (read-only): list types and their policy config",
		Long: "List the block types visible to your key — the shipped _global set plus\n" +
			"your own tenant's overlay — and inspect one type's policy config.\n" +
			"Read-only; any valid key may read.",
		Example: `  ctx types                 # table (TTY) or JSON (pipe)
  ctx types list
  ctx types get knowledge`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTypesList(getClient)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List every visible block type with its source and default flag",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTypesList(getClient)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "get <name>",
		Short: "Show one block type incl. its policy config (404 if not visible)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTypesGet(getClient, args[0])
		},
	})
	cmd.AddCommand(typesSetCmd(getClient))
	cmd.AddCommand(&cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a block type (409 if still referenced or builtin)",
		Long: "Delete a block type. A type still referenced by blocks is refused with a\n" +
			"count (409); builtin types are undeletable. A tenant-admin may delete only\n" +
			"types in its own tenant namespace; _global types are operator-only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTypesRm(getClient, args[0])
		},
	})
	return cmd
}

// typesSetCmd builds `ctx types set <name>` — an upsert. Only the flags that
// were explicitly set are sent (cobra Changed), so an update touches nothing it
// was not told to. The target scope is pinned by role server-side; there is no
// --scope flag by design (§5.2 scope-injection class).
func typesSetCmd(getClient func() (*Client, error)) *cobra.Command {
	var display, description, config string
	var isDefault bool
	cmd := &cobra.Command{
		Use:   "set <name>",
		Short: "Create or update a block type (upsert)",
		Long: "Create or update a block type. Writes go to your own tenant namespace\n" +
			"(operator keys write the _global namespace); the scope is pinned by your\n" +
			"key's role, not a flag. --config takes the raw policy-config JSON envelope\n" +
			"(see `ctx types get` for the shape); it is validated server-side.",
		Example: `  ctx types set sprint --display "Sprint"
  ctx types set sprint --description "a time-boxed iteration"
  ctx types set incident --config '{"v":1,"retrieval":{"policy":"full-pass"}}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{}
			if cmd.Flags().Changed("display") {
				payload["display_name"] = display
			}
			if cmd.Flags().Changed("description") {
				payload["description"] = description
			}
			if cmd.Flags().Changed("default") {
				payload["is_default"] = isDefault
			}
			if cmd.Flags().Changed("config") {
				if !json.Valid([]byte(config)) {
					return fmt.Errorf("--config is not valid JSON")
				}
				payload["config"] = json.RawMessage(config)
			}
			return runTypesSet(getClient, args[0], payload)
		},
	}
	cmd.Flags().StringVar(&display, "display", "", "human-readable display name")
	cmd.Flags().StringVar(&description, "description", "", "description of the type")
	cmd.Flags().StringVar(&config, "config", "", "raw policy-config JSON envelope")
	cmd.Flags().BoolVar(&isDefault, "default", false, "make this the default type for its namespace (create only)")
	return cmd
}

func runTypesSet(getClient func() (*Client, error), name string, payload map[string]any) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPut, "/api/types/"+name, payload)
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
	var out struct {
		Type typeRow `json:"type"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("set %s (%s)\n", out.Type.Name, out.Type.Source)
	return nil
}

func runTypesRm(getClient func() (*Client, error), name string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodDelete, "/api/types/"+name, nil)
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
	fmt.Printf("deleted %s\n", name)
	return nil
}

func runTypesList(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/types", nil)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil { // shared success/error frame
		return err
	}
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Types []typeRow `json:"types"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	printTypesTable(payload.Types)
	return nil
}

func printTypesTable(rows []typeRow) {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSOURCE\tDEFAULT\tDISPLAY")
	for _, r := range rows {
		def := ""
		if r.IsDefault {
			def = "default"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.Source, def, r.DisplayName)
	}
	_ = w.Flush()
}

func runTypesGet(getClient func() (*Client, error), name string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/types/"+name, nil)
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
		Type typeRow `json:"type"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	t := payload.Type
	fmt.Printf("%s\n", t.Name)
	fmt.Printf("  source:       %s\n", t.Source)
	fmt.Printf("  scope:        %s\n", t.Scope)
	fmt.Printf("  builtin:      %v\n", t.Builtin)
	fmt.Printf("  is_default:   %v\n", t.IsDefault)
	if t.DisplayName != "" {
		fmt.Printf("  display_name: %s\n", t.DisplayName)
	}
	if t.Description != "" {
		fmt.Printf("  description:  %s\n", t.Description)
	}
	fmt.Printf("  config:       %s\n", compactRaw(t.Config))
	return nil
}

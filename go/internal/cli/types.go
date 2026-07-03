// ctx types — CLI for the read-only /api/types block-type registry surface
// (workflow W1, design/03 §4.7).
//
//	ctx types              # = list; TTY: table, pipe: raw JSON
//	ctx types list         # same as above
//	ctx types get <name>   # one type incl. its policy config + source
//
// A read-only member surface: any valid key lists the types visible to it (the
// shipped _global set plus its own tenant's overlay). The write side (set/rm)
// is a later wave. Every subcommand parses the response envelope: success:false
// (404 unknown type, 401 unauthorized) reaches stderr with exit code 1 — these
// commands must not inherit the PrintJSON-and-exit-0 trap of the older endpoint
// commands, because `ctx types` output feeds scripts and CI gates.

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
	return cmd
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

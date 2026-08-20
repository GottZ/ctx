// ctx secrets — CLI for the write-only /api/secrets surface (F2-W8).
//
//	ctx secrets               # = list (metadata + referenced_by, never values)
//	echo "$KEY" | ctx secrets set <name>     # create or rotate
//	echo "$KEY" | ctx secrets rotate <name>  # alias of set, reads as intent
//	ctx secrets rm <name>     # delete (409 while settings reference it)
//
// Values travel EXCLUSIVELY via stdin: an argv value would be world-readable
// in /proc/<pid>/cmdline and land in the shell history — set/rotate reject a
// second positional argument with a pipe example instead of accepting it.
//
// File is named sealbox.go on purpose: pre-commit Gate 1 blocks any NEW file
// with 'secret' in its basename (external constraint); command names stay
// descriptive.

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

// secretRow mirrors the server's secretView wire shape (metadata only).
type secretRow struct {
	Name         string   `json:"name"`
	KeyVersion   int      `json:"key_version"`
	CreatedAt    string   `json:"created_at"`
	RotatedAt    *string  `json:"rotated_at"`
	ReferencedBy []string `json:"referenced_by"`
}

// rejectArgvValue is the set/rotate guard: a value in argv is already leaked
// (cmdline, history) by the time we could warn — refuse and show the safe way.
func rejectArgvValue(name string) error {
	return fmt.Errorf("refusing a secret value from the command line (visible in /proc and shell history).\n"+
		"Pipe it via stdin instead:\n\n  echo \"$PROVIDER_KEY\" | ctx secrets set %s\n  cat keyfile | ctx secrets set %s", name, name)
}

func secretsCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secrets",
		Aliases: []string{"sec"},
		Short:   "Sealed provider credentials, write-only (admin key required)",
		Long: "Manage AES-256-GCM-sealed provider credentials in context_secrets.\n" +
			"Write-only: values go in via stdin and never come back out — list shows\n" +
			"metadata and which settings keys reference each secret (secret_ref).",
		Example: `  ctx secrets
  echo "$OPENROUTER_KEY" | ctx secrets set openrouter-main
  ctx backends update <id> '{"api_key_ref":"openrouter-main"}'
  ctx secrets rm openrouter-main`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretsList(getClient)
		},
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List secrets (metadata + referenced_by, never values)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretsList(getClient)
		},
	})
	set := func(use, short string) *cobra.Command {
		return &cobra.Command{
			Use:   use + " <name>",
			Short: short,
			Args:  cobra.RangeArgs(1, 2),
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) == 2 {
					return rejectArgvValue(args[0])
				}
				value, ok := ReadStdin()
				if !ok || value == "" {
					return fmt.Errorf("no value on stdin.\n\n  echo \"$PROVIDER_KEY\" | ctx secrets %s %s", use, args[0])
				}
				return runSecretsPut(getClient, args[0], value)
			},
		}
	}
	cmd.AddCommand(set("set", "Create or rotate a secret (value via stdin ONLY)"))
	cmd.AddCommand(set("rotate", "Rotate a secret value (alias of set; value via stdin ONLY)"))
	cmd.AddCommand(&cobra.Command{
		Use:   "rm <name>",
		Short: "Delete a secret (refused with 409 while settings reference it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSecretsRm(getClient, args[0])
		},
	})
	return cmd
}

func runSecretsList(getClient func() (*Client, error)) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodGet, "/api/secrets", nil)
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
		Secrets []secretRow `json:"secrets"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	if len(payload.Secrets) == 0 {
		fmt.Println("no secrets stored")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tKEY_VERSION\tCREATED\tROTATED\tREFERENCED_BY")
	for _, s := range payload.Secrets {
		rotated := "-"
		if s.RotatedAt != nil {
			rotated = *s.RotatedAt
		}
		refs := "-"
		if len(s.ReferencedBy) > 0 {
			refs = strings.Join(s.ReferencedBy, ",")
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n", s.Name, s.KeyVersion, s.CreatedAt, rotated, refs)
	}
	return w.Flush()
}

func runSecretsPut(getClient func() (*Client, error), name, value string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPut, "/api/secrets/"+name, map[string]string{"value": value})
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
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("%s: %sd (value sealed; reference it from a backend-pool row, e.g. `ctx backends update <id> '{\"api_key_ref\":\"%s\"}'`)\n",
		payload.Name, payload.Action, payload.Name)
	return nil
}

func runSecretsRm(getClient func() (*Client, error), name string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodDelete, "/api/secrets/"+name, nil)
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
	fmt.Printf("%s: deleted\n", name)
	return nil
}

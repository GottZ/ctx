// ctx eject — the eject toggle (AM-7 canonical rename of the former "gaming"
// toggle; design 03 §2.6, U01-W8).
//
//	ctx eject          # status (any valid key)
//	ctx eject on       # drop the GPU-host backends from every chain (ADMIN key)
//	ctx eject off      # restore them (ADMIN key)
//
// The legacy name `ctx gaming` stays as a shape-compatible alias (cobra alias
// on this same command, U01-E6/N19 — existing scripts and clients keep working)
// and routes to the SAME canonical eject-mode manage action.
//
// The flip persists through the reserved `eject` disable-profile (092): it
// survives a ctxd restart (unlike dream-mode, whose atomic dies with the
// process) and takes effect on the next chain without one. The envelope is
// parsed: success:false (403 for a non-admin flip, 422 for a bad mode) reaches
// stderr with exit code 1.

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

// ejectView mirrors the server's gamingStateView wire shape (LEGACY shape, kept
// byte-identical for client compat, N19). unknown_backends is intentionally NOT
// modelled: the server retired it structurally in U01-W5 — FK membership can
// neither dangle nor typo, so the field is never populated (context_gaming.go
// ejectShapeView, "No unknown_backends — FK membership is always resolvable").
type ejectView struct {
	Active           bool     `json:"active"`
	DisabledBackends []string `json:"disabled_backends"`
	Note             string   `json:"note"`
}

func ejectCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "eject [on|off]",
		Aliases: []string{"gaming"},
		Short:   "Toggle eject mode (flip needs an admin key)",
		Long: "Eject toggle: drop the GPU-host backends out of EVERY chain so the GPU\n" +
			"is free (maintenance / eject use-case); the CPU/external backends stay in\n" +
			"as failover. The flip is the reserved `eject` disable-profile (092): it\n" +
			"persists across restarts and hits the next chain without one.\n\n" +
			"No argument = status (any valid key). on|off needs an ADMIN key.\n\n" +
			"Alias: `ctx gaming` is the legacy name for the profile `eject`, kept\n" +
			"shape-compatible so existing scripts keep working.",
		Example: `  ctx eject           # status
  ctx eject on        # GPU out of every chain
  ctx eject off       # GPU back in the pool
  ctx gaming on       # legacy alias, identical effect`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data := json.RawMessage("{}")
			if len(args) == 1 {
				mode := strings.ToLower(args[0])
				if mode != "on" && mode != "off" {
					return fmt.Errorf("eject takes \"on\", \"off\", or no argument (status), got %q", args[0])
				}
				data = json.RawMessage(fmt.Sprintf(`{"mode":%q}`, mode))
			}
			return runEject(getClient, data)
		},
	}
}

func runEject(getClient func() (*Client, error), data json.RawMessage) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	// eject-mode is the CANONICAL manage action (AM-7); gaming-mode is its
	// server-side alias. The CLI always speaks the canonical name.
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "eject-mode", "data": data})
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
	// The response still nests the view under "gaming" (legacy wire key, kept
	// byte-identical for client compat, N19).
	var payload struct {
		Gaming ejectView `json:"gaming"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	g := payload.Gaming
	state := "off"
	if g.Active {
		state = "on"
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "eject\t%s\n", state)
	_, _ = fmt.Fprintf(w, "disabled backends\t%s\n", strings.Join(g.DisabledBackends, ", "))
	_ = w.Flush()
	return nil
}

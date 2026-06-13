// ctx gaming — the gaming toggle (F3-P6, design 03 §2.6).
//
//	ctx gaming         # status (any valid key)
//	ctx gaming on      # drop the GPU backends from every chain (ADMIN key)
//	ctx gaming off     # restore them (ADMIN key)
//
// The flip persists through the F2 settings layer: it survives a ctxd restart
// (unlike dream-mode, whose atomic dies with the process) and takes effect on
// the next chain without one. The envelope is parsed: success:false (403 for a
// non-admin flip, 422 for a bad mode) reaches stderr with exit code 1.

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

// gamingView mirrors the server's gamingStateView wire shape.
type gamingView struct {
	Active           bool     `json:"active"`
	DisabledBackends []string `json:"disabled_backends"`
	UnknownBackends  []string `json:"unknown_backends"`
	Note             string   `json:"note"`
}

func gamingCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "gaming [on|off]",
		Short: "Toggle gaming mode (flip needs an admin key)",
		Long: "Flip the GPU-host backends out of EVERY chain so the GPU is free to game;\n" +
			"the CPU/external backends stay in as failover. The flip persists across\n" +
			"restarts (F2 settings) and hits the next chain without one.\n\n" +
			"No argument = status (any valid key). on|off needs an ADMIN key.",
		Example: `  ctx gaming          # status
  ctx gaming on       # GPU free to game
  ctx gaming off      # GPU back in the pool`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data := json.RawMessage("{}")
			if len(args) == 1 {
				mode := strings.ToLower(args[0])
				if mode != "on" && mode != "off" {
					return fmt.Errorf("gaming takes \"on\", \"off\", or no argument (status), got %q", args[0])
				}
				data = json.RawMessage(fmt.Sprintf(`{"mode":%q}`, mode))
			}
			return runGaming(getClient, data)
		},
	}
}

func runGaming(getClient func() (*Client, error), data json.RawMessage) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "gaming-mode", "data": data})
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
		Gaming gamingView `json:"gaming"`
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
	_, _ = fmt.Fprintf(w, "gaming\t%s\n", state)
	_, _ = fmt.Fprintf(w, "disabled backends\t%s\n", strings.Join(g.DisabledBackends, ", "))
	if len(g.UnknownBackends) > 0 {
		_, _ = fmt.Fprintf(w, "unknown (typo?)\t%s\n", strings.Join(g.UnknownBackends, ", "))
	}
	_ = w.Flush()
	return nil
}

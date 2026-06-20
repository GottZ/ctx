// ctx quota — per-tenant cost/call budget (MT T36b, 04-W4).
//
//	ctx quota              # show the calling key's own tenant quota
//	ctx quota <scope>      # show a scope's quota (server-admin)
//	ctx quota set <scope> --daily-cost 5 --on-exceed block   # set (server-admin)
//
// The quota is an operator cost ceiling: reading it needs only a tenant-admin
// (own scope), setting it needs a SERVER-admin key (a tenant raising its own
// budget would void the ceiling). A budget flag left unset — or negative —
// means unlimited (NULL).
package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// quotaView mirrors the server's quotaView wire shape.
type quotaView struct {
	Scope          string   `json:"scope"`
	DailyCostUSD   *float64 `json:"daily_cost_usd"`
	MonthlyCostUSD *float64 `json:"monthly_cost_usd"`
	DailyCalls     *int     `json:"daily_calls"`
	OnExceed       string   `json:"on_exceed"`
	Enabled        bool     `json:"enabled"`
	Unlimited      bool     `json:"unlimited"`
}

func quotaCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quota [scope]",
		Short: "Show a tenant's cost/call budget (set needs a server-admin key)",
		Long: "Show the per-tenant external-cost / call budget. No argument = the calling\n" +
			"key's own tenant (any tenant-admin); a scope argument needs a server-admin.\n" +
			"Use `ctx quota set <scope>` to write one (server-admin only).",
		Example: "  ctx quota                 # own tenant\n  ctx quota friend-tenant   # a scope (server-admin)\n  ctx quota set friend-tenant --daily-cost 5 --on-exceed block",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			data := json.RawMessage("{}")
			if len(args) == 1 {
				data = json.RawMessage(fmt.Sprintf(`{"scope":%q}`, args[0]))
			}
			return runQuotaGet(getClient, data)
		},
	}
	cmd.AddCommand(quotaSetCmd(getClient))
	return cmd
}

func quotaSetCmd(getClient func() (*Client, error)) *cobra.Command {
	var dailyCost, monthlyCost float64
	var dailyCalls int
	var onExceed string
	var disabled bool
	cmd := &cobra.Command{
		Use:     "set <scope>",
		Short:   "Set a tenant's budget (server-admin key required)",
		Example: "  ctx quota set friend-tenant --daily-cost 5\n  ctx quota set friend-tenant --daily-calls 1000 --on-exceed block\n  ctx quota set friend-tenant --disabled",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := map[string]any{"scope": args[0], "enabled": !disabled}
			// A budget flag is sent only when given; absent/negative = unlimited.
			if cmd.Flags().Changed("daily-cost") && dailyCost >= 0 {
				payload["daily_cost_usd"] = dailyCost
			}
			if cmd.Flags().Changed("monthly-cost") && monthlyCost >= 0 {
				payload["monthly_cost_usd"] = monthlyCost
			}
			if cmd.Flags().Changed("daily-calls") && dailyCalls >= 0 {
				payload["daily_calls"] = dailyCalls
			}
			if onExceed != "" {
				payload["on_exceed"] = onExceed
			}
			data, _ := json.Marshal(payload)
			return runQuotaSet(getClient, data)
		},
	}
	cmd.Flags().Float64Var(&dailyCost, "daily-cost", -1, "max external USD / 24h (negative = unlimited)")
	cmd.Flags().Float64Var(&monthlyCost, "monthly-cost", -1, "max external USD / 30d (negative = unlimited)")
	cmd.Flags().IntVar(&dailyCalls, "daily-calls", -1, "max attributed calls / 24h (negative = unlimited)")
	cmd.Flags().StringVar(&onExceed, "on-exceed", "", `action on cost exhaustion: "external_off" (default) or "block"`)
	cmd.Flags().BoolVar(&disabled, "disabled", false, "disable enforcement for this scope (keeps the row)")
	return cmd
}

func runQuotaGet(getClient func() (*Client, error), data json.RawMessage) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "tenant-quota-get", "data": data})
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	return renderQuota(resp)
}

func runQuotaSet(getClient func() (*Client, error), data json.RawMessage) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "tenant-quota-set", "data": data})
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return err
	}
	return renderQuota(resp)
}

func renderQuota(resp []byte) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Quota quotaView `json:"quota"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	q := payload.Quota
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "scope\t%s\n", q.Scope)
	if q.Unlimited {
		_, _ = fmt.Fprintf(w, "quota\tunlimited (no policy)\n")
		_ = w.Flush()
		return nil
	}
	_, _ = fmt.Fprintf(w, "enabled\t%v\n", q.Enabled)
	_, _ = fmt.Fprintf(w, "daily cost\t%s\n", quotaLimit(q.DailyCostUSD))
	_, _ = fmt.Fprintf(w, "monthly cost\t%s\n", quotaLimit(q.MonthlyCostUSD))
	_, _ = fmt.Fprintf(w, "daily calls\t%s\n", quotaCallLimit(q.DailyCalls))
	_, _ = fmt.Fprintf(w, "on exceed\t%s\n", q.OnExceed)
	_ = w.Flush()
	return nil
}

func quotaLimit(v *float64) string {
	if v == nil {
		return "unlimited"
	}
	return fmt.Sprintf("$%.4g", *v)
}

func quotaCallLimit(v *int) string {
	if v == nil {
		return "unlimited"
	}
	return fmt.Sprintf("%d", *v)
}

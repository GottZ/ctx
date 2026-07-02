// ctx tenant — tenant lifecycle, cross-tenant read grants, structural limits
// and usage (WF W14, workflow-ui plan design/03 §7, decision E5).
//
//	ctx tenant list                                    # all tenants (server-admin)
//	ctx tenant get <tenant-id>                         # one tenant (server-admin)
//	ctx tenant create <slug> <display name…>           # compound bootstrap (server-admin)
//	ctx tenant update <tenant-id> [--status S] [--display-name NAME]
//	ctx tenant delete <tenant-id>                      # full prune (server-admin)
//	ctx tenant usage [tenant-id]                       # own tenant (tenant-admin); id needs server-admin
//	ctx tenant limit set <tenant-id> --max-scopes N --max-keys N
//	ctx tenant grant create <grantee-tenant-id> <scope>
//	ctx tenant grant list [grantee-tenant-id]
//	ctx tenant grant delete <grant-id>
//
// Thin wrapper over the EXISTING tenant-*/tenant-grant-*/tenant-limit-set/
// tenant-usage-get manage actions — no new server surface. Every call is
// envelope-checked (tenantManage): an API failure such as the 403 "admin key
// required" becomes a clean command error (stderr + exit 1 via main.go),
// never a raw-JSON success exit. Cost budgets are NOT here — see `ctx quota`
// (tenant-quota-get/set).
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// tenantView mirrors the server's store.Tenant wire shape.
type tenantView struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
	MaxScopes   *int   `json:"max_scopes"` // nil = unlimited
	MaxKeys     *int   `json:"max_keys"`   // nil = unlimited
}

// tenantGrantView mirrors store.TenantGrant.
type tenantGrantView struct {
	ID            string `json:"id"`
	GranteeTenant string `json:"grantee_tenant"`
	GrantedScope  string `json:"granted_scope"`
	CreatedAt     string `json:"created_at"`
}

// tenantUsageView mirrors the tenant-usage-get "usage" object.
type tenantUsageView struct {
	TenantID   string `json:"tenant_id"`
	MaxScopes  *int   `json:"max_scopes"`
	MaxKeys    *int   `json:"max_keys"`
	ScopeCount int    `json:"scope_count"`
	KeyCount   int    `json:"key_count"`
}

func tenantCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tenant",
		Aliases: []string{"tn"},
		Short:   "Tenant lifecycle, read grants, limits and usage (mostly server-admin)",
		Long: "Manage tenants: a tenant owns scopes, blocks live in scopes (Model C).\n" +
			"Wraps the tenant-* manage actions: lifecycle (list/get/create/update/\n" +
			"delete), cross-tenant read grants, structural limits and usage counts.\n" +
			"Everything except `usage` needs a server-admin key; `usage` reads the\n" +
			"calling key's own tenant for any tenant-admin. Cost budgets: `ctx quota`.",
	}
	cmd.AddCommand(tenantListCmd(getClient))
	cmd.AddCommand(tenantGetCmd(getClient))
	cmd.AddCommand(tenantCreateCmd(getClient))
	cmd.AddCommand(tenantUpdateCmd(getClient))
	cmd.AddCommand(tenantDeleteCmd(getClient))
	cmd.AddCommand(tenantUsageCmd(getClient))
	cmd.AddCommand(tenantLimitCmd(getClient))
	cmd.AddCommand(tenantGrantCmd(getClient))

	// Default to list — the read-only entry point, like `ctx keys`.
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		return tenantListRun(getClient)
	}
	return cmd
}

// tenantManage posts one manage action and gates the envelope: success:false
// (403 admin gate, 404 unknown id, 400 validation, …) becomes a command error
// carrying the server's reason — cobra prints it to stderr and main.go exits 1.
func tenantManage(getClient func() (*Client, error), body map[string]any) ([]byte, error) {
	c, err := getClient()
	if err != nil {
		return nil, err
	}
	resp, err := c.Post("manage", body)
	if err != nil {
		return nil, err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// ── lifecycle ────────────────────────────────────────────────────────.

func tenantListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List every tenant (server-admin key required)",
		Args:    cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return tenantListRun(getClient)
		},
	}
}

func tenantListRun(getClient func() (*Client, error)) error {
	resp, err := tenantManage(getClient, map[string]any{"action": "tenant-list"})
	if err != nil {
		return err
	}
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Tenants []tenantView `json:"tenants"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	if len(payload.Tenants) == 0 {
		fmt.Println("No tenants.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tSLUG\tSTATUS\tSCOPES\tKEYS\tCREATED\tDISPLAY NAME")
	for _, t := range payload.Tenants {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Slug, t.Status, limitCell(t.MaxScopes), limitCell(t.MaxKeys),
			dateCell(t.CreatedAt), t.DisplayName)
	}
	return w.Flush()
}

func tenantGetCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "get <tenant-id>",
		Short: "Show one tenant incl. structural limits (server-admin key required)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			resp, err := tenantManage(getClient, map[string]any{"action": "tenant-get", "id": args[0]})
			if err != nil {
				return err
			}
			return renderTenant(resp)
		},
	}
}

func tenantCreateCmd(getClient func() (*Client, error)) *cobra.Command {
	var maxScopes, maxKeys int
	cmd := &cobra.Command{
		Use:   "create <slug> <display name...>",
		Short: "Create a tenant: registers <slug>:main and mints its owner key (server-admin)",
		Long: "Compound bootstrap in one transaction: tenant row, optional structural\n" +
			"limit seed, initial scope '<slug>:main', owner key. The owner-key\n" +
			"plaintext is shown EXACTLY ONCE — save it immediately.",
		Example: "  ctx tenant create friend \"Friend Corp\"\n" +
			"  ctx tenant create friend \"Friend Corp\" --max-scopes 10 --max-keys 20",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data := map[string]any{"slug": args[0], "display_name": strings.Join(args[1:], " ")}
			// Absent flag = keep the server-side default cap; negative = unlimited (null).
			if cmd.Flags().Changed("max-scopes") {
				data["max_scopes"] = nullableLimit(maxScopes)
			}
			if cmd.Flags().Changed("max-keys") {
				data["max_keys"] = nullableLimit(maxKeys)
			}
			resp, err := tenantManage(getClient, map[string]any{"action": "tenant-create", "data": data})
			if err != nil {
				return err
			}
			return renderTenantCreate(resp)
		},
	}
	cmd.Flags().IntVar(&maxScopes, "max-scopes", 0, "seed max scopes (negative = unlimited; absent = server default)")
	cmd.Flags().IntVar(&maxKeys, "max-keys", 0, "seed max active keys (negative = unlimited; absent = server default)")
	return cmd
}

func tenantUpdateCmd(getClient func() (*Client, error)) *cobra.Command {
	var status, displayName string
	cmd := &cobra.Command{
		Use:   "update <tenant-id>",
		Short: "Update a tenant's status and/or display name (server-admin key required)",
		Long: "Set the lifecycle status (active|suspended|offboarding) and/or the\n" +
			"display name. status=suspended bites at the tenant's next auth.",
		Example: "  ctx tenant update <id> --status suspended\n" +
			"  ctx tenant update <id> --display-name \"New Name\"",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if status == "" && displayName == "" {
				return fmt.Errorf("nothing to update: provide --status and/or --display-name")
			}
			body := map[string]any{"action": "tenant-update", "id": args[0]}
			if status != "" {
				body["status"] = status
			}
			if displayName != "" {
				body["data"] = map[string]any{"display_name": displayName}
			}
			resp, err := tenantManage(getClient, body)
			if err != nil {
				return err
			}
			return renderTenant(resp)
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "lifecycle status: active|suspended|offboarding")
	cmd.Flags().StringVar(&displayName, "display-name", "", "new display name")
	return cmd
}

func tenantDeleteCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <tenant-id>",
		Short: "Delete a tenant: full prune of its scoped data and keys (server-admin)",
		Long: "IRREVERSIBLE full prune: every block in the tenant's scopes, its keys,\n" +
			"then the tenant row. The default tenant is refused server-side (400).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			resp, err := tenantManage(getClient, map[string]any{"action": "tenant-delete", "id": args[0]})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── usage ────────────────────────────────────────────────────────────.

func tenantUsageCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "usage [tenant-id]",
		Short: "Show structural usage vs limits (own tenant for a tenant-admin)",
		Long: "Scope and active-key counts next to the tenant's limits. Without an id\n" +
			"this reads the calling key's OWN tenant (tenant-admin suffices); the\n" +
			"tenant-id argument targets another tenant and needs a server-admin key\n" +
			"(a non-server-admin is always pinned to its own tenant).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"action": "tenant-usage-get"}
			if len(args) == 1 {
				body["id"] = args[0]
			}
			resp, err := tenantManage(getClient, body)
			if err != nil {
				return err
			}
			return renderTenantUsage(resp)
		},
	}
}

// ── limits ───────────────────────────────────────────────────────────.

func tenantLimitCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "limit",
		Aliases: []string{"limits"},
		Short:   "Structural per-tenant caps (read via `tenant get` / `tenant usage`)",
	}
	cmd.AddCommand(tenantLimitSetCmd(getClient))
	return cmd
}

func tenantLimitSetCmd(getClient func() (*Client, error)) *cobra.Command {
	var maxScopes, maxKeys int
	cmd := &cobra.Command{
		Use:   "set <tenant-id>",
		Short: "Set the structural caps — REPLACE semantics, both flags required (server-admin)",
		Long: "Sets max scopes and max active keys for one tenant. Both flags are\n" +
			"mandatory (replace, not patch — no silent uncap); a negative value\n" +
			"means unlimited, 0 freezes creation for that dimension.",
		Example: "  ctx tenant limit set <id> --max-scopes 10 --max-keys 20\n" +
			"  ctx tenant limit set <id> --max-scopes -1 --max-keys 50",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("max-scopes") || !cmd.Flags().Changed("max-keys") {
				return fmt.Errorf("both --max-scopes and --max-keys are required (replace semantics; negative = unlimited)")
			}
			body := map[string]any{
				"action": "tenant-limit-set",
				"id":     args[0],
				"data": map[string]any{
					"max_scopes": nullableLimit(maxScopes),
					"max_keys":   nullableLimit(maxKeys),
				},
			}
			resp, err := tenantManage(getClient, body)
			if err != nil {
				return err
			}
			return renderTenant(resp)
		},
	}
	cmd.Flags().IntVar(&maxScopes, "max-scopes", 0, "max scopes (negative = unlimited, 0 = frozen)")
	cmd.Flags().IntVar(&maxKeys, "max-keys", 0, "max active keys (negative = unlimited, 0 = frozen)")
	return cmd
}

// ── grants ───────────────────────────────────────────────────────────.

func tenantGrantCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "grant",
		Short: "Cross-tenant read grants: share one of your scopes with another tenant",
		Long: "A grant gives a grantee tenant READ access to one scope (never write).\n" +
			"It takes effect at the grantee's next request; deleting it revokes\n" +
			"immediately. All grant subcommands need a server-admin key.",
	}
	cmd.AddCommand(tenantGrantCreateCmd(getClient))
	cmd.AddCommand(tenantGrantListCmd(getClient))
	cmd.AddCommand(tenantGrantDeleteCmd(getClient))
	return cmd
}

func tenantGrantCreateCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "create <grantee-tenant-id> <scope>",
		Aliases: []string{"add"},
		Short:   "Grant a tenant read access to a scope (server-admin key required)",
		Example: "  ctx tenant grant create 11111111-2222-3333-4444-555566667777 work",
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			resp, err := tenantManage(getClient, map[string]any{
				"action": "tenant-grant-create",
				"data":   map[string]string{"grantee_tenant": args[0], "granted_scope": args[1]},
			})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

func tenantGrantListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list [grantee-tenant-id]",
		Aliases: []string{"ls"},
		Short:   "List read grants, optionally for one grantee tenant (server-admin)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"action": "tenant-grant-list"}
			if len(args) == 1 {
				body["id"] = args[0]
			}
			resp, err := tenantManage(getClient, body)
			if err != nil {
				return err
			}
			return renderTenantGrants(resp)
		},
	}
}

func tenantGrantDeleteCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "delete <grant-id>",
		Aliases: []string{"rm", "remove"},
		Short:   "Revoke a read grant — effective immediately (server-admin key required)",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			resp, err := tenantManage(getClient, map[string]any{"action": "tenant-grant-delete", "id": args[0]})
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// ── rendering ────────────────────────────────────────────────────────.
// TTY: tabwriter tables / key-value blocks. Pipe: pretty JSON (scriptable),
// same convention as `ctx quota` and `ctx dream stats`.

func renderTenant(resp []byte) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Tenant tenantView `json:"tenant"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	writeTenantKV(payload.Tenant)
	return nil
}

func writeTenantKV(t tenantView) {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "id\t%s\n", t.ID)
	_, _ = fmt.Fprintf(w, "slug\t%s\n", t.Slug)
	_, _ = fmt.Fprintf(w, "display name\t%s\n", t.DisplayName)
	_, _ = fmt.Fprintf(w, "status\t%s\n", t.Status)
	_, _ = fmt.Fprintf(w, "max scopes\t%s\n", limitCell(t.MaxScopes))
	_, _ = fmt.Fprintf(w, "max keys\t%s\n", limitCell(t.MaxKeys))
	_, _ = fmt.Fprintf(w, "created\t%s\n", dateCell(t.CreatedAt))
	_ = w.Flush()
}

func renderTenantCreate(resp []byte) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Tenant     tenantView `json:"tenant"`
		Scope      string     `json:"scope"`
		OwnerKeyID string     `json:"owner_key_id"`
		OwnerKey   string     `json:"owner_key"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	fmt.Printf("Tenant created: %s\n\n", payload.Tenant.DisplayName)
	writeTenantKV(payload.Tenant)
	fmt.Printf("\n  initial scope: %s\n", payload.Scope)
	fmt.Printf("  owner_key_id:  %s\n", payload.OwnerKeyID)
	fmt.Printf("  owner_key:     %s\n", payload.OwnerKey)
	fmt.Printf("\n  Save the owner key now — it cannot be retrieved later.\n")
	return nil
}

func renderTenantUsage(resp []byte) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Usage tenantUsageView `json:"usage"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	u := payload.Usage
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "tenant\t%s\n", u.TenantID)
	_, _ = fmt.Fprintf(w, "scopes\t%d / %s\n", u.ScopeCount, limitCell(u.MaxScopes))
	_, _ = fmt.Fprintf(w, "active keys\t%d / %s\n", u.KeyCount, limitCell(u.MaxKeys))
	_ = w.Flush()
	return nil
}

func renderTenantGrants(resp []byte) error {
	if !StdoutIsTTY() {
		PrintJSON(resp)
		return nil
	}
	var payload struct {
		Grants []tenantGrantView `json:"grants"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		PrintJSON(resp)
		return err
	}
	if len(payload.Grants) == 0 {
		fmt.Println("No grants.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tGRANTEE TENANT\tSCOPE\tCREATED")
	for _, g := range payload.Grants {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", g.ID, g.GranteeTenant, g.GrantedScope, dateCell(g.CreatedAt))
	}
	return w.Flush()
}

// ── helpers ──────────────────────────────────────────────────────────.

// nullableLimit maps the CLI flag convention (negative = unlimited, same as
// `ctx quota set`) onto the API convention (JSON null = unlimited).
func nullableLimit(v int) any {
	if v < 0 {
		return nil
	}
	return v
}

// limitCell renders a nullable structural cap for table output.
func limitCell(v *int) string {
	if v == nil {
		return "unlimited"
	}
	return fmt.Sprintf("%d", *v)
}

// dateCell shortens an RFC3339 timestamp to its date for table output.
func dateCell(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

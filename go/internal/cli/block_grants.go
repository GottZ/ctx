// ctx block-grant — row-level read sharing of a SINGLE block (MT T43, 07-W6).
//
//	ctx block-grant create <block-id> <grantee-tenant>   # share one block (server-admin)
//	ctx block-grant list [block-id]                       # what have I shared, to whom
//	ctx block-grant revoke <block-id> <grantee-tenant>    # un-share
//
// All three need a SERVER-admin key; create/revoke additionally require that the
// calling tenant OWNS the block, and a CROSS-tenant create needs the tenant's
// `tenant.allow_cross_tenant_block_grant` opt-in (default off). The block stays in
// its scope — the grant only makes it additively READABLE for the grantee, and a
// revoke (or a block delete) drops it immediately.
package cli

import (
	"github.com/spf13/cobra"
)

func blockGrantCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "block-grant",
		Aliases: []string{"bg"},
		Short:   "Share a single block with another tenant (read-only, revocable)",
		Long: "Row-level read sharing: grant a grantee tenant read access to ONE block\n" +
			"without changing its scope. Server-admin key required; cross-tenant grants\n" +
			"need the tenant's allow_cross_tenant_block_grant opt-in.",
	}
	cmd.AddCommand(blockGrantCreateCmd(getClient))
	cmd.AddCommand(blockGrantListCmd(getClient))
	cmd.AddCommand(blockGrantRevokeCmd(getClient))
	return cmd
}

func blockGrantCreateCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "create <block-id> <grantee-tenant>",
		Aliases: []string{"add"},
		Short:   "Share one block with a grantee tenant",
		Example: "  ctx block-grant create 019d25d8-b8aa-7f02-8ad0-e0bba7b7cfcf 11111111-2222-3333-4444-555566667777",
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return blockGrantPost(getClient, "block-grant-create", args[0], args[1])
		},
	}
}

func blockGrantRevokeCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "revoke <block-id> <grantee-tenant>",
		Aliases: []string{"rm", "delete"},
		Short:   "Un-share a block (takes effect at the grantee's next request)",
		Args:    cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			return blockGrantPost(getClient, "block-grant-revoke", args[0], args[1])
		},
	}
}

func blockGrantListCmd(getClient func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "list [block-id]",
		Aliases: []string{"ls"},
		Short:   "List the grants made by your tenant (optionally for one block)",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}
			body := map[string]any{"action": "block-grant-list"}
			if len(args) == 1 {
				body["id"] = args[0]
			}
			resp, err := c.Post("manage", body)
			if err != nil {
				return err
			}
			PrintJSON(resp)
			return nil
		},
	}
}

// blockGrantPost issues a create/revoke manage call with the (block_id,
// grantee_tenant) data payload. The server decodes the nested object into the
// manageRequest.Data json.RawMessage, so a plain map is enough — no pre-marshal.
func blockGrantPost(getClient func() (*Client, error), action, blockID, granteeTenant string) error {
	c, err := getClient()
	if err != nil {
		return err
	}
	resp, err := c.Post("manage", map[string]any{
		"action": action,
		"data":   map[string]string{"block_id": blockID, "grantee_tenant": granteeTenant},
	})
	if err != nil {
		return err
	}
	PrintJSON(resp)
	return nil
}

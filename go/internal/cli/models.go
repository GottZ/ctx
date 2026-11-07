// ctx models — the model API administration: consumers (external programs
// that call /v1 with a ctxm_ key) and the model aliases they may be granted
// (OpenAI-compatible API layer, RC-D wave W05-19).
//
//	ctx models consumers list [--status S,…] [--q TEXT] [--tenant ID] [--limit N] [--cursor C]
//	ctx models consumers create --label L [policy flags…]   # mints a key: shown ONCE
//	ctx models consumers rotate <consumer-id> [--grace D] [--expires T]
//	ctx models consumers revoke <key-id>
//	ctx models consumers deactivate <consumer-id>
//	ctx models aliases list
//
// Thin wrapper over the EXISTING model-consumer-*/model-key-*/model-alias-list
// manage actions — no new server surface. The output is a projection over the
// typed views of the server's answer (an allowlist of fields), never the raw
// body: a list never echoes a field the server did not document. The
// plaintext of a minted key (create, rotate) appears exactly once on stdout
// and never on stderr or in an error.
//
// S1 (contract + stubs): every command and flag is registered so `--help` is
// final; every RunE answers errModelsNotImplemented without a request.
package cli

import (
	"errors"
	"time"

	"github.com/spf13/cobra"
)

// errModelsNotImplemented is the S1 stub answer of every `ctx models` command.
var errModelsNotImplemented = errors.New("ctx models: not implemented")

// modelsStdoutIsTTY is the output-mode seam (TTY table vs. JSON); tests swap it.
var modelsStdoutIsTTY = StdoutIsTTY

func modelsCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Model API administration: consumers, their keys, and model aliases",
		Long: "Manage the consumers of the OpenAI-compatible /v1 API: external programs\n" +
			"that authenticate with a ctxm_ key. A consumer belongs to one tenant;\n" +
			"its keys are minted on create and rotate and shown exactly once.\n" +
			"Model aliases are the names a consumer may be granted.",
	}
	cmd.AddCommand(modelsConsumersCmd(getClient))
	cmd.AddCommand(modelsAliasesCmd(getClient))
	return cmd
}

func modelsConsumersCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "consumers",
		Aliases: []string{"consumer"},
		Short:   "Model API consumers and their keys",
		Long: "List, create and deactivate model API consumers; rotate and revoke\n" +
			"their keys. create and rotate are self-service (when the server allows\n" +
			"it); list, revoke and deactivate need a tenant-admin key.",
	}
	cmd.AddCommand(modelsConsumersListCmd(getClient))
	cmd.AddCommand(modelsConsumersCreateCmd(getClient))
	cmd.AddCommand(modelsConsumersRotateCmd(getClient))
	cmd.AddCommand(modelsConsumersRevokeCmd(getClient))
	cmd.AddCommand(modelsConsumersDeactivateCmd(getClient))
	return cmd
}

// modelsNotImplemented is the S1 RunE of every runnable `ctx models` command.
func modelsNotImplemented(_ *cobra.Command, _ []string) error {
	return errModelsNotImplemented
}

// modelsListOpts are the flags of `ctx models consumers list`.
type modelsListOpts struct {
	status []string
	q      string
	tenant string
	limit  int
	cursor string
	json   bool
}

func modelsConsumersListCmd(_ func() (*Client, error)) *cobra.Command {
	var o modelsListOpts
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List model API consumers with their key status (tenant-admin key required)",
		Long: "Lists consumers, newest first, keyset-paginated: pass next_cursor back\n" +
			"with --cursor for the next page. TTY: table; pipe or --json: JSON.\n" +
			"No key plaintext is ever listed — only each key's hint.",
		Args: cobra.NoArgs,
		RunE: modelsNotImplemented,
	}
	f := cmd.Flags()
	f.StringSliceVar(&o.status, "status", nil, "statuses to list: active|expiring|rotating|no_key|inactive, or all (default: server default)")
	f.StringVar(&o.q, "q", "", "filter by label text")
	f.StringVar(&o.tenant, "tenant", "", "tenant id (uuid) to list; another tenant than your key's own needs a server-admin key")
	f.IntVar(&o.limit, "limit", 0, "page size (> 0; absent = server default)")
	f.StringVar(&o.cursor, "cursor", "", "opaque cursor from a previous page's next_cursor")
	f.BoolVar(&o.json, "json", false, "print JSON even on a terminal")
	return cmd
}

// modelsCreateOpts are the flags of `ctx models consumers create`.
type modelsCreateOpts struct {
	label          string
	description    string
	tenant         string
	tenantScope    string
	models         []string
	allModels      bool
	sensitivity    string
	lane           string
	fairWeight     int
	cidrs          []string
	allowPlaintext bool
	expires        string
	json           bool
}

func modelsConsumersCreateCmd(_ func() (*Client, error)) *cobra.Command {
	var o modelsCreateOpts
	cmd := &cobra.Command{
		Use:   "create --label <label>",
		Short: "Create a consumer and mint its first key — the key is shown ONCE (self-service)",
		Long: "Creates an app consumer and mints its first ctxm_ key. The plaintext is\n" +
			"printed exactly once — save it immediately; it cannot be retrieved\n" +
			"later (rotate mints a new one). Only the flags you set are sent;\n" +
			"everything else takes the server's default policy.",
		Example: "  ctx models consumers create --label ci-bot --model <alias-id>\n" +
			"  ctx models consumers create --label batch --lane bulk --sensitivity public --expires 2027-01-01T00:00:00Z",
		Args: cobra.NoArgs,
		RunE: modelsNotImplemented,
	}
	f := cmd.Flags()
	f.StringVar(&o.label, "label", "", "consumer label (required)")
	f.StringVar(&o.description, "description", "", "free-text description")
	f.StringVar(&o.tenant, "tenant", "", "tenant id (uuid) that owns the consumer (server-admin key required; default: your key's tenant)")
	f.StringVar(&o.tenantScope, "tenant-scope", "", "scope of the consumer's own tenant it resolves in (a scope belongs to one tenant)")
	f.StringSliceVar(&o.models, "model", nil, "model alias id to grant (repeatable)")
	f.BoolVar(&o.allModels, "all-models", false, "grant every model alias offered to the consumer")
	f.StringVar(&o.sensitivity, "sensitivity", "", "highest prompt sensitivity: credentials|personal|internal|public")
	f.StringVar(&o.lane, "lane", "", "admission lane: urgent|bulk")
	f.IntVar(&o.fairWeight, "fair-weight", 0, "fair-share weight, 1..100")
	f.StringSliceVar(&o.cidrs, "cidr", nil, "allowed client CIDR (repeatable)")
	f.BoolVar(&o.allowPlaintext, "allow-plaintext", false, "allow the key over plain http")
	f.StringVar(&o.expires, "expires", "", "key expiry, RFC3339 (default: the server's maximum key lifetime)")
	f.BoolVar(&o.json, "json", false, "print JSON even on a terminal")
	return cmd
}

// modelsRotateOpts are the flags of `ctx models consumers rotate`.
type modelsRotateOpts struct {
	grace   time.Duration
	expires string
	json    bool
}

func modelsConsumersRotateCmd(_ func() (*Client, error)) *cobra.Command {
	var o modelsRotateOpts
	cmd := &cobra.Command{
		Use:   "rotate <consumer-id>",
		Short: "Mint a new key for a consumer — the key is shown ONCE (self-service)",
		Long: "Mints a successor key. The predecessor stays valid for --grace (sent as\n" +
			"whole seconds; 0s revokes it at once, the leak case; absent = the\n" +
			"server's gateway.key_rotation_grace).",
		Example: "  ctx models consumers rotate <consumer-id>\n" +
			"  ctx models consumers rotate <consumer-id> --grace 0s",
		Args: cobra.ExactArgs(1),
		RunE: modelsNotImplemented,
	}
	f := cmd.Flags()
	f.DurationVar(&o.grace, "grace", 0, "predecessor grace period, e.g. 24h or 0s (>= 0; absent = server default)")
	f.StringVar(&o.expires, "expires", "", "new key's expiry, RFC3339 (default: the server's maximum key lifetime)")
	f.BoolVar(&o.json, "json", false, "print JSON even on a terminal")
	return cmd
}

func modelsConsumersRevokeCmd(_ func() (*Client, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke one key and end its running requests (tenant-admin key required)",
		Long: "Revokes the key immediately and ends the requests running on it in\n" +
			"this ctx process; prints cancelled_inflight (unknown when the server\n" +
			"cannot count).",
		Args: cobra.ExactArgs(1),
		RunE: modelsNotImplemented,
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON even on a terminal")
	return cmd
}

func modelsConsumersDeactivateCmd(_ func() (*Client, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "deactivate <consumer-id>",
		Short: "Deactivate a consumer: revoke all its keys, end its requests (tenant-admin)",
		Long: "Soft delete: the consumer stays listed as inactive, all its keys are\n" +
			"revoked and its running requests end; prints cancelled_inflight\n" +
			"(unknown when the server cannot count).",
		Args: cobra.ExactArgs(1),
		RunE: modelsNotImplemented,
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON even on a terminal")
	return cmd
}

func modelsAliasesCmd(getClient func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "aliases",
		Aliases: []string{"alias"},
		Short:   "Model aliases visible to your key",
	}
	cmd.AddCommand(modelsAliasesListCmd(getClient))
	return cmd
}

func modelsAliasesListCmd(_ func() (*Client, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the model aliases your key can read (tenant-admin key required)",
		Long: "Lists the aliases of your tenant's scope and the shipped global ones,\n" +
			"with writable telling which your key may change. TTY: table; pipe or\n" +
			"--json: JSON.",
		Args: cobra.NoArgs,
		RunE: modelsNotImplemented,
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON even on a terminal")
	return cmd
}

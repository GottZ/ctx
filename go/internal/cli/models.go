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
// manage actions — no new server surface. Three rules hold for every command:
//
//   - The output is a projection over the typed views below (an allowlist of
//     the documented fields), never the raw body: a list never echoes a field
//     the server did not document, whatever the answer carries.
//   - The plaintext of a minted key (create, rotate) appears exactly once on
//     stdout and never on stderr or in an error. It is taken out of the answer
//     BEFORE the envelope is judged, so an answer that is broken in any other
//     way still hands the key over before the command exits 1.
//   - Every flag is validated before the request; only flags that were set
//     are sent (cmd.Flags().Changed), so absent ones keep the server default.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/spf13/cobra"
)

// modelsStdoutIsTTY is the output-mode seam (TTY table vs. JSON); tests swap it.
var modelsStdoutIsTTY = StdoutIsTTY

// modelsKeyWarning follows the plaintext on a terminal (never carries the key).
const modelsKeyWarning = "Save the key now — it cannot be retrieved later."

// The client-side vocabularies mirror the server's checks, so a typo fails
// before the request: the statuses of model-consumer-list
// (handler/model_consumers_read.go listOptions) and the lanes and the
// fair_weight range of migration 161 (handler/model_consumers.go
// validatePolicyValues). The sensitivities come from backends.ValidSensitivity.
var (
	modelsStatuses = []string{"active", "expiring", "rotating", "no_key", "inactive", "all"}
	modelsLanes    = []string{"urgent", "bulk"}
)

const (
	modelsFairWeightMin = 1
	modelsFairWeightMax = 100
)

// ── views: the allowlist of what the CLI prints ──────────────────────────────.

// modelsKeyView is the server's keyView (handler/model_consumers_read.go).
// The hint is "ctxm_…" plus the key's tail — never the plaintext.
type modelsKeyView struct {
	ID          string  `json:"id"`
	Hint        string  `json:"hint"`
	Status      string  `json:"status"`
	CreatedAt   string  `json:"created_at"`
	ExpiresAt   *string `json:"expires_at"`
	RevokedAt   *string `json:"revoked_at"`
	RotatedFrom *string `json:"rotated_from"`
	LastUsedAt  *string `json:"last_used_at"`
}

// modelsPeerView is the server's peerView (server-admin only).
type modelsPeerView struct {
	InstanceID string `json:"instance_id"`
	BoundAt    string `json:"bound_at"`
}

// modelsConsumerView is the server's consumerView.
type modelsConsumerView struct {
	ID                string          `json:"id"`
	TenantID          string          `json:"tenant_id"`
	TenantSlug        string          `json:"tenant_slug"`
	Label             string          `json:"label"`
	Description       string          `json:"description"`
	Kind              string          `json:"kind"`
	TenantScope       *string         `json:"tenant_scope"`
	AllModels         bool            `json:"all_models"`
	Models            []string        `json:"models"`
	PromptSensitivity string          `json:"prompt_sensitivity"`
	FairWeight        int             `json:"fair_weight"`
	AdmissionLane     string          `json:"admission_lane"`
	AllowedCIDRs      []string        `json:"allowed_cidrs"`
	AllowPlaintext    bool            `json:"allow_plaintext"`
	Active            bool            `json:"active"`
	Status            string          `json:"status"`
	LastUsedAt        *string         `json:"last_used_at"`
	CreatedAt         string          `json:"created_at"`
	CreatedByLabel    *string         `json:"created_by_label"`
	Keys              []modelsKeyView `json:"keys"`
	KeysTotal         int             `json:"keys_total"`
	Peer              *modelsPeerView `json:"peer"`
}

// modelsAliasView is one model-alias-list item (handler modelAliasView plus
// writable). policy is the alias's policy object, passed as the server wrote it.
type modelsAliasView struct {
	ID          string          `json:"id"`
	Scope       string          `json:"scope"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	Role        string          `json:"role"`
	Sensitivity string          `json:"sensitivity"`
	Policy      json.RawMessage `json:"policy"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Writable    bool            `json:"writable"`
}

// modelsConsumerPage is the projection of a model-consumer-list answer.
type modelsConsumerPage struct {
	Consumers  []modelsConsumerView `json:"consumers"`
	NextCursor *string              `json:"next_cursor"`
}

// modelsAliasPage is the projection of a model-alias-list answer.
type modelsAliasPage struct {
	ModelAliases []modelsAliasView `json:"model_aliases"`
	Count        int               `json:"count"`
}

// modelsInflight is the projection of a model-key-revoke or
// model-consumer-delete answer; nil = the server could not count.
type modelsInflight struct {
	CancelledInflight *int `json:"cancelled_inflight"`
}

// ── command tree ─────────────────────────────────────────────────────────────.

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

// modelsListOpts are the flags of `ctx models consumers list`.
type modelsListOpts struct {
	status []string
	q      string
	tenant string
	limit  int
	cursor string
	json   bool
}

func modelsConsumersListCmd(getClient func() (*Client, error)) *cobra.Command {
	var o modelsListOpts
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List model API consumers with their key status (tenant-admin key required)",
		Long: "Lists consumers, newest first, keyset-paginated: pass next_cursor back\n" +
			"with --cursor for the next page. TTY: table; pipe or --json: JSON.\n" +
			"No key plaintext is ever listed — only each key's hint.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd, getClient)
		},
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

func modelsConsumersCreateCmd(getClient func() (*Client, error)) *cobra.Command {
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd, getClient)
		},
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

func modelsConsumersRotateCmd(getClient func() (*Client, error)) *cobra.Command {
	var o modelsRotateOpts
	cmd := &cobra.Command{
		Use:   "rotate <consumer-id>",
		Short: "Mint a new key for a consumer — the key is shown ONCE (self-service)",
		Long: "Mints a successor key. The predecessor stays valid for --grace (sent as\n" +
			"whole seconds — a fraction of a second is refused, never rounded; 0s\n" +
			"revokes it at once, the leak case; absent = the server's\n" +
			"gateway.key_rotation_grace).",
		Example: "  ctx models consumers rotate <consumer-id>\n" +
			"  ctx models consumers rotate <consumer-id> --grace 0s",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return o.run(cmd, getClient, args[0])
		},
	}
	f := cmd.Flags()
	f.DurationVar(&o.grace, "grace", 0, "predecessor grace period in whole seconds, e.g. 24h or 0s (>= 0; absent = server default)")
	f.StringVar(&o.expires, "expires", "", "new key's expiry, RFC3339 (default: the server's maximum key lifetime)")
	f.BoolVar(&o.json, "json", false, "print JSON even on a terminal")
	return cmd
}

func modelsConsumersRevokeCmd(getClient func() (*Client, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke one key and end its running requests (tenant-admin key required)",
		Long: "Revokes the key immediately and ends the requests running on it in\n" +
			"this ctx process; prints cancelled_inflight (unknown when the server\n" +
			"cannot count).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return modelsRevokeRun(getClient, "model-key-revoke", "key_id", args[0], "Key revoked: ", asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON even on a terminal")
	return cmd
}

func modelsConsumersDeactivateCmd(getClient func() (*Client, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "deactivate <consumer-id>",
		Short: "Deactivate a consumer: revoke all its keys, end its requests (tenant-admin)",
		Long: "Soft delete: the consumer stays listed as inactive, all its keys are\n" +
			"revoked and its running requests end; prints cancelled_inflight\n" +
			"(unknown when the server cannot count).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return modelsRevokeRun(getClient, "model-consumer-delete", "id", args[0], "Consumer deactivated: ", asJSON)
		},
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

func modelsAliasesListCmd(getClient func() (*Client, error)) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the model aliases your key can read (tenant-admin key required)",
		Long: "Lists the aliases of your tenant's scope and the shipped global ones,\n" +
			"with writable telling which your key may change. TTY: table; pipe or\n" +
			"--json: JSON.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return modelsAliasesRun(getClient, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON even on a terminal")
	return cmd
}

// ── runs ─────────────────────────────────────────────────────────────────────.

func (o *modelsListOpts) run(cmd *cobra.Command, getClient func() (*Client, error)) error {
	if err := o.validate(cmd); err != nil {
		return err
	}
	resp, err := modelsManage(getClient, "model-consumer-list", o.data(cmd))
	if err != nil {
		return err
	}
	var page modelsConsumerPage
	if err := json.Unmarshal(resp, &page); err != nil {
		return fmt.Errorf("ctx models: undecodable consumer list: %w", err)
	}
	if page.Consumers == nil {
		page.Consumers = []modelsConsumerView{}
	}
	if modelsWantJSON(o.json) {
		return modelsPrintJSON(page)
	}
	return renderModelsConsumers(page)
}

func (o *modelsCreateOpts) run(cmd *cobra.Command, getClient func() (*Client, error)) error {
	if err := o.validate(cmd); err != nil {
		return err
	}
	return modelsMintRun(getClient, "model-consumer-create", o.data(cmd), modelsConsumerMint, o.json)
}

func (o *modelsRotateOpts) run(cmd *cobra.Command, getClient func() (*Client, error), consumerID string) error {
	if err := o.validate(cmd); err != nil {
		return err
	}
	return modelsMintRun(getClient, "model-key-rotate", o.data(cmd, consumerID), modelsKeyMint, o.json)
}

func modelsRevokeRun(getClient func() (*Client, error), action, idField, id, done string, asJSON bool) error {
	resp, err := modelsManage(getClient, action, map[string]any{idField: id})
	if err != nil {
		return err
	}
	var out modelsInflight
	if err := json.Unmarshal(resp, &out); err != nil {
		return fmt.Errorf("ctx models: undecodable answer: %w", err)
	}
	if modelsWantJSON(asJSON) {
		return modelsPrintJSON(out)
	}
	cell := "unknown"
	if out.CancelledInflight != nil {
		cell = strconv.Itoa(*out.CancelledInflight)
	}
	fmt.Printf("%s%s\ncancelled_inflight: %s\n", done, sanitizeTerminal(id), cell)
	return nil
}

func modelsAliasesRun(getClient func() (*Client, error), asJSON bool) error {
	resp, err := modelsManage(getClient, "model-alias-list", nil)
	if err != nil {
		return err
	}
	var page modelsAliasPage
	if err := json.Unmarshal(resp, &page); err != nil {
		return fmt.Errorf("ctx models: undecodable alias list: %w", err)
	}
	if page.ModelAliases == nil {
		page.ModelAliases = []modelsAliasView{}
	}
	if modelsWantJSON(asJSON) {
		return modelsPrintJSON(page)
	}
	return renderModelsAliases(page)
}

// ── validation and wire bodies ───────────────────────────────────────────────.

func (o *modelsListOpts) validate(cmd *cobra.Command) error {
	if cmd.Flags().Changed("limit") && o.limit <= 0 {
		return fmt.Errorf("--limit must be > 0, got %d", o.limit)
	}
	for _, s := range o.status {
		if !modelsOneOf(s, modelsStatuses) {
			return fmt.Errorf("--status %q: want one of %s", s, strings.Join(modelsStatuses, ", "))
		}
	}
	return nil
}

func (o *modelsListOpts) data(cmd *cobra.Command) map[string]any {
	set := modelsSetter(cmd, map[string]any{})
	set("status", "status", o.status)
	set("q", "q", o.q)
	set("tenant", "tenant_id", o.tenant)
	set("limit", "limit", o.limit)
	set("cursor", "cursor", o.cursor)
	return set("", "", nil)
}

func (o *modelsCreateOpts) validate(cmd *cobra.Command) error {
	changed := cmd.Flags().Changed
	switch {
	case o.label == "":
		return errors.New("--label is required")
	case changed("lane") && !modelsOneOf(o.lane, modelsLanes):
		return fmt.Errorf("--lane %q: want urgent or bulk", o.lane)
	case changed("sensitivity") && !backends.ValidSensitivity(backends.Sensitivity(o.sensitivity)):
		return fmt.Errorf("--sensitivity %q: want credentials, personal, internal or public", o.sensitivity)
	case changed("fair-weight") && (o.fairWeight < modelsFairWeightMin || o.fairWeight > modelsFairWeightMax):
		return fmt.Errorf("--fair-weight %d: want %d..%d", o.fairWeight, modelsFairWeightMin, modelsFairWeightMax)
	case changed("expires"):
		return modelsCheckRFC3339("expires", o.expires)
	}
	return nil
}

func (o *modelsCreateOpts) data(cmd *cobra.Command) map[string]any {
	set := modelsSetter(cmd, map[string]any{"label": o.label})
	set("description", "description", o.description)
	set("tenant", "tenant_id", o.tenant)
	set("tenant-scope", "tenant_scope", o.tenantScope)
	set("model", "models", o.models)
	set("all-models", "all_models", o.allModels)
	set("sensitivity", "prompt_sensitivity", o.sensitivity)
	set("lane", "admission_lane", o.lane)
	set("fair-weight", "fair_weight", o.fairWeight)
	set("cidr", "allowed_cidrs", o.cidrs)
	set("allow-plaintext", "allow_plaintext", o.allowPlaintext)
	set("expires", "expires_at", o.expires)
	return set("", "", nil)
}

// validate refuses a negative or fractional --grace: grace is a security
// parameter (how long a possibly leaked predecessor stays valid), so a value
// the wire cannot carry exactly is an error, never rounded.
func (o *modelsRotateOpts) validate(cmd *cobra.Command) error {
	changed := cmd.Flags().Changed
	switch {
	case changed("grace") && o.grace < 0:
		return fmt.Errorf("--grace %s: must be >= 0", o.grace)
	case changed("grace") && o.grace%time.Second != 0:
		return fmt.Errorf("--grace %s: must be a whole number of seconds", o.grace)
	case changed("expires"):
		return modelsCheckRFC3339("expires", o.expires)
	}
	return nil
}

func (o *modelsRotateOpts) data(cmd *cobra.Command, consumerID string) map[string]any {
	set := modelsSetter(cmd, map[string]any{"consumer_id": consumerID})
	set("grace", "grace", int64(o.grace/time.Second))
	set("expires", "expires_at", o.expires)
	return set("", "", nil)
}

// modelsSetter returns set(flag, key, v): it stores v under key only when the
// flag was given on the command line; set("", "", nil) returns the map.
func modelsSetter(cmd *cobra.Command, data map[string]any) func(flag, key string, v any) map[string]any {
	return func(flag, key string, v any) map[string]any {
		if flag != "" && cmd.Flags().Changed(flag) {
			data[key] = v
		}
		return data
	}
}

func modelsCheckRFC3339(flag, v string) error {
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return fmt.Errorf("--%s %q: want an RFC3339 timestamp, e.g. 2027-01-01T00:00:00Z", flag, v)
	}
	return nil
}

func modelsOneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// ── transport ────────────────────────────────────────────────────────────────.

// modelsPost sends one manage action; data nil sends no data field.
func modelsPost(getClient func() (*Client, error), action string, data map[string]any) ([]byte, error) {
	c, err := getClient()
	if err != nil {
		return nil, err
	}
	body := map[string]any{"action": action}
	if data != nil {
		body["data"] = data
	}
	return c.Post("manage", body)
}

// modelsManage is modelsPost plus the envelope gate, for every action that
// mints no key: success:false becomes the command error (exit 1) with the
// server's reason, before anything is printed.
func modelsManage(getClient func() (*Client, error), action string, data map[string]any) ([]byte, error) {
	resp, err := modelsPost(getClient, action, data)
	if err != nil {
		return nil, err
	}
	if err := checkEnvelope(resp, envelopeRequired); err != nil {
		return nil, err
	}
	return resp, nil
}

func modelsWantJSON(asJSON bool) bool { return asJSON || !modelsStdoutIsTTY() }

func modelsPrintJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("ctx models: encoding output: %w", err)
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

// ── reveal-once ──────────────────────────────────────────────────────────────.

// modelsMint describes one key-minting answer: the object that comes with the
// key ("consumer" for create, "key" for rotate), its decoder (the projection;
// false = missing or without an id) and its terminal rendering.
type modelsMint struct {
	field  string
	decode func(json.RawMessage) (any, bool)
	render func(any)
}

var (
	modelsConsumerMint = modelsMint{field: "consumer", decode: decodeModelsConsumer, render: renderModelsConsumerKV}
	modelsKeyMint      = modelsMint{field: "key", decode: decodeModelsKey, render: renderModelsKeyKV}
)

func decodeModelsConsumer(raw json.RawMessage) (any, bool) {
	var c modelsConsumerView
	if json.Unmarshal(raw, &c) != nil || c.ID == "" {
		return nil, false
	}
	return c, true
}

func decodeModelsKey(raw json.RawMessage) (any, bool) {
	var k modelsKeyView
	if json.Unmarshal(raw, &k) != nil || k.ID == "" {
		return nil, false
	}
	return k, true
}

// modelsMintRun posts a key-minting action and hands the key over exactly once.
func modelsMintRun(getClient func() (*Client, error), action string, data map[string]any, m modelsMint, asJSON bool) error {
	resp, err := modelsPost(getClient, action, data)
	if err != nil {
		return err
	}
	return modelsReveal(resp, m, asJSON)
}

// modelsReveal is the reveal-once path. The key is taken out of the answer
// FIRST; only then are the envelope and the object judged. With a key in
// hand, every defect is reported after the key went to stdout, and no error
// ever quotes the body (checkEnvelope's "request failed: <body>" would carry
// the key to stderr). Without a key nothing is at stake and the package's
// envelope contract applies unchanged.
func modelsReveal(resp []byte, m modelsMint, asJSON bool) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(resp, &fields) != nil {
		if bytes.Contains(resp, []byte(modelkey.Prefix)) {
			return errors.New("ctx models: unparseable answer that may carry a key (body withheld); check the consumer's keys with `ctx models consumers list`")
		}
		return checkEnvelope(resp, envelopeRequired)
	}
	var key string
	_ = json.Unmarshal(fields["api_key"], &key)
	if key == "" {
		if err := checkEnvelope(resp, envelopeRequired); err != nil {
			return err
		}
		return errors.New("ctx models: the answer carries no api_key")
	}
	envErr := modelsMintEnvelope(fields, key)
	obj, ok := m.decode(fields[m.field])
	if envErr == nil && ok {
		return modelsEmitMinted(m, obj, key, asJSON)
	}
	if err := modelsEmitKeyOnly(key, asJSON); err != nil {
		return err
	}
	if envErr != nil {
		return envErr
	}
	return fmt.Errorf("ctx models: the answer carries a key but no valid %s (the key was printed on stdout)", m.field)
}

// modelsMintEnvelope judges the envelope of an answer that carries a key,
// without ever quoting the body; the server's reason is redacted of the key.
func modelsMintEnvelope(fields map[string]json.RawMessage, key string) error {
	var success *bool
	_ = json.Unmarshal(fields["success"], &success)
	switch {
	case success == nil:
		return errors.New("ctx models: the answer carries a key but no success field — not a ctx envelope (the key was printed on stdout)")
	case *success:
		return nil
	}
	var reason string
	_ = json.Unmarshal(fields["error"], &reason)
	reason = strings.ReplaceAll(reason, key, "[key]")
	if reason == "" {
		reason = "request failed"
	}
	return fmt.Errorf("ctx models: %s — success:false, yet the answer carries a key (printed on stdout)", reason)
}

func modelsEmitMinted(m modelsMint, obj any, key string, asJSON bool) error {
	if modelsWantJSON(asJSON) {
		return modelsPrintJSON(map[string]any{m.field: obj, "api_key": key})
	}
	m.render(obj)
	modelsPrintKeyTTY(key)
	return nil
}

func modelsEmitKeyOnly(key string, asJSON bool) error {
	if modelsWantJSON(asJSON) {
		return modelsPrintJSON(map[string]string{"api_key": key})
	}
	modelsPrintKeyTTY(key)
	return nil
}

// modelsPrintKeyTTY prints the one line with the plaintext and the warning
// (a line of its own, without the key).
func modelsPrintKeyTTY(key string) {
	fmt.Printf("\n  api_key: %s\n\n  %s\n", sanitizeTerminal(key), modelsKeyWarning)
}

// ── terminal rendering ───────────────────────────────────────────────────────.
// Labels, descriptions and alias names are tenant-admin input; every cell
// goes through sanitizeTerminal before it reaches a terminal.

func renderModelsConsumers(page modelsConsumerPage) error {
	if len(page.Consumers) == 0 {
		fmt.Println("No model consumers.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tLABEL\tKIND\tSTATUS\tLANE\tSENSITIVITY\tKEYS\tLAST USED")
	for _, c := range page.Consumers {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			sanitizeTerminal(c.ID), sanitizeTerminal(c.Label), sanitizeTerminal(c.Kind),
			sanitizeTerminal(c.Status), sanitizeTerminal(c.AdmissionLane),
			sanitizeTerminal(c.PromptSensitivity), c.KeysTotal, modelsTimeCell(c.LastUsedAt, "never"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if page.NextCursor != nil && *page.NextCursor != "" {
		fmt.Printf("\nmore: --cursor %s\n", sanitizeTerminal(*page.NextCursor))
	}
	return nil
}

func renderModelsAliases(page modelsAliasPage) error {
	if len(page.ModelAliases) == 0 {
		fmt.Println("No model aliases.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tKIND\tROLE\tSCOPE\tSENSITIVITY\tENABLED\tID")
	for _, a := range page.ModelAliases {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%t\t%s\n",
			sanitizeTerminal(a.Name), sanitizeTerminal(a.Kind), sanitizeTerminal(a.Role),
			sanitizeTerminal(a.Scope), sanitizeTerminal(a.Sensitivity), a.Enabled, sanitizeTerminal(a.ID))
	}
	return w.Flush()
}

func renderModelsConsumerKV(obj any) {
	c, _ := obj.(modelsConsumerView)
	fmt.Printf("Model consumer created: %s\n\n", sanitizeTerminal(c.Label))
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	kv := func(k, v string) { _, _ = fmt.Fprintf(w, "  %s\t%s\n", k, sanitizeTerminal(v)) }
	kv("id", c.ID)
	kv("tenant", c.TenantSlug+" ("+c.TenantID+")")
	kv("tenant scope", modelsStrCell(c.TenantScope, "-"))
	kv("kind", c.Kind)
	kv("status", c.Status)
	kv("models", modelsGrantCell(c))
	kv("sensitivity", c.PromptSensitivity)
	kv("lane", c.AdmissionLane)
	kv("fair weight", strconv.Itoa(c.FairWeight))
	kv("cidrs", modelsListCell(c.AllowedCIDRs, "any"))
	kv("plain http", strconv.FormatBool(c.AllowPlaintext))
	for _, k := range c.Keys {
		kv("key", k.Hint+"  expires "+modelsTimeCell(k.ExpiresAt, "never"))
	}
	_ = w.Flush()
}

func renderModelsKeyKV(obj any) {
	k, _ := obj.(modelsKeyView)
	fmt.Println("Key rotated.")
	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	kv := func(name, v string) { _, _ = fmt.Fprintf(w, "  %s\t%s\n", name, sanitizeTerminal(v)) }
	kv("id", k.ID)
	kv("hint", k.Hint)
	kv("status", k.Status)
	kv("rotated from", modelsStrCell(k.RotatedFrom, "-"))
	kv("expires", modelsTimeCell(k.ExpiresAt, "never"))
	_ = w.Flush()
}

func modelsGrantCell(c modelsConsumerView) string {
	if c.AllModels {
		return "all"
	}
	return modelsListCell(c.Models, "none")
}

func modelsListCell(v []string, empty string) string {
	if len(v) == 0 {
		return empty
	}
	return strings.Join(v, ", ")
}

func modelsStrCell(v *string, empty string) string {
	if v == nil || *v == "" {
		return empty
	}
	return *v
}

func modelsTimeCell(v *string, empty string) string {
	if v == nil || *v == "" {
		return empty
	}
	return dateCell(*v)
}

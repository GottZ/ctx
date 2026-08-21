// ctx backends seed — the non-interactive first seed of the backend pool
// (design/02 §4.1a, wave A02-W2). It is the client-side replacement for the
// env-era bootstrap: after the Stufe-2 cut, no env tuple describes a backend
// any more, so the topology enters the pool through the SAME admin manage
// surface every other mutation uses — full validation, audit trail and
// reloadAfterMutation included, none of which the old direct-INSERT had.
//
//	ctx backends seed --file seed.json
//	ctx backends seed -                       # SeedSpec via stdin
//	ctx backends seed --host http://gpu:11434 --model qwen3 --embed-model qwen3-embed
//
// Posture, all deliberate and all fail-closed:
//
//   - server-admin only. backendCreateScope pins a tenant-admin's create to
//     its own HomeScope, so a tenant-admin seed would silently write tenant
//     topology, report success, and leave the _global pool (dream, digest,
//     every other tenant) dead. Refused with a named reason instead.
//   - full-trust with confirm_trust_elevation. The manage surface defaults new
//     rows to trust=public, and Chain filters trust before availability while
//     pool.default_block_sensitivity is credentials — a public seed would
//     produce two rows and still serve empty chains for the operator's own
//     blocks. The seed describes the operator's primary topology, so it asks
//     for full-trust explicitly (overridable per SeedSpec).
//   - secrets before rows, then a metadata existence check. The server does
//     not validate that api_key_ref names an existing secret, so a dangling
//     ref would report seed success and fail at request time with an auth
//     error. The check is the client's duty.
//   - per-row idempotency over the target set. Each create commits on its own
//     (no batch transaction), so a run that dies between the two creates must
//     be completable: existing target rows are skipped, missing ones are
//     added. Only rows OUTSIDE the target set trip the guard, and --force
//     skips exactly that guard, nothing else.

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/spf13/cobra"
)

// Target-set row names (decision E8). Deliberately neutral: the legacy
// bootstrap names are artifacts of one host, and neutral names keep the
// legacy default fingerprint (herbert-chat/llama-embed on localhost:11434,
// what an unconfigured env bootstrap left behind) distinguishable from a
// freshly seeded pool.
const (
	seedChatName  = "chat-primary"
	seedEmbedName = "embed-primary"
)

// seedPriority is the priority both seed rows get. It is INITIAL DATA, not a
// contract — the pool is the living config from the first row on.
const seedPriority = 100

// seedBackend is one leg of the SeedSpec. Field set is chosen for symmetry
// with a later `ctx backends export` (pool → SeedSpec): everything here
// round-trips through the pool row, and api_key (write-only) has api_key_ref
// as its readable counterpart.
type seedBackend struct {
	// Name overrides the target-set name. Absent = the E8 default for the
	// leg. It exists so an export can name the rows it actually found.
	Name          string `json:"name,omitempty"`
	Host          string `json:"host"`
	Protocol      string `json:"protocol,omitempty"`
	Model         string `json:"model"`
	APIKey        string `json:"api_key,omitempty"`
	APIKeyRef     string `json:"api_key_ref,omitempty"`
	NumCtx        int    `json:"num_ctx,omitempty"`
	Think         *bool  `json:"think,omitempty"`
	Trust         string `json:"trust,omitempty"`
	ProviderClass string `json:"provider_class,omitempty"`
}

// seedSpec is the whole input document (design/02 §3).
type seedSpec struct {
	Chat  seedBackend `json:"chat"`
	Embed seedBackend `json:"embed"`
}

// seedRow is one planned target row: the manage payload plus the F2 secret
// that has to exist before the row may reference it.
type seedRow struct {
	name string
	// roles is the leg's routing table, in designed order.
	roles []string
	// secretName is the api_key_ref the row will carry ("" = no credential).
	secretName string
	// secretValue is the plaintext to seal ("" = the ref is expected to exist
	// already, the api_key_ref-only case).
	secretValue string
	payload     map[string]any
}

// leadRole is the role the leg exists for: the first of its list, and the one
// whose empty chain stops the store (no embed backend fails every query at the
// embedding step, no synthesis backend leaves it without an answer). The
// trailing roles (translate, chat, digest, dream on the chat leg) are pool
// management — a row found without them is a topology choice, not a failed
// seed, and rewriting it is exactly what a seed must never do.
func (r seedRow) leadRole() string {
	if len(r.roles) == 0 {
		return ""
	}
	return r.roles[0]
}

// seedResult is the machine-readable summary (pipe path). The human path
// prints the same facts as lines.
type seedResult struct {
	Success bool     `json:"success"`
	Scope   string   `json:"scope"`
	Created []string `json:"created"`
	Skipped []string `json:"skipped"`
	Secrets []string `json:"secrets"`
	// Unserved names target rows that exist but do not serve their leg's role.
	// They are neither created (the name is taken) nor skipped as fine — they
	// are the reason success is false.
	Unserved []string `json:"unserved,omitempty"`
}

// seedBlocker is a present target row that cannot serve the leg it occupies.
// The name is taken, so the seed must neither write a second row nor rewrite
// the one it found — but reporting the run as a clean no-op would be a lie: the
// role stays unserved. Named, with the exact repair, and the run exits non-zero.
type seedBlocker struct {
	name   string
	role   string
	reason string
	repair string
}

func (b seedBlocker) String() string {
	return fmt.Sprintf("%s: present, but does not serve %s — %s. Repair: %s",
		b.name, b.role, b.reason, b.repair)
}

func backendsSeedCmd(getClient func() (*Client, error)) *cobra.Command {
	var (
		file       string
		host       string
		model      string
		embedModel string
		force      bool
	)
	cmd := &cobra.Command{
		Use:   "seed [-]",
		Short: "Seed the empty pool with a chat + embed backend (server-admin key required)",
		Long: "Non-interactive first seed of context_backends from a SeedSpec document.\n" +
			"Writes exactly two _global rows (" + seedChatName + ", " + seedEmbedName + ") with\n" +
			"full-trust posture, sealing any api_key into an F2 secret first. Idempotent per\n" +
			"row: a re-run after a partial seed completes it instead of refusing. Rows outside\n" +
			"the target set abort the run unless --force is given.",
		Example: `  ctx backends seed --file seed.json
  cat seed.json | ctx backends seed -
  ctx backends seed --host http://gpu:11434 --model qwen3 --embed-model qwen3-embedding`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := loadSeedSpec(args, file, host, model, embedModel)
			if err != nil {
				return err
			}
			return runBackendsSeed(getClient, spec, force)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "SeedSpec JSON file (\"-\" or omitted = stdin)")
	cmd.Flags().StringVar(&host, "host", "", "shorthand: base URL for BOTH backends (single-host case)")
	cmd.Flags().StringVar(&model, "model", "", "shorthand: chat model")
	cmd.Flags().StringVar(&embedModel, "embed-model", "", "shorthand: embedding model")
	cmd.Flags().BoolVar(&force, "force", false, "seed even though the pool holds rows outside the target set")
	return cmd
}

// loadSeedSpec resolves the input document from --file, stdin or the
// single-host shorthand flags. The two input styles are mutually exclusive:
// silently letting a flag override a file field would make the effective spec
// unreadable from the command line alone.
func loadSeedSpec(args []string, file, host, model, embedModel string) (seedSpec, error) {
	shorthand := host != "" || model != "" || embedModel != ""
	fromArg := len(args) > 0 && args[0] == "-"
	if file != "" && file != "-" {
		if shorthand {
			return seedSpec{}, errors.New("--file and the --host/--model/--embed-model shorthand are mutually exclusive")
		}
		warnSeedFilePermissions(file)
		raw, err := os.ReadFile(file) //nolint:gosec // operator-supplied path, that is the point
		if err != nil {
			return seedSpec{}, fmt.Errorf("read seed file: %w", err)
		}
		return parseSeedSpec(raw)
	}
	if raw, ok := ReadStdin(); ok && strings.TrimSpace(raw) != "" {
		if shorthand {
			return seedSpec{}, errors.New("stdin spec and the --host/--model/--embed-model shorthand are mutually exclusive")
		}
		return parseSeedSpec([]byte(raw))
	}
	if fromArg || file == "-" {
		return seedSpec{}, errors.New("no SeedSpec on stdin")
	}
	if !shorthand {
		return seedSpec{}, errors.New("no SeedSpec given — use --file <path>, pipe it on stdin, or the --host/--model/--embed-model shorthand")
	}
	if host == "" || model == "" || embedModel == "" {
		return seedSpec{}, errors.New("the shorthand needs all three of --host, --model and --embed-model")
	}
	return seedSpec{
		Chat:  seedBackend{Host: host, Model: model},
		Embed: seedBackend{Host: host, Model: embedModel},
	}, nil
}

// warnSeedFilePermissions flags a seed file that is readable beyond its owner.
// A SeedSpec carries the provider api_key in the clear; the file is a transient
// input, not a config artifact (docs/operations.md: chmod 0600, delete after
// the seed, never commit).
func warnSeedFilePermissions(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if fi.Mode().Perm()&0o077 != 0 {
		Errorf("warning: %s is readable beyond its owner (mode %04o) and may carry an api_key in the clear — chmod 0600 it and delete it after the seed",
			path, fi.Mode().Perm())
	}
}

func parseSeedSpec(raw []byte) (seedSpec, error) {
	var spec seedSpec
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return seedSpec{}, fmt.Errorf("seed spec: %w", err)
	}
	return spec, nil
}

// buildSeedRows turns a SeedSpec into the two planned rows. Pure — the whole
// payload shape (roles, priorities, trust posture, model_map) is unit-testable
// without a server.
func buildSeedRows(spec seedSpec) ([]seedRow, error) {
	if spec.Embed.Think != nil {
		return nil, errors.New("embed: think is a chat-side toggle and has no meaning on an embedding backend")
	}
	chat, err := seedRowFor(spec.Chat, seedChatName,
		[]string{backends.RoleSynthesis, backends.RoleTranslate, backends.RoleChat, backends.RoleDigest, backends.RoleDream}, "chat")
	if err != nil {
		return nil, err
	}
	embed, err := seedRowFor(spec.Embed, seedEmbedName,
		[]string{backends.RoleEmbed, backends.RoleDreamEmbed}, "embed")
	if err != nil {
		return nil, err
	}
	return []seedRow{chat, embed}, nil
}

// seedRowFor validates one leg and materializes its backend-create payload.
func seedRowFor(in seedBackend, defaultName string, roles []string, label string) (seedRow, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = defaultName
	}
	host := strings.TrimSpace(in.Host)
	if host == "" {
		return seedRow{}, fmt.Errorf("%s: host is required", label)
	}
	model := strings.TrimSpace(in.Model)
	if model == "" {
		return seedRow{}, fmt.Errorf("%s: model is required", label)
	}
	protocol := strings.TrimSpace(in.Protocol)
	if protocol == "" {
		protocol = string(backends.ProtocolOllama)
	}
	switch backends.Protocol(protocol) {
	case backends.ProtocolOllama, backends.ProtocolOpenAI:
	default:
		return seedRow{}, fmt.Errorf("%s: protocol %q is not one of ollama, openai", label, protocol)
	}
	trust := strings.TrimSpace(in.Trust)
	if trust == "" {
		trust = string(backends.TrustFull)
	}
	switch backends.Trust(trust) {
	case backends.TrustFull, backends.TrustNoCredentials, backends.TrustNonPersonal, backends.TrustPublic:
	default:
		return seedRow{}, fmt.Errorf("%s: trust %q is not one of full-trust, no-credentials, non-personal, public", label, trust)
	}
	if in.APIKey != "" && in.APIKeyRef != "" {
		return seedRow{}, fmt.Errorf("%s: api_key and api_key_ref are mutually exclusive — the key seals INTO a ref", label)
	}
	if in.NumCtx < 0 {
		return seedRow{}, fmt.Errorf("%s: num_ctx must not be negative", label)
	}

	spec := map[string]any{"model": model}
	if in.Think != nil {
		spec["params"] = map[string]any{"think": *in.Think}
	}
	payload := map[string]any{
		"name":      name,
		"base_url":  host,
		"protocol":  protocol,
		"trust":     trust,
		"roles":     roles,
		"model_map": map[string]any{"default": spec},
		"priority":  seedPriority,
		// Explicit even though a server-admin defaults here anyway: the seed
		// states the scope it means, so a future non-_global seed is a spec
		// change and not a silent consequence of the caller's key.
		"scope": backends.GlobalScope,
	}
	if backends.Trust(trust) != backends.TrustPublic {
		payload["confirm_trust_elevation"] = true
	}
	if in.NumCtx > 0 {
		payload["num_ctx"] = in.NumCtx
	}
	if in.ProviderClass != "" {
		payload["provider_class"] = in.ProviderClass
	}

	row := seedRow{name: name, roles: roles, payload: payload}
	switch {
	case in.APIKeyRef != "":
		row.secretName = in.APIKeyRef
	case in.APIKey != "":
		row.secretName = name + "-key"
		row.secretValue = in.APIKey
	}
	if row.secretName != "" {
		payload["api_key_ref"] = row.secretName
	}
	return row, nil
}

// seedWhoami is the identity subset the seed gate needs. admin==true is the
// server-global tier (auth.AuthResult.IsAdmin); a tenant-admin carries
// admin==false with role owner|admin.
type seedWhoami struct {
	Admin     bool   `json:"admin"`
	Role      string `json:"role"`
	HomeScope string `json:"home_scope"`
}

func runBackendsSeed(getClient func() (*Client, error), spec seedSpec, force bool) error {
	rows, err := buildSeedRows(spec)
	if err != nil {
		return err
	}
	c, err := getClient()
	if err != nil {
		return err
	}

	// Pre-check 1: server-admin tier. Before ANY write.
	if err := requireServerAdminForSeed(c); err != nil {
		return err
	}
	// The target set is what this spec describes; anything else in the pool is
	// a foreign row.
	present, blocked, err := seedPoolState(c, rows, force)
	if err != nil {
		return err
	}

	var missing []seedRow
	res := seedResult{Success: true, Scope: backends.GlobalScope}
	for _, r := range rows {
		if b, bad := blocked[r.name]; bad {
			// Still skipped — see seedPoolState — but never as a success.
			Errorf("%s", b)
			res.Unserved = append(res.Unserved, r.name)
			res.Success = false
			continue
		}
		if present[r.name] {
			Errorf("%s: already present — skipped", r.name)
			res.Skipped = append(res.Skipped, r.name)
			continue
		}
		missing = append(missing, r)
	}

	// Secrets BEFORE rows, and only for the rows actually being written: an
	// existing target row keeps its credential untouched (a seed must never
	// silently rotate a live key).
	if err := seedSecrets(c, missing, &res); err != nil {
		return err
	}
	for _, r := range missing {
		created, err := seedCreateRow(c, r)
		if err != nil {
			return withOrphanSecretNote(err, res.Secrets)
		}
		if created {
			Errorf("%s: created", r.name)
			res.Created = append(res.Created, r.name)
		} else {
			Errorf("%s: already present — skipped", r.name)
			res.Skipped = append(res.Skipped, r.name)
		}
	}
	if perr := printSeedResult(res); perr != nil {
		return perr
	}
	if !res.Success {
		// The result document goes out first (success:false, unserved listed),
		// then the run fails: a pipe consumer gets the machine-readable truth AND
		// a non-zero exit, instead of one or the other.
		return fmt.Errorf("%d target row(s) exist but do not serve their role: %s — the pool is NOT seeded. "+
			"Repair them as printed above, or `ctx backends delete <id>` and re-run the seed",
			len(res.Unserved), strings.Join(res.Unserved, ", "))
	}
	return nil
}

// requireServerAdminForSeed is pre-check 1 (design/02 §4.1a): the seed targets
// the _global pool, and only a server-admin may write it — a tenant-admin's
// create is pinned to its HomeScope, which would look like success and leave
// the shared pool dead.
func requireServerAdminForSeed(c *Client) error {
	resp, _, err := c.Do(http.MethodGet, "/api/whoami", nil)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return fmt.Errorf("identity check failed: %w", err)
	}
	var who seedWhoami
	if err := json.Unmarshal(resp, &who); err != nil {
		return fmt.Errorf("unparseable whoami response: %s", truncateForError(resp))
	}
	if who.Admin {
		return nil
	}
	tier := "this key"
	if who.Role == "owner" || who.Role == "admin" {
		tier = fmt.Sprintf("a tenant-admin key (role %q, home scope %q)", who.Role, who.HomeScope)
	}
	return fmt.Errorf("seeding the %s pool requires a server-admin key — %s would be pinned to its own tenant scope, "+
		"leaving the shared pool empty while reporting success. Nothing was written",
		backends.GlobalScope, tier)
}

// seedPoolState reads the pool and applies the foreign-row guard. It returns
// which target rows already exist in _global, plus the blockers among them: a
// present row that does NOT serve its leg's lead role. A target name in a
// FOREIGN scope counts as a foreign row, not as present — seeding _global must
// not be satisfied by a tenant's row of the same name.
//
// "Present" alone was never a serving signal (the same gap the init wizard's
// probe closes on the other side): a re-run over a target row that is disabled,
// held by an active disable-profile, or stripped of its role reported a clean
// no-op and exit 0 while the role stayed dead. The row is still SKIPPED — the
// name is taken, a second row would be a duplicate and rewriting the found row
// would silently rotate live topology — but it is named, and the run fails.
func seedPoolState(c *Client, rows []seedRow, force bool) (map[string]bool, map[string]seedBlocker, error) {
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "backend-list"})
	if err != nil {
		return nil, nil, err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return nil, nil, fmt.Errorf("pool read failed: %w", err)
	}
	var payload struct {
		Backends []backendRow `json:"backends"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return nil, nil, fmt.Errorf("unparseable backend-list response: %s", truncateForError(resp))
	}
	targets := make(map[string]seedRow, len(rows))
	for _, r := range rows {
		targets[r.name] = r
	}
	present := map[string]bool{}
	blocked := map[string]seedBlocker{}
	var foreign []string
	for _, b := range payload.Backends {
		target, isTarget := targets[b.Name]
		if !isTarget || b.Scope != backends.GlobalScope {
			foreign = append(foreign, b.Name)
			continue
		}
		present[b.Name] = true
		if blocker, bad := seedRowBlocker(target, b); bad {
			blocked[b.Name] = blocker
		}
	}
	if len(foreign) > 0 && !force {
		return nil, nil, fmt.Errorf("pool already contains %d row(s) outside the seed target set (%s) — "+
			"this installation is configured, not fresh. Re-run with --force to seed anyway. Nothing was written",
			len(foreign), strings.Join(foreign, ", "))
	}
	return present, blocked, nil
}

// seedRowBlocker judges one present target row against the leg it occupies.
// Serving-eligibility is read exactly as Chain reads it (enabled AND no active
// disable-profile — cooldown is transient and never disqualifies), and the role
// check is the lead role only: extra roles are pool management, a missing lead
// role means the pipeline the leg exists for has no backend at all.
func seedRowBlocker(target seedRow, have backendRow) (seedBlocker, bool) {
	role := target.leadRole()
	b := seedBlocker{name: have.Name, role: role}
	switch {
	case !have.Enabled || have.EffectiveState == backendStateDisabled:
		b.reason = "the row is disabled"
		b.repair = fmt.Sprintf("ctx backends update %s '{\"enabled\":true}'", have.ID)
	case have.EffectiveState == backendStateProfileDisabled:
		b.reason = "an active disable-profile holds it"
		if len(have.DisabledByProfiles) > 0 {
			b.reason = fmt.Sprintf("the active disable-profile(s) %s hold it", strings.Join(have.DisabledByProfiles, ", "))
		}
		b.repair = "deactivate that profile (`ctx eject off` for the reserved `eject` profile)"
	case role != "" && !have.hasRole(role):
		b.reason = fmt.Sprintf("its roles (%s) do not carry %s", strings.Join(have.Roles, ", "), role)
		b.repair = fmt.Sprintf("ctx backends update %s '{\"roles\":[\"%s\"]}'", have.ID, strings.Join(target.roles, "\",\""))
	default:
		return seedBlocker{}, false
	}
	return b, true
}

// seedSecrets seals every pending api_key and then verifies, through the
// metadata surface, that each referenced ref actually exists in the target
// scope. The server persists api_key_ref unvalidated, so a dangling ref would
// pass create and only surface as a runtime auth failure.
func seedSecrets(c *Client, rows []seedRow, res *seedResult) error {
	for _, r := range rows {
		if r.secretValue == "" {
			continue
		}
		resp, status, err := c.Do(http.MethodPut, "/api/secrets/"+r.secretName, map[string]string{"value": r.secretValue})
		if err != nil {
			return withOrphanSecretNote(err, res.Secrets)
		}
		if status == http.StatusServiceUnavailable {
			return withOrphanSecretNote(seedSealboxUnavailable(resp), res.Secrets)
		}
		if err := checkSettingsEnvelope(resp); err != nil {
			return withOrphanSecretNote(fmt.Errorf("sealing %s: %w. Nothing was written to the pool", r.secretName, err), res.Secrets)
		}
		Errorf("%s: sealed as api_key_ref for %s", r.secretName, r.name)
		res.Secrets = append(res.Secrets, r.secretName)
	}

	want := map[string]string{}
	for _, r := range rows {
		if r.secretName != "" {
			want[r.secretName] = r.name
		}
	}
	if len(want) == 0 {
		return nil
	}
	resp, _, err := c.Do(http.MethodGet, "/api/secrets", nil)
	if err != nil {
		return err
	}
	if err := checkSettingsEnvelope(resp); err != nil {
		return withOrphanSecretNote(fmt.Errorf("secret metadata read failed: %w. Nothing was written to the pool", err), res.Secrets)
	}
	var payload struct {
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return withOrphanSecretNote(fmt.Errorf("unparseable secrets response: %s", truncateForError(resp)), res.Secrets)
	}
	have := map[string]bool{}
	for _, s := range payload.Secrets {
		have[s.Name] = true
	}
	for ref, row := range want {
		if !have[ref] {
			return withOrphanSecretNote(fmt.Errorf("api_key_ref %q for %s does not exist in the %s scope — a backend row referencing it "+
				"would fail at request time. Create it with `ctx secrets set %s` first. Nothing was written to the pool",
				ref, row, backends.GlobalScope, ref), res.Secrets)
		}
	}
	return nil
}

// withOrphanSecretNote names the secrets an aborted run left behind. They are
// benign — no row references them, and a re-run reuses them (the PUT is an
// upsert) — but an unnamed leftover credential is exactly the kind of residue
// an operator should hear about, not discover later in a list.
func withOrphanSecretNote(err error, sealed []string) error {
	if err == nil || len(sealed) == 0 {
		return err
	}
	return fmt.Errorf("%w.\nSealed but now unreferenced: %s — a re-run reuses them, `ctx secrets rm <name>` removes them",
		err, strings.Join(sealed, ", "))
}

// seedSealboxUnavailable is abort class 2 (design/02 §3): the server has no
// usable CTX_SECRETS_KEY, so an api_key cannot be sealed. This is the normal
// first contact for every non-Ollama install (the compose default is empty,
// .env.example ships a non-hex placeholder), so it gets a designed message
// with the fix instead of a generic sealbox error. Never a silent plaintext
// downgrade.
func seedSealboxUnavailable(resp []byte) error {
	detail := strings.TrimSpace(string(resp))
	var env settingsEnvelope
	if err := json.Unmarshal(resp, &env); err == nil && env.Error != "" {
		detail = env.Error
	}
	return fmt.Errorf("the server cannot seal secrets, so the api_key has nowhere to go (%s).\n"+
		"Set CTX_SECRETS_KEY on the server and restart it:\n\n"+
		"  openssl rand -hex 32\n\n"+
		"Then re-run the seed. See docs/operations.md#backends. Nothing was written to the pool", detail)
}

// seedCreateRow posts one backend-create. A name collision (concurrent seed,
// or a row that appeared between the list and the create) is the idempotent
// case, not a failure: the store maps the UNIQUE (scope, name) violation to
// "backend name … already exists".
func seedCreateRow(c *Client, r seedRow) (bool, error) {
	data, err := json.Marshal(r.payload)
	if err != nil {
		return false, fmt.Errorf("%s: marshal payload: %w", r.name, err)
	}
	resp, status, err := c.Do(http.MethodPost, "/api/manage", map[string]any{
		"action": "backend-create",
		"data":   json.RawMessage(data),
	})
	if err != nil {
		return false, err
	}
	if cerr := checkSettingsEnvelope(resp); cerr != nil {
		if status == http.StatusConflict || strings.Contains(cerr.Error(), "already exists") {
			return false, nil
		}
		return false, fmt.Errorf("creating %s: %w", r.name, cerr)
	}
	return true, nil
}

func printSeedResult(res seedResult) error {
	if !StdoutIsTTY() {
		out, err := json.Marshal(res)
		if err != nil {
			return err
		}
		PrintJSON(out)
		return nil
	}
	fmt.Printf("seeded %s: %d created, %d already present\n",
		backends.GlobalScope, len(res.Created), len(res.Skipped))
	if len(res.Created) > 0 {
		fmt.Printf("  created: %s\n", strings.Join(res.Created, ", "))
	}
	if len(res.Skipped) > 0 {
		fmt.Printf("  skipped: %s\n", strings.Join(res.Skipped, ", "))
	}
	if len(res.Secrets) > 0 {
		fmt.Printf("  sealed:  %s\n", strings.Join(res.Secrets, ", "))
	}
	if len(res.Unserved) > 0 {
		fmt.Printf("  UNSERVED: %s — present rows that do not serve their role (see above)\n",
			strings.Join(res.Unserved, ", "))
	}
	fmt.Println("verify with `ctx backends` and probe reachability with `ctx backends test <id>`")
	return nil
}

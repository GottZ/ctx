package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/clientconfig"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Version is set by main.go from ldflags. Used by the init command.
var Version = "dev"

// noColor returns true when ANSI codes should be suppressed.
func noColor() bool {
	return os.Getenv("NO_COLOR") != ""
}

// ── formatting helpers ──────────────────────────────────────────────.

func okMark() string {
	if noColor() {
		return "[ok]"
	}
	return "\x1b[32m\u2713\x1b[0m"
}

func failMark() string {
	if noColor() {
		return "[FAIL]"
	}
	return "\x1b[31m\u2717\x1b[0m"
}

func warnMark() string {
	if noColor() {
		return "[WARN]"
	}
	return "\x1b[33m!\x1b[0m"
}

func skipMark() string {
	if noColor() {
		return "[skip]"
	}
	return "\x1b[2m-\x1b[0m"
}

// label formats "  Key ............. value" with dot-padding.
func label(key string, valueFormat string, args ...any) string {
	const totalWidth = 23 // "  Key ............. " = 23 columns
	prefix := "  " + key + " "
	dots := totalWidth - len(prefix)
	if dots < 3 {
		dots = 3
	}
	return prefix + strings.Repeat(".", dots) + " " + fmt.Sprintf(valueFormat, args...)
}

// prompt asks a question and returns the trimmed answer. Empty string = default.
func prompt(question string) string {
	fmt.Print(question)
	var answer string
	_, _ = fmt.Scanln(&answer)
	return strings.TrimSpace(answer)
}

// promptYesNo asks a [Y/n] question. Default is yes.
func promptYesNo(question string) bool {
	answer := prompt(question + " [Y/n]: ")
	return answer == "" || strings.HasPrefix(strings.ToLower(answer), "y")
}

// ── Claude settings.json path ──────────────────────────────────────.

func claudeSettingsPath() string {
	// CLAUDE_CONFIG_DIR relocates Claude Code's entire config directory; when it
	// is set, the live settings file is $CLAUDE_CONFIG_DIR/settings.json and
	// ~/.claude/settings.json is not read. Honor it before any home-relative path.
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json")
	}
	if runtime.GOOS == "windows" {
		if profile := os.Getenv("USERPROFILE"); profile != "" {
			return filepath.Join(profile, ".claude", "settings.json")
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".claude", "settings.json")
}

// displayConfigPath returns a user-friendly config path (with ~ on Unix).
func displayConfigPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// ── Step 1: Config check ───────────────────────────────────────────.

type initResult struct {
	ConfigOK     bool
	ServerOK     bool
	KeyOK        bool
	BackendsOK   bool
	VersionOK    bool
	HooksOK      bool
	StatuslineOK bool
}

// initSummaryOK is the wizard's closing verdict. Backends and Version are
// tracked but deliberately NOT summed into it: both steps can end short of ✓
// for reasons that are none of init's business — a key below server-admin, an
// offline GitHub — and neither invalidates what init actually configures.
// The key verdict IS summed: a key the server does not accept makes every
// later call fail with a bare "unauthorized", so "ready" on top of it would be
// exactly the false green of #40 Bug 8b.
// init exits 0 regardless; the per-step line carries the detail.
func initSummaryOK(r initResult) bool {
	return r.ConfigOK && r.ServerOK && r.KeyOK && r.HooksOK && r.StatuslineOK
}

func stepConfig() (Config, bool) {
	return runConfigStep(prompt)
}

// runConfigStep is stepConfig with the prompt injected, so tests can script it.
func runConfigStep(ask func(question string) string) (Config, bool) {
	cfgPath := clientconfig.FilePath()
	displayPath := displayConfigPath(cfgPath)

	cfg, err := clientconfig.Load()
	if err == nil {
		fmt.Println(label("Config", "%s exists %s", displayPath, okMark()))
		return cfg, true
	}

	fmt.Println(label("Config", "not found"))

	baseURL := ask("  ? Base URL: ")
	if baseURL == "" {
		fmt.Println(label("Config", "skipped (no URL) %s", failMark()))
		return Config{}, false
	}
	apiKey, verified, ok := askAcceptedKey(ask, baseURL)
	if !ok {
		return Config{}, false
	}

	// Ensure directory exists
	dir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		fmt.Println(label("Config", "cannot create %s: %v %s", dir, err, failMark()))
		return Config{}, false
	}

	content := fmt.Sprintf("CTX_BASE_URL=%s\nCTX_KEY=%s\n", baseURL, apiKey)
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		fmt.Println(label("Config", "write failed: %v %s", err, failMark()))
		return Config{}, false
	}

	if !verified {
		fmt.Println(label("Config", "saved to %s, key not verified %s", displayPath, warnMark()))
		return Config{BaseURL: baseURL, Key: apiKey}, true
	}
	fmt.Println(label("Config", "saved to %s %s", displayPath, okMark()))
	return Config{BaseURL: baseURL, Key: apiKey}, true
}

// ── Step 2: Server health ──────────────────────────────────────────.

func stepServer(cfg Config) bool {
	baseURL := strings.TrimSuffix(cfg.BaseURL, "/")

	client := newHTTPClient(5 * time.Second)
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		fmt.Println(label("Server", "%s (unreachable) %s", baseURL, failMark()))
		fmt.Println("         Check: URL correct? Server running?")
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var health struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &health); err != nil || health.Status == "" {
		fmt.Println(label("Server", "%s (invalid response) %s", baseURL, failMark()))
		return false
	}

	if health.Status == "ok" {
		fmt.Println(label("Server", "%s (healthy) %s", baseURL, okMark()))
		return true
	}

	fmt.Println(label("Server", "%s (%s) %s", baseURL, health.Status, warnMark()))
	return true // degraded is still usable
}

// ── Step 3: API key ────────────────────────────────────────────────.
//
// #40 Bug 8b: any non-empty answer used to become CTX_KEY, and the only
// network check was the unauthenticated GET /health — a typo got "healthy ✓"
// and a config whose first authenticated call returned a bare "unauthorized".
// The check lives here, where the typo happens, and NOT in the server's
// SanitizeKey: rejecting malformed keys there would turn the 401 into a
// key-shape oracle on the unauthenticated path and give up a strip that is
// tested as a security property.

// apiKeyLen is the length of every raw API key: 32 random bytes, hex-encoded
// (store.CreateApiKey).
const apiKeyLen = 64

// maxKeyAttempts bounds the re-ask loop. An empty answer ends it at once, so a
// closed stdin cannot spin it either.
const maxKeyAttempts = 3

// maxListedPositions caps the position list of a form problem — a pasted wrong
// value has dozens, and the count already carries the size.
const maxListedPositions = 8

// The server's credential grammar (handler.resolveCredential): these prefixes
// mark OAuth tokens, everything else is a raw key. Mirrored rather than
// imported — the CLI does not link the store; TestOAuthPrefixesMirrorServer
// pins the two values.
const (
	oauthAccessTokenPrefix  = "ctxt_"
	oauthRefreshTokenPrefix = "ctxr_"
)

// keyFormProblem names how a value deviates from the raw-key shape — counted
// and with 1-based character positions, so the typo can be found — or returns
// "" for a well-formed key. Uppercase hex is a deviation of its own: the
// server keeps it (SanitizeKey strips only non-hex), but ctx_auth hashes the
// key as given and keys are minted lowercase, so it never authenticates.
func keyFormProblem(key string) string {
	if strings.HasPrefix(key, oauthAccessTokenPrefix) || strings.HasPrefix(key, oauthRefreshTokenPrefix) {
		return "this is an OAuth token, not an API key — tokens expire, the config needs the API key itself"
	}
	var nonHex, upper []int
	n := 0
	for _, r := range key {
		n++
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
			upper = append(upper, n)
		default:
			nonHex = append(nonHex, n)
		}
	}
	var problems []string
	if n != apiKeyLen {
		problems = append(problems, fmt.Sprintf("%d characters, expected %d", n, apiKeyLen))
	}
	if len(nonHex) > 0 {
		problems = append(problems, countedPositions(nonHex, "non-hex character"))
	}
	if len(upper) > 0 {
		problems = append(problems, countedPositions(upper, "uppercase character")+" (keys are lowercase)")
	}
	return strings.Join(problems, "; ")
}

// countedPositions renders "2 non-hex characters at positions 5, 17".
func countedPositions(pos []int, noun string) string {
	if len(pos) == 1 {
		return fmt.Sprintf("1 %s at position %d", noun, pos[0])
	}
	listed, more := pos, ""
	if len(listed) > maxListedPositions {
		listed, more = listed[:maxListedPositions], ", …"
	}
	parts := make([]string, len(listed))
	for i, p := range listed {
		parts[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("%d %ss at positions %s%s", len(pos), noun, strings.Join(parts, ", "), more)
}

type keyVerdict int

const (
	keyAccepted   keyVerdict = iota // 200 + identity
	keyRejected                     // 401: the server knows no such key
	keyUnverified                   // unreachable, or an answer that is not ctxd's
)

type keyProbe struct {
	verdict keyVerdict
	label   string // the key's label, on accept
	detail  string // why the key stayed unverified
}

// probeKey asks the server who the key is. GET /api/whoami is the cheapest
// authenticated read there is — one key lookup, no side effect — and the Auth
// middleware answers an unknown key with 401 before the handler runs. The
// request goes to the user's OWN ctxd, so the keyed Client is the right
// transport here (unlike stepVersion's foreign host).
func probeKey(baseURL, key string) keyProbe {
	c := NewClientWithTimeout(Config{BaseURL: baseURL, Key: key}, 5*time.Second, 64<<10)
	body, status, err := c.Do(http.MethodGet, "/api/whoami", nil)
	if err != nil {
		return keyProbe{verdict: keyUnverified, detail: "server unreachable"}
	}
	if status == http.StatusUnauthorized {
		return keyProbe{verdict: keyRejected}
	}
	var who struct {
		Success bool   `json:"success"`
		Label   string `json:"label"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &who) != nil || !who.Success {
		return keyProbe{verdict: keyUnverified, detail: fmt.Sprintf("unexpected answer from /api/whoami: HTTP %d", status)}
	}
	return keyProbe{verdict: keyAccepted, label: who.Label}
}

// askAcceptedKey reads the key until it is well-formed AND the server accepts
// it, so that "saved" means the key works. A malformed answer never leaves the
// machine; a rejected one is re-asked like a malformed one. When the server
// cannot answer at all, the offline path the wizard always had stays open: the
// key comes back unverified and the Server step names the unreachable host.
func askAcceptedKey(ask func(question string) string, baseURL string) (key string, verified, ok bool) {
	for attempt := 1; attempt <= maxKeyAttempts; attempt++ {
		key = ask("  ? API Key: ")
		if key == "" {
			fmt.Println(label("Config", "skipped (no key) %s", failMark()))
			return "", false, false
		}
		if problem := keyFormProblem(key); problem != "" {
			fmt.Println(indentHint("not an API key: " + problem + retryNote(attempt)))
			continue
		}
		p := probeKey(baseURL, key)
		switch p.verdict {
		case keyAccepted:
			return key, true, true
		case keyRejected:
			fmt.Println(indentHint(strings.TrimSuffix(baseURL, "/") +
				" rejects this key (401 unauthorized): a typo, a revoked key, or a key of another server" +
				retryNote(attempt)))
		default:
			fmt.Println(indentHint("key not verified: " + p.detail))
			return key, false, true
		}
	}
	fmt.Println(label("Config", "not saved (no accepted key after %d attempts) %s", maxKeyAttempts, failMark()))
	return "", false, false
}

func retryNote(attempt int) string {
	if attempt >= maxKeyAttempts {
		return ""
	}
	return " — try again"
}

// stepKey verifies the configured key on every run, so a re-run over a
// hand-edited, rotated or revoked key does not report it as ready. A fresh
// config was probed before it was saved; this second, cheap probe keeps the
// line the same on every run.
func stepKey(cfg Config) bool {
	p := probeKey(cfg.BaseURL, cfg.Key)
	switch p.verdict {
	case keyAccepted:
		if p.label != "" {
			fmt.Println(label("Key", "accepted (%q) %s", p.label, okMark()))
		} else {
			fmt.Println(label("Key", "accepted %s", okMark()))
		}
		return true
	case keyRejected:
		fmt.Println(label("Key", "rejected (401 unauthorized) %s", failMark()))
		if problem := keyFormProblem(cfg.Key); problem != "" {
			fmt.Println(indentHint("not an API key: " + problem))
		}
		fmt.Println(indentHint("fix " + configuredKeySource() + " — every authenticated call fails until then"))
	default:
		fmt.Println(label("Key", "not verified (%s) %s", p.detail, warnMark()))
	}
	return false
}

// configuredKeySource names where the key came from: an exported CTX_KEY wins
// over the file (clientconfig.Load).
func configuredKeySource() string {
	if os.Getenv("CTX_KEY") != "" {
		return "the CTX_KEY environment variable (it takes precedence over the config file)"
	}
	return "CTX_KEY in " + displayConfigPath(clientconfig.FilePath())
}

// ── Step 4: Backend pool ───────────────────────────────────────────.

// The backends step is the interactive twin of `ctx backends seed` (design/02
// §4.1b). It never re-implements the seed: where the serving roles are
// missing, it collects the topology through prompts and hands it to
// runBackendsSeed — so the server-admin gate, the full-trust posture, the
// secrets-before-rows order and the per-row idempotency are literally the same
// code on both paths.
//
// Two properties are deliberate. The step DEGRADES instead of aborting: a key
// below server-admin (a tenant-admin included — its writes would be pinned to
// its own scope and leave the shared pool dead) leaves init fully usable for
// hooks and statusline, which is what a non-admin runs it for. And it reports
// REACHABILITY, not existence: a pool of dead rows is exactly what an
// unconfigured env bootstrap leaves behind, and a bare "pool not empty" check
// would wave that through green.

// legacyDefaultHost is the base_url the env-era bootstrap wrote when nothing
// was configured. Inside the ctx container it points at the container itself,
// so these rows are dead by construction on the canonical compose install.
const legacyDefaultHost = "http://localhost:11434"

// legacyDefaultNames are the two row names that same bootstrap used. Together
// with the host they form the fingerprint of a pool nobody ever configured —
// worth naming, because its repair is a replacement (--force), not an edit.
var legacyDefaultNames = map[string]bool{"herbert-chat": true, "llama-embed": true}

// seedRoleProbes are the roles the step verifies — the two the seed writes,
// and the two whose empty chain stops the store: no embed backend fails every
// query at the embedding step, no synthesis backend leaves it without an
// answer.
var seedRoleProbes = []struct{ role, label string }{
	{backends.RoleSynthesis, "chat"},
	{backends.RoleEmbed, "embed"},
}

// backendsAsk is the step's input surface. It is a pair of functions rather
// than one io.Reader because the api-key answer must not travel like the
// others: it is read WITHOUT echo (F2-W8 — a secret in the terminal scrollback
// is a leaked secret), and both halves are injected in tests.
type backendsAsk struct {
	line   func(question string) string
	secret func(question string) (string, error)
}

func terminalAsk() backendsAsk {
	return backendsAsk{line: prompt, secret: promptSecret}
}

// promptSecret reads one value without echoing it. A failure is not silently
// downgraded to an echoing read: it names the non-interactive path instead.
func promptSecret(question string) (string, error) {
	fmt.Print(question)
	value, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("cannot read the key without echoing it (%w) — this needs a terminal; "+
			"seed non-interactively with `ctx backends seed --file seed.json` instead", err)
	}
	return strings.TrimSpace(string(value)), nil
}

func stepBackends(cfg Config) bool {
	return runBackendsStep(NewClient(cfg), terminalAsk())
}

func runBackendsStep(c *Client, ask backendsAsk) bool {
	// Pre-check first, list second: a key below server-admin must never reach a
	// write, and the reason it cannot is the same one the seed states.
	if err := requireServerAdminForSeed(c); err != nil {
		return backendsStepDegraded(err)
	}
	rows, err := backendsStepPool(c)
	if err != nil {
		return backendsStepDegraded(err)
	}
	if missing := missingSeedRoles(rows); len(missing) > 0 {
		spec, err := askSeedSpec(ask, missing)
		if err != nil {
			fmt.Println(label("Backends", "not seeded %s", failMark()))
			fmt.Println(indentHint(err.Error()))
			return false
		}
		if err := runBackendsSeed(func() (*Client, error) { return c, nil }, spec, false); err != nil {
			fmt.Println(label("Backends", "seed failed %s", failMark()))
			fmt.Println(indentHint(err.Error()))
			return false
		}
		// Re-read: backend-list renders the pool SNAPSHOT, which the create
		// refreshed through reloadAfterMutation — so the probe below sees the
		// rows that were just written.
		if rows, err = backendsStepPool(c); err != nil {
			return backendsStepDegraded(err)
		}
	}
	return probeSeedRoles(c, rows)
}

// backendsStepDegraded is the ✗-without-abort exit: the pool could not be
// verified — a key below server-admin, or a pool read that failed — init keeps
// going, and the line says how to seed later. The reason comes from the seed's
// own gate, so both paths give the operator the same sentence.
func backendsStepDegraded(err error) bool {
	fmt.Println(label("Backends", "not verified %s", failMark()))
	fmt.Println(indentHint(firstLine(err)))
	fmt.Println(indentHint("seed the pool later via `ctx backends seed` (server-admin key) — docs/operations.md#backends"))
	return false
}

func indentHint(s string) string {
	return "         " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n         ")
}

func firstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// backendsStepPool reads the pool through the same manage action the CLI list
// uses.
func backendsStepPool(c *Client) ([]backendRow, error) {
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "backend-list"})
	if err != nil {
		return nil, err
	}
	if err := checkEnvelope(resp, envelopeRequired); err != nil {
		return nil, fmt.Errorf("pool read failed: %w", err)
	}
	var payload struct {
		Backends []backendRow `json:"backends"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return nil, fmt.Errorf("unparseable backend-list response: %s", truncateForError(resp))
	}
	return payload.Backends, nil
}

// poolRowForRole picks the row that would actually serve a role: in the
// _global scope the seed targets, serving-eligible (enabled AND not held by an
// ACTIVE disable-profile — the qualification Chain/PrimaryModel apply, read off
// the effective_state the list merges in), and the HIGHEST PRIORITY among
// those. A tenant-scoped row of the same role does NOT count — it cannot serve
// the shared pipelines.
//
// Neither half is cosmetic. Without the effective_state check the single
// enabled _global embed row could sit in an active disable profile: the wizard
// reports green while every query dies at the embedding step. And the list does
// NOT arrive priority DESC — handleBackendList renders the pool snapshot in
// load order (loadBackendsSQL: ORDER BY scope, name) — so a "first match" would
// probe the alphabetically first row instead of the chain head: false red on a
// dead low-priority row, false green on a dead primary. The tie-break mirrors
// Chain's: priority DESC, then name ASC.
func poolRowForRole(rows []backendRow, role string) (backendRow, bool) {
	best := backendRow{}
	found := false
	for _, r := range rows {
		if r.Scope != backends.GlobalScope || !r.servingEligible() || !r.hasRole(role) {
			continue
		}
		if !found || r.Priority > best.Priority || (r.Priority == best.Priority && r.Name < best.Name) {
			best, found = r, true
		}
	}
	return best, found
}

func missingSeedRoles(rows []backendRow) []string {
	var missing []string
	for _, want := range seedRoleProbes {
		if _, ok := poolRowForRole(rows, want.role); !ok {
			missing = append(missing, want.role)
		}
	}
	return missing
}

// askSeedSpec collects the single-host topology the wizard can express. Both
// legs get the same host, protocol and key — anything beyond that (split
// hosts, separate credentials, dream-only rows) is normal pool management
// through `ctx backends create`, not a first seed.
func askSeedSpec(ask backendsAsk, missing []string) (seedSpec, error) {
	fmt.Println(indentHint("no serving backend for: " + strings.Join(missing, ", ")))
	host := ask.line("  ? Backend host (e.g. http://localhost:11434): ")
	if host == "" {
		return seedSpec{}, errors.New("no host given — nothing was written")
	}
	protocol := ask.line("  ? Protocol, ollama, openai or anthropic [ollama]: ")
	if protocol == "" {
		protocol = string(backends.ProtocolOllama)
	}
	chatModel := ask.line("  ? Chat model: ")
	embedModel := ask.line("  ? Embedding model: ")
	chat := seedBackend{Host: host, Protocol: protocol, Model: chatModel}
	embed := seedBackend{Host: host, Protocol: protocol, Model: embedModel}
	if backends.Protocol(protocol) == backends.ProtocolAnthropic {
		// The Messages API is chat-only and keyed: the embed leg needs its
		// own host and wire, the chat leg a declared window and the key.
		numCtx, err := askNumCtx(ask, "  ? Context window of the chat model in tokens [100000]: ", 100000)
		if err != nil {
			return seedSpec{}, err
		}
		chat.NumCtx = numCtx
		embed.Host = ask.line("  ? Embedding host (e.g. http://localhost:11434): ")
		if embed.Host == "" {
			return seedSpec{}, errors.New("no embedding host given — the anthropic wire serves no embeddings, nothing was written")
		}
		embed.Protocol = ask.line("  ? Embedding protocol, ollama or openai [ollama]: ")
		if embed.Protocol == "" {
			embed.Protocol = string(backends.ProtocolOllama)
		}
		if answerIsYes(ask.line("  ? Does the embedding host need an API key? [y/N]: ")) {
			v, err := ask.secret("  ? Embedding API key (not echoed): ")
			if err != nil {
				return seedSpec{}, err
			}
			embed.APIKey = v
		}
		v, err := ask.secret("  ? Anthropic API key (not echoed): ")
		if err != nil {
			return seedSpec{}, err
		}
		chat.APIKey = v
		return seedSpec{Chat: chat, Embed: embed}, nil
	}
	if answerIsYes(ask.line("  ? Does the host need an API key? [y/N]: ")) {
		v, err := ask.secret("  ? API key (not echoed): ")
		if err != nil {
			return seedSpec{}, err
		}
		chat.APIKey, embed.APIKey = v, v
	}
	return seedSpec{Chat: chat, Embed: embed}, nil
}

// askNumCtx reads a positive integer answer, the default for the empty
// line. Anything else is an error, not a silent fallback: a typo'd window
// ("100k") would otherwise seed a 100000-token prompt budget on a model
// whose window the operator meant to declare as ten times that.
func askNumCtx(ask backendsAsk, question string, def int) (int, error) {
	answer := strings.TrimSpace(ask.line(question))
	if answer == "" {
		return def, nil
	}
	n, err := strconv.Atoi(answer)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("context window %q is not a positive integer (tokens, e.g. 100000) — nothing was written", answer)
	}
	return n, nil
}

func answerIsYes(answer string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "y")
}

// probeSeedRoles is the functional check the step ends on: one backend-test
// per role against the row that would serve it. Everything short of a
// reachable row is a named hint, never an abort — a dead backend is an
// operational fact, and init's job is to make it visible.
func probeSeedRoles(c *Client, rows []backendRow) bool {
	ok := true
	parts := make([]string, 0, len(seedRoleProbes))
	var hints []string
	for _, want := range seedRoleProbes {
		row, found := poolRowForRole(rows, want.role)
		if !found {
			parts = append(parts, want.label+": none")
			hints = append(hints, fmt.Sprintf("no serving %s backend in %s — `ctx backends seed` writes one",
				want.role, backends.GlobalScope))
			ok = false
			continue
		}
		reachable, err := probeBackendRow(c, row)
		switch {
		case err != nil:
			parts = append(parts, fmt.Sprintf("%s: %s (unprobed)", want.label, row.Name))
			hints = append(hints, fmt.Sprintf("%s could not be probed: %s", row.Name, firstLine(err)))
			ok = false
		case reachable:
			parts = append(parts, fmt.Sprintf("%s: %s", want.label, row.Name))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s (unreachable)", want.label, row.Name))
			hints = append(hints, deadRowHint(row))
			ok = false
		}
	}
	mark := okMark()
	if !ok {
		mark = warnMark()
	}
	fmt.Println(label("Backends", "%s %s", strings.Join(parts, ", "), mark))
	for _, h := range hints {
		fmt.Println(indentHint(h))
	}
	return ok
}

func probeBackendRow(c *Client, row backendRow) (bool, error) {
	resp, _, err := c.Do(http.MethodPost, "/api/manage", map[string]any{"action": "backend-test", "id": row.ID})
	if err != nil {
		return false, err
	}
	if err := checkEnvelope(resp, envelopeRequired); err != nil {
		return false, err
	}
	var out struct {
		Reachable bool `json:"reachable"`
	}
	if err := json.Unmarshal(resp, &out); err != nil {
		return false, fmt.Errorf("unparseable backend-test response: %s", truncateForError(resp))
	}
	return out.Reachable, nil
}

// deadRowHint names the repair. The legacy-default fingerprint gets its own
// wording because its repair differs: those rows are not a configuration to
// fix but the residue of an env bootstrap that ran with nothing configured —
// they have to LEAVE the pool.
//
// The hint deliberately does NOT offer `seed --force` as a replacement. --force
// skips exactly one thing, the foreign-row guard; it then CREATES the target
// rows and leaves everything else untouched. The legacy rows would stay enabled
// at priority 100 next to the new ones — dead members of every failover chain,
// each costing a health-probe timeout. Removing them first is the only order
// that actually replaces them.
func deadRowHint(row backendRow) string {
	if legacyDefaultRow(row) {
		return fmt.Sprintf("%s (%s) is unreachable and carries the env-era default fingerprint — "+
			"`ctx backends seed --force` would ADD rows next to it, not replace it: remove it first with "+
			"`ctx backends delete %s`, then run `ctx backends seed` (parking it instead, "+
			"`ctx backends update %s '{\"enabled\":false}'`, keeps it out of the chains but leaves the seed needing --force)",
			row.Name, row.BaseURL, row.ID, row.ID)
	}
	return fmt.Sprintf("%s (%s) is unreachable — `ctx backends test %s` shows the failing check",
		row.Name, row.BaseURL, row.ID)
}

func legacyDefaultRow(row backendRow) bool {
	return legacyDefaultNames[row.Name] && strings.TrimSuffix(row.BaseURL, "/") == legacyDefaultHost
}

// ── Step 5: Version check ──────────────────────────────────────────.

type githubRelease struct {
	TagName string `json:"tag_name"`
}

// stepVersion is the ONE call in the whole CLI that leaves the user's ctxd and
// talks to a FOREIGN host. It therefore builds its OWN http.Client and must
// never be routed through the package Client or newHTTPClient's twin from
// stepServer: Client.Get/Post/Do set the header X-Context-Key unconditionally
// (client.go), so a "one client for everything" merge would hand the user's API
// key to a third party (design/03 §5.6). The rule is held by a grep invariant
// and by TestForeignHostClientCarriesNoKey, not by this comment.
func stepVersion() bool {
	local := Version
	fmt.Printf("") // anchor for the line

	// Own client, foreign host — see the doc comment above before merging it.
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", "https://api.github.com/repos/GottZ/ctx/releases/latest", nil)
	if err != nil {
		fmt.Println(label("Version", "%s (cannot check latest) %s", local, skipMark()))
		return true
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println(label("Version", "%s (offline) %s", local, skipMark()))
		return true
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		fmt.Println(label("Version", "%s (GitHub API %d) %s", local, resp.StatusCode, skipMark()))
		return true
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil || release.TagName == "" {
		fmt.Println(label("Version", "%s (parse error) %s", local, skipMark()))
		return true
	}

	remote := release.TagName
	// Normalize: strip leading 'v' for comparison
	localClean := strings.TrimPrefix(local, "v")
	remoteClean := strings.TrimPrefix(remote, "v")

	if localClean == remoteClean || local == "dev" {
		fmt.Println(label("Version", "%s (latest) %s", local, okMark()))
		return true
	}

	fmt.Println(label("Version", "%s -> %s available %s", local, remote, warnMark()))
	return true
}

// ── Step 6: Claude Code hooks ──────────────────────────────────────.

const hookCmdBrief = "ctx brief --hook"
const hookCmdPersist = "ctx persist --hook"

// settingsJSON is the subset of ~/.claude/settings.json we care about.
type settingsJSON struct {
	raw map[string]any
}

func loadSettings(path string) (*settingsJSON, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &settingsJSON{raw: make(map[string]any)}, nil
		}
		return nil, err
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse settings.json: %w", err)
	}
	return &settingsJSON{raw: raw}, nil
}

// settingsClock stamps the backup name; a variable so tests can pin it.
var settingsClock = time.Now

// maxSettingsBackupsPerSecond bounds the -2, -3 … suffix search of one
// timestamp — init rewrites the file at most twice per run.
const maxSettingsBackupsPerSecond = 100

// save rewrites settings.json and returns the path of the backup it made, or
// "" when none was needed. The rewrite re-sorts and re-indents the whole file,
// so an existing file whose content actually changes is first copied next to
// itself (#49 Punkt 3a) — the backup makes the change inspectable with a plain
// diff. A file whose serialized content would not change is not touched at
// all, and without a backup there is no rewrite: a failed copy aborts the save.
func (s *settingsJSON) save(path string) (string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(s.raw, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')

	backup := ""
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		if bytes.Equal(current, data) {
			return "", nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if backup, err = backupSettings(path, current, info.Mode().Perm()); err != nil {
			return "", fmt.Errorf("backup before rewrite: %w", err)
		}
	case !os.IsNotExist(err):
		return "", err
	}
	return backup, os.WriteFile(path, data, 0o600)
}

// backupSettings writes data next to path as <name>.bak-<UTC timestamp>, with
// the original's permission bits — settings.json can carry env secrets, so the
// copy must not be more readable than the file. O_EXCL: an existing backup is
// never replaced. Hooks and statusline are two rewrites in one run, usually in
// the same second; the second gets a -2 suffix instead of overwriting the copy
// that holds the pre-init original.
func backupSettings(path string, data []byte, perm os.FileMode) (string, error) {
	base := path + ".bak-" + settingsClock().UTC().Format("20060102T150405Z")
	for i := 1; i <= maxSettingsBackupsPerSecond; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		// OpenFile's mode is cut by the umask, Chmod's is not.
		if err := writeBackupFile(f, data, perm); err != nil {
			_ = os.Remove(name)
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("no free backup name for %s after %d attempts", base, maxSettingsBackupsPerSecond)
}

func writeBackupFile(f *os.File, data []byte, perm os.FileMode) error {
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// printSettingsBackup names the backup under a step's result line.
func printSettingsBackup(backup string) {
	if backup != "" {
		fmt.Println(indentHint("previous settings.json kept as " + displayConfigPath(backup)))
	}
}

// hasHookEntry reports whether any entry in the hook array contains the given
// command substring. It handles both the legacy flat form
// {"type":"command","command":…} and the current nested matcher-group form
// {"hooks":[{"type":"command","command":…}]}, so detection stays idempotent
// against either shape and re-running init does not append duplicates.
func hasHookEntry(hooks []any, cmdSubstr string) bool {
	for _, entry := range hooks {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := m["command"].(string); strings.Contains(cmd, cmdSubstr) {
			return true // legacy flat form
		}
		if inner, ok := m["hooks"].([]any); ok && hasHookEntry(inner, cmdSubstr) {
			return true // nested matcher-group form
		}
	}
	return false
}

// getHooksMap returns the "hooks" object, creating it if needed.
func (s *settingsJSON) getHooksMap() map[string]any {
	h, ok := s.raw["hooks"].(map[string]any)
	if !ok {
		h = make(map[string]any)
		s.raw["hooks"] = h
	}
	return h
}

// getHookArray returns the array for a specific hook event.
func getHookArray(hooks map[string]any, event string) []any {
	arr, ok := hooks[event].([]any)
	if !ok {
		return nil
	}
	return arr
}

// newHookGroup builds the nested matcher-group form Claude Code requires for a
// hook event: {"hooks":[{"type":"command","command":cmd}]}. The "matcher" key is
// omitted on purpose — SubagentStart/SubagentStop are not tool-scoped events. A
// bare {"type":"command",…} without this wrapper is silently ignored.
func newHookGroup(cmd string) map[string]any {
	return map[string]any{
		"hooks": []any{
			map[string]any{"type": "command", "command": cmd},
		},
	}
}

func stepHooks() bool {
	settingsPath := claudeSettingsPath()
	settings, err := loadSettings(settingsPath)
	if err != nil {
		fmt.Println(label("Hooks", "cannot read settings.json: %v %s", err, failMark()))
		return false
	}

	hooksMap := settings.getHooksMap()
	startArr := getHookArray(hooksMap, "SubagentStart")
	stopArr := getHookArray(hooksMap, "SubagentStop")

	hasStart := hasHookEntry(startArr, "ctx brief")
	hasStop := hasHookEntry(stopArr, "ctx persist")

	if hasStart && hasStop {
		fmt.Println(label("Hooks", "SubagentStart %s  SubagentStop %s", okMark(), okMark()))
		return true
	}

	// Show what's missing
	parts := []string{}
	if !hasStart {
		parts = append(parts, "SubagentStart")
	}
	if !hasStop {
		parts = append(parts, "SubagentStop")
	}
	fmt.Println(label("Hooks", "missing: %s", strings.Join(parts, ", ")))

	if !promptYesNo("  ? Install Claude Code hooks?") {
		fmt.Println(label("Hooks", "skipped %s", skipMark()))
		return false
	}

	// Add missing hooks in the nested matcher-group form (see newHookGroup).
	if !hasStart {
		startArr = append(startArr, newHookGroup(hookCmdBrief))
		hooksMap["SubagentStart"] = startArr
	}
	if !hasStop {
		stopArr = append(stopArr, newHookGroup(hookCmdPersist))
		hooksMap["SubagentStop"] = stopArr
	}

	backup, err := settings.save(settingsPath)
	if err != nil {
		fmt.Println(label("Hooks", "write failed: %v %s", err, failMark()))
		return false
	}

	fmt.Println(label("Hooks", "SubagentStart %s  SubagentStop %s", okMark(), okMark()))
	printSettingsBackup(backup)
	return true
}

// ── Step 7: Statusline ─────────────────────────────────────────────.

const statuslineCmd2 = "ctx statusline"

func stepStatusline() bool {
	return runStatuslineStep(prompt)
}

// runStatuslineStep is stepStatusline with the prompt injected. Claude Code has
// exactly one statusLine, so installing ctx's replaces whatever is there. A
// statusline that is not ctx's is therefore shown and asked about on its own,
// defaulting to keep (#49 Punkt 3b) — the generic install question used to
// replace it on a bare Enter.
func runStatuslineStep(ask func(question string) string) bool {
	settingsPath := claudeSettingsPath()
	settings, err := loadSettings(settingsPath)
	if err != nil {
		fmt.Println(label("Statusline", "cannot read settings.json: %v %s", err, failMark()))
		return false
	}

	existing := settings.raw["statusLine"]
	present := existing != nil // a JSON null configures nothing
	switch {
	case present && isCtxStatusLine(existing):
		fmt.Println(label("Statusline", "configured %s", okMark()))
		return true
	case present:
		fmt.Println(label("Statusline", "another statusline is configured"))
		fmt.Println(indentHint(describeStatusLine(existing)))
		if !answerIsYes(ask("  ? Replace it with ctx statusline? [y/N]: ")) {
			fmt.Println(label("Statusline", "kept the existing one %s", skipMark()))
			return false
		}
	default:
		fmt.Println(label("Statusline", "not configured"))
		if answer := ask("  ? Install statusline? [Y/n]: "); answer != "" && !answerIsYes(answer) {
			fmt.Println(label("Statusline", "skipped %s", skipMark()))
			return false
		}
	}

	settings.raw["statusLine"] = map[string]any{
		"type":    "command",
		"command": statuslineCmd2,
	}

	backup, err := settings.save(settingsPath)
	if err != nil {
		fmt.Println(label("Statusline", "write failed: %v %s", err, failMark()))
		return false
	}

	fmt.Println(label("Statusline", "configured %s", okMark()))
	printSettingsBackup(backup)
	return true
}

func isCtxStatusLine(v any) bool {
	m, _ := v.(map[string]any)
	cmd, _ := m["command"].(string)
	return strings.Contains(cmd, statuslineCmd2)
}

// describeStatusLine renders an existing statusLine for the question: its
// command in the usual {type, command} shape, the raw JSON otherwise. Quoted,
// so a command carrying control characters cannot rewrite the terminal line.
func describeStatusLine(v any) string {
	if m, ok := v.(map[string]any); ok {
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			return fmt.Sprintf("command %q", cmd)
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("value %v", v)
	}
	return "value " + strconv.Quote(truncateForError(raw))
}

// ── init command ───────────────────────────────────────────────────.

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Setup ctx config, hooks, and statusline",
		Long: `Interactive setup wizard for ctx. Checks and configures:

  1. Config file (~/.config/ctx/config) — API key and base URL; the key is
     form-checked (64 lowercase hex characters) and probed before it is saved
  2. Server connection — /health endpoint reachability
  3. API key — authenticated probe (GET /api/whoami): does the server accept it?
  4. Backend pool — seeds chat + embed backends when none serve (server-admin key)
  5. Version — compare local version against latest GitHub release
  6. Claude Code hooks — SubagentStart/SubagentStop in settings.json
  7. Statusline — ctx statusline in settings.json

Idempotent: re-running shows status for each item, only changes what's missing.
Respects NO_COLOR environment variable for plain text output.`,
		Example: `  ctx init          # Interactive setup
  NO_COLOR=1 ctx init  # Plain text output`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit()
		},
	}
}

func runInit() error {
	fmt.Printf("\nctx init %s\n\n", Version)

	var result initResult

	// Step 1: Config
	cfg, ok := stepConfig()
	result.ConfigOK = ok

	// Step 2: Server (only if config exists)
	if result.ConfigOK {
		result.ServerOK = stepServer(cfg)
	} else {
		fmt.Println(label("Server", "skipped (no config) %s", skipMark()))
	}

	// Step 3: Key (needs a reachable server)
	switch {
	case !result.ConfigOK:
		fmt.Println(label("Key", "skipped (no config) %s", skipMark()))
	case !result.ServerOK:
		fmt.Println(label("Key", "skipped (no server) %s", skipMark()))
	default:
		result.KeyOK = stepKey(cfg)
	}

	// Step 4: Backend pool (needs a reachable server and an accepted key)
	switch {
	case !result.ConfigOK || !result.ServerOK:
		fmt.Println(label("Backends", "skipped (no server) %s", skipMark()))
	case !result.KeyOK:
		fmt.Println(label("Backends", "skipped (key not verified) %s", skipMark()))
	default:
		result.BackendsOK = stepBackends(cfg)
	}

	// Step 5: Version
	result.VersionOK = stepVersion()

	// Step 6: Hooks
	result.HooksOK = stepHooks()

	// Step 7: Statusline
	result.StatuslineOK = stepStatusline()

	// Step 8: Summary
	fmt.Println()
	allGood := initSummaryOK(result)
	if allGood {
		if noColor() {
			fmt.Println("  [ok] ready")
		} else {
			fmt.Println("  \x1b[32m\u2713\x1b[0m ready")
		}
	} else {
		if noColor() {
			fmt.Println("  [!!] some steps need attention")
		} else {
			fmt.Println("  \x1b[33m!\x1b[0m some steps need attention")
		}
	}
	fmt.Println()

	return nil
}

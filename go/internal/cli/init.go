package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	BackendsOK   bool
	VersionOK    bool
	HooksOK      bool
	StatuslineOK bool
}

// initSummaryOK is the wizard's closing verdict. Backends and Version are
// tracked but deliberately NOT summed into it: both steps can end short of ✓
// for reasons that are none of init's business — a key below server-admin, an
// offline GitHub — and neither invalidates what init actually configures.
// init exits 0 regardless; the per-step line carries the detail.
func initSummaryOK(r initResult) bool {
	return r.ConfigOK && r.ServerOK && r.HooksOK && r.StatuslineOK
}

func stepConfig() (Config, bool) {
	cfgPath := clientconfig.FilePath()
	displayPath := displayConfigPath(cfgPath)

	cfg, err := clientconfig.Load()
	if err == nil {
		fmt.Println(label("Config", "%s exists %s", displayPath, okMark()))
		return cfg, true
	}

	fmt.Println(label("Config", "not found"))

	baseURL := prompt("  ? Base URL: ")
	if baseURL == "" {
		fmt.Println(label("Config", "skipped (no URL) %s", failMark()))
		return Config{}, false
	}
	apiKey := prompt("  ? API Key: ")
	if apiKey == "" {
		fmt.Println(label("Config", "skipped (no key) %s", failMark()))
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

	fmt.Println(label("Config", "saved to %s %s", displayPath, okMark()))
	return Config{BaseURL: baseURL, Key: apiKey}, true
}

// ── Step 2: Server health ──────────────────────────────────────────.

func stepServer(cfg Config) bool {
	baseURL := strings.TrimSuffix(cfg.BaseURL, "/")

	client := &http.Client{Timeout: 5 * time.Second}
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

// ── Step 3: Backend pool ───────────────────────────────────────────.

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
	if err := checkSettingsEnvelope(resp); err != nil {
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
	protocol := ask.line("  ? Protocol, ollama or openai [ollama]: ")
	if protocol == "" {
		protocol = string(backends.ProtocolOllama)
	}
	chatModel := ask.line("  ? Chat model: ")
	embedModel := ask.line("  ? Embedding model: ")
	key := ""
	if answerIsYes(ask.line("  ? Does the host need an API key? [y/N]: ")) {
		v, err := ask.secret("  ? API key (not echoed): ")
		if err != nil {
			return seedSpec{}, err
		}
		key = v
	}
	return seedSpec{
		Chat:  seedBackend{Host: host, Protocol: protocol, Model: chatModel, APIKey: key},
		Embed: seedBackend{Host: host, Protocol: protocol, Model: embedModel, APIKey: key},
	}, nil
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
	if err := checkSettingsEnvelope(resp); err != nil {
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

// ── Step 4: Version check ──────────────────────────────────────────.

type githubRelease struct {
	TagName string `json:"tag_name"`
}

func stepVersion() bool {
	local := Version
	fmt.Printf("") // anchor for the line

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

// ── Step 5: Claude Code hooks ──────────────────────────────────────.

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

func (s *settingsJSON) save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.raw, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
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

	if err := settings.save(settingsPath); err != nil {
		fmt.Println(label("Hooks", "write failed: %v %s", err, failMark()))
		return false
	}

	fmt.Println(label("Hooks", "SubagentStart %s  SubagentStop %s", okMark(), okMark()))
	return true
}

// ── Step 6: Statusline ─────────────────────────────────────────────.

const statuslineCmd2 = "ctx statusline"

func stepStatusline() bool {
	settingsPath := claudeSettingsPath()
	settings, err := loadSettings(settingsPath)
	if err != nil {
		fmt.Println(label("Statusline", "cannot read settings.json: %v %s", err, failMark()))
		return false
	}

	// Check existing statusLine
	sl, _ := settings.raw["statusLine"].(map[string]any)
	if sl != nil {
		cmd, _ := sl["command"].(string)
		if strings.Contains(cmd, "ctx statusline") {
			fmt.Println(label("Statusline", "configured %s", okMark()))
			return true
		}
	}

	fmt.Println(label("Statusline", "not configured"))

	if !promptYesNo("  ? Install statusline?") {
		fmt.Println(label("Statusline", "skipped %s", skipMark()))
		return false
	}

	settings.raw["statusLine"] = map[string]any{
		"type":    "command",
		"command": statuslineCmd2,
	}

	if err := settings.save(settingsPath); err != nil {
		fmt.Println(label("Statusline", "write failed: %v %s", err, failMark()))
		return false
	}

	fmt.Println(label("Statusline", "configured %s", okMark()))
	return true
}

// ── init command ───────────────────────────────────────────────────.

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Setup ctx config, hooks, and statusline",
		Long: `Interactive setup wizard for ctx. Checks and configures:

  1. Config file (~/.config/ctx/config) — API key and base URL
  2. Server connection — /health endpoint reachability
  3. Backend pool — seeds chat + embed backends when none serve (server-admin key)
  4. Version — compare local version against latest GitHub release
  5. Claude Code hooks — SubagentStart/SubagentStop in settings.json
  6. Statusline — ctx statusline in settings.json

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

	// Step 3: Backend pool (needs a reachable server and a key)
	if result.ConfigOK && result.ServerOK {
		result.BackendsOK = stepBackends(cfg)
	} else {
		fmt.Println(label("Backends", "skipped (no server) %s", skipMark()))
	}

	// Step 4: Version
	result.VersionOK = stepVersion()

	// Step 5: Hooks
	result.HooksOK = stepHooks()

	// Step 6: Statusline
	result.StatuslineOK = stepStatusline()

	// Step 7: Summary
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

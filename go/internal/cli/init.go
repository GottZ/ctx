package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	VersionOK    bool
	HooksOK      bool
	StatuslineOK bool
}

func stepConfig() (Config, bool) {
	cfgPath := configFilePath()
	displayPath := displayConfigPath(cfgPath)

	cfg, err := LoadConfig()
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

// ── Step 3: Version check ──────────────────────────────────────────.

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

// ── Step 4: Claude Code hooks ──────────────────────────────────────.

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

// ── Step 5: Statusline ─────────────────────────────────────────────.

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
  3. Version — compare local version against latest GitHub release
  4. Claude Code hooks — SubagentStart/SubagentStop in settings.json
  5. Statusline — ctx statusline in settings.json

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

	// Step 3: Version
	result.VersionOK = stepVersion()

	// Step 4: Hooks
	result.HooksOK = stepHooks()

	// Step 5: Statusline
	result.StatuslineOK = stepStatusline()

	// Step 6: Summary
	fmt.Println()
	allGood := result.ConfigOK && result.ServerOK && result.HooksOK && result.StatuslineOK
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

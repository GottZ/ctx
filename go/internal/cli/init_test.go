package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	if noColor() {
		t.Error("noColor() should return false when NO_COLOR is empty")
	}

	t.Setenv("NO_COLOR", "1")
	if !noColor() {
		t.Error("noColor() should return true when NO_COLOR is set")
	}
}

func TestOkMark(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := okMark(); got != "[ok]" {
		t.Errorf("okMark() = %q, want [ok]", got)
	}

	t.Setenv("NO_COLOR", "")
	if got := okMark(); got == "[ok]" {
		t.Error("okMark() should return ANSI when NO_COLOR is not set")
	}
}

func TestFailMark(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := failMark(); got != "[FAIL]" {
		t.Errorf("failMark() = %q, want [FAIL]", got)
	}
}

func TestWarnMark(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := warnMark(); got != "[WARN]" {
		t.Errorf("warnMark() = %q, want [WARN]", got)
	}
}

func TestSkipMark(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if got := skipMark(); got != "[skip]" {
		t.Errorf("skipMark() = %q, want [skip]", got)
	}
}

func TestLabel(t *testing.T) {
	got := label("Config", "exists %s", "[ok]")
	if got == "" {
		t.Error("label() returned empty string")
	}
	// Should contain the key and value
	if !contains(got, "Config") || !contains(got, "exists") || !contains(got, "[ok]") {
		t.Errorf("label() = %q, missing expected parts", got)
	}
	// Should contain dots
	if !contains(got, "...") {
		t.Errorf("label() = %q, missing dot padding", got)
	}
}

func TestDisplayConfigPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}

	path := filepath.Join(home, ".config", "ctx", "config")
	got := displayConfigPath(path)
	if got != "~/.config/ctx/config" {
		t.Errorf("displayConfigPath(%q) = %q, want ~/.config/ctx/config", path, got)
	}

	// Non-home path should pass through
	got = displayConfigPath("/tmp/test")
	if got != "/tmp/test" {
		t.Errorf("displayConfigPath(/tmp/test) = %q, want /tmp/test", got)
	}
}

func TestHasHookEntry(t *testing.T) {
	// legacy flat form (still detected for backward compatibility)
	flat := []any{
		map[string]any{"type": "command", "command": "ctx brief --hook"},
		map[string]any{"type": "command", "command": "some-other-cmd"},
	}

	if !hasHookEntry(flat, "ctx brief") {
		t.Error("should find 'ctx brief' in flat hooks")
	}
	if hasHookEntry(flat, "ctx persist") {
		t.Error("should not find 'ctx persist' in flat hooks")
	}
	if hasHookEntry(nil, "anything") {
		t.Error("nil hooks should return false")
	}

	// current nested matcher-group form
	nested := []any{newHookGroup("ctx persist --hook")}
	if !hasHookEntry(nested, "ctx persist") {
		t.Error("should find 'ctx persist' in nested matcher-group hooks")
	}
	if hasHookEntry(nested, "ctx brief") {
		t.Error("should not find 'ctx brief' in nested hooks")
	}
}

func TestNewHookGroup(t *testing.T) {
	g := newHookGroup("ctx brief --hook")
	inner, ok := g["hooks"].([]any)
	if !ok || len(inner) != 1 {
		t.Fatalf("newHookGroup should wrap one entry under \"hooks\", got %#v", g)
	}
	entry, ok := inner[0].(map[string]any)
	if !ok {
		t.Fatalf("inner entry should be a map, got %#v", inner[0])
	}
	if entry["type"] != "command" || entry["command"] != "ctx brief --hook" {
		t.Errorf("unexpected inner entry: %#v", entry)
	}
	if !hasHookEntry([]any{g}, "ctx brief") {
		t.Error("hasHookEntry should detect a newHookGroup entry")
	}
}

func TestLoadSettings_NewFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.json")

	// Non-existent file should return empty map
	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings() error: %v", err)
	}
	if s.raw == nil {
		t.Fatal("raw map should not be nil")
	}
	if len(s.raw) != 0 {
		t.Errorf("raw map should be empty, got %v", s.raw)
	}
}

func TestLoadSettings_ExistingFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.json")

	content := `{
  "customSlashCommands": [{"name": "test"}],
  "hooks": {
    "SubagentStart": [{"hooks": [{"type": "command", "command": "existing-cmd"}]}]
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings() error: %v", err)
	}

	// customSlashCommands should be preserved
	if _, ok := s.raw["customSlashCommands"]; !ok {
		t.Error("customSlashCommands should be preserved")
	}

	// Hooks should be readable
	hooks := s.getHooksMap()
	startArr := getHookArray(hooks, "SubagentStart")
	if len(startArr) != 1 {
		t.Errorf("SubagentStart should have 1 entry, got %d", len(startArr))
	}
}

func TestLoadSettings_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.json")

	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadSettings(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestSettingsSave_PreservesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "settings.json")

	original := `{
  "customSlashCommands": [{"name": "ctx"}],
  "environmentVariables": {"FOO": "bar"}
}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	// Add hooks
	hooks := s.getHooksMap()
	hooks["SubagentStart"] = []any{newHookGroup(hookCmdBrief)}

	if err := s.save(path); err != nil {
		t.Fatal(err)
	}

	// Re-read and verify
	data, _ := os.ReadFile(path)
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}

	// customSlashCommands preserved
	if _, ok := result["customSlashCommands"]; !ok {
		t.Error("customSlashCommands should be preserved after save")
	}

	// environmentVariables preserved
	if _, ok := result["environmentVariables"]; !ok {
		t.Error("environmentVariables should be preserved after save")
	}

	// hooks added
	hooks2, ok := result["hooks"].(map[string]any)
	if !ok {
		t.Fatal("hooks should exist after save")
	}
	start, ok := hooks2["SubagentStart"].([]any)
	if !ok || len(start) != 1 {
		t.Error("SubagentStart hook should have 1 entry")
	}
}

func TestSettingsSave_CreatesDir(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "subdir", "settings.json")

	s := &settingsJSON{raw: map[string]any{"test": true}}
	if err := s.save(path); err != nil {
		t.Fatalf("save() error: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist after save: %v", err)
	}
}

func TestGetHooksMap_CreatesIfMissing(t *testing.T) {
	s := &settingsJSON{raw: map[string]any{}}
	hooks := s.getHooksMap()
	if hooks == nil {
		t.Fatal("getHooksMap() should create hooks map")
	}

	// Should be persisted in raw
	if _, ok := s.raw["hooks"]; !ok {
		t.Error("hooks should be set in raw map")
	}
}

func TestClaudeSettingsPath(t *testing.T) {
	// Default (no CLAUDE_CONFIG_DIR): home-relative ~/.claude/settings.json.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	path := claudeSettingsPath()
	if path == "" {
		t.Error("claudeSettingsPath() returned empty string")
	}
	if !contains(path, ".claude") || !contains(path, "settings.json") {
		t.Errorf("claudeSettingsPath() = %q, expected .claude/settings.json", path)
	}

	// CLAUDE_CONFIG_DIR set: settings live directly under it (no ~/.claude).
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	want := filepath.Join(dir, "settings.json")
	if got := claudeSettingsPath(); got != want {
		t.Errorf("claudeSettingsPath() = %q, want %q", got, want)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
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

	if _, err := s.save(path); err != nil {
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
	if _, err := s.save(path); err != nil {
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

// ── Step 4: backend pool (A02-W3) ──────────────────────────────────
//
// The step's contract is the seed's contract seen from the wizard: it must not
// write below server-admin, it must produce the same full-trust payloads
// `ctx backends seed` produces, it must never echo an api_key, and it must
// report reachability instead of mere existence — while never turning any of
// that into a non-zero exit.

// initMock is a manage/secrets server standing in for ctxd.
type initMock struct {
	t         *testing.T
	srv       *httptest.Server
	whoami    map[string]any
	rows      []map[string]any
	reachable map[string]bool

	mu      sync.Mutex
	creates []map[string]any
	secrets map[string]string
	actions []string
}

func newInitMock(t *testing.T, whoami map[string]any, rows []map[string]any) *initMock {
	t.Helper()
	m := &initMock{t: t, whoami: whoami, rows: rows, reachable: map[string]bool{}, secrets: map[string]string{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.route))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *initMock) client() *Client {
	return NewClient(Config{BaseURL: m.srv.URL, Key: "test-key"})
}

func (m *initMock) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/whoami":
		writeMockJSON(w, m.whoami)
	case r.URL.Path == "/api/secrets" && r.Method == http.MethodGet:
		m.mu.Lock()
		names := make([]map[string]any, 0, len(m.secrets))
		for name := range m.secrets {
			names = append(names, map[string]any{"name": name})
		}
		m.mu.Unlock()
		writeMockJSON(w, map[string]any{"success": true, "secrets": names})
	case strings.HasPrefix(r.URL.Path, "/api/secrets/") && r.Method == http.MethodPut:
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.secrets[strings.TrimPrefix(r.URL.Path, "/api/secrets/")] = body["value"]
		m.mu.Unlock()
		writeMockJSON(w, map[string]any{"success": true})
	case r.URL.Path == "/api/manage":
		m.manage(w, r)
	default:
		m.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		writeMockJSON(w, map[string]any{"success": false, "error": "unexpected"})
	}
}

func (m *initMock) manage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string         `json:"action"`
		ID     string         `json:"id"`
		Data   map[string]any `json:"data"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	m.mu.Lock()
	m.actions = append(m.actions, req.Action)
	m.mu.Unlock()

	switch req.Action {
	case "backend-list":
		m.mu.Lock()
		rows := append([]map[string]any{}, m.rows...)
		m.mu.Unlock()
		writeMockJSON(w, map[string]any{"success": true, "backends": rows})
	case "backend-create":
		m.mu.Lock()
		m.creates = append(m.creates, req.Data)
		name, _ := req.Data["name"].(string)
		m.rows = append(m.rows, map[string]any{
			"id": "id-" + name, "name": name, "scope": "_global", "enabled": true,
			"base_url": req.Data["base_url"], "roles": req.Data["roles"],
		})
		m.reachable["id-"+name] = true
		m.mu.Unlock()
		writeMockJSON(w, map[string]any{"success": true})
	case "backend-test":
		m.mu.Lock()
		ok := m.reachable[req.ID]
		m.mu.Unlock()
		writeMockJSON(w, map[string]any{"success": true, "reachable": ok, "latency_ms": 1})
	default:
		m.t.Errorf("unexpected manage action %q", req.Action)
		writeMockJSON(w, map[string]any{"success": false, "error": "unexpected action"})
	}
}

func (m *initMock) did(action string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.actions {
		if a == action {
			return true
		}
	}
	return false
}

func writeMockJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// scriptedAsk replays fixed answers; the secret half records whether it was
// used at all, so "the key never travels the echoing path" is assertable.
func scriptedAsk(lines []string, secret string) (backendsAsk, *int) {
	i := 0
	secretCalls := 0
	return backendsAsk{
		line: func(string) string {
			if i >= len(lines) {
				return ""
			}
			answer := lines[i]
			i++
			return answer
		},
		secret: func(string) (string, error) {
			secretCalls++
			return secret, nil
		},
	}, &secretCalls
}

var serverAdmin = map[string]any{"success": true, "admin": true, "role": "admin", "home_scope": "_global"}

func TestInitBackendsStepSeedsEmptyPool(t *testing.T) {
	m := newInitMock(t, serverAdmin, nil)
	ask, secretCalls := scriptedAsk([]string{"http://gpu:11434", "", "qwen3", "qwen3-embedding", "n"}, "")

	var ok bool
	out := captureStdout(t, func() { ok = runBackendsStep(m.client(), ask) })
	if !ok {
		t.Fatalf("a freshly seeded, reachable pool must end the step green; output:\n%s", out)
	}
	if *secretCalls != 0 {
		t.Errorf("no api_key was asked for, the no-echo prompt must not run (%d calls)", *secretCalls)
	}
	if len(m.creates) != 2 {
		t.Fatalf("want 2 backend-create calls, got %d", len(m.creates))
	}
	byName := map[string]map[string]any{}
	for _, c := range m.creates {
		name, _ := c["name"].(string)
		byName[name] = c
	}
	for _, name := range []string{seedChatName, seedEmbedName} {
		payload, found := byName[name]
		if !found {
			t.Fatalf("wizard did not create %q — got %v", name, m.creates)
		}
		if payload["trust"] != "full-trust" {
			t.Errorf("%s: trust = %v, want full-trust", name, payload["trust"])
		}
		if payload["confirm_trust_elevation"] != true {
			t.Errorf("%s: confirm_trust_elevation = %v, want true", name, payload["confirm_trust_elevation"])
		}
		if payload["scope"] != "_global" {
			t.Errorf("%s: scope = %v, want _global", name, payload["scope"])
		}
		if payload["protocol"] != "ollama" {
			t.Errorf("%s: protocol = %v, want the ollama default for an empty answer", name, payload["protocol"])
		}
		if payload["base_url"] != "http://gpu:11434" {
			t.Errorf("%s: base_url = %v", name, payload["base_url"])
		}
	}
	if !m.did("backend-test") {
		t.Error("the step must PROBE the seeded rows — existence alone is not a serving signal")
	}
}

func TestInitBackendsStepSealsAPIKeyWithoutEcho(t *testing.T) {
	m := newInitMock(t, serverAdmin, nil)
	ask, secretCalls := scriptedAsk([]string{"https://api.example.com", "openai", "big", "big-embed", "y"}, "sk-live")

	var ok bool
	out := captureStdout(t, func() { ok = runBackendsStep(m.client(), ask) })
	if !ok {
		t.Fatalf("seed with a key must succeed; output:\n%s", out)
	}
	if *secretCalls != 1 {
		t.Errorf("the api_key must be read exactly once through the no-echo prompt, got %d", *secretCalls)
	}
	if len(m.secrets) == 0 {
		t.Fatal("no secret was sealed — the key would have had to travel in the row")
	}
	for _, c := range m.creates {
		body, _ := json.Marshal(c)
		if strings.Contains(string(body), "sk-live") {
			t.Fatalf("plaintext api_key leaked into a backend-create payload: %s", body)
		}
		if c["api_key_ref"] == nil {
			t.Errorf("%v: api_key_ref missing although a key was given", c["name"])
		}
	}
	if strings.Contains(out, "sk-live") {
		t.Error("the api_key must never reach the terminal")
	}
}

// Negative probe 1: neither a plain key nor a tenant-admin key may reach a
// write — and init keeps running either way.
func TestInitBackendsStepRefusesBelowServerAdmin(t *testing.T) {
	cases := []struct {
		name   string
		whoami map[string]any
	}{
		{"non-admin", map[string]any{"success": true, "admin": false, "role": "user", "home_scope": "acme"}},
		{"tenant-admin", map[string]any{"success": true, "admin": false, "role": "owner", "home_scope": "acme"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newInitMock(t, tc.whoami, nil)
			ask, secretCalls := scriptedAsk([]string{"http://gpu:11434", "", "m", "e", "n"}, "")

			var ok bool
			out := captureStdout(t, func() { ok = runBackendsStep(m.client(), ask) })
			if ok {
				t.Error("a key below server-admin must not report a verified pool")
			}
			if len(m.creates) != 0 || m.did("backend-create") {
				t.Errorf("no row may be written: %v", m.creates)
			}
			if m.did("backend-list") {
				t.Error("the tier gate must come before the pool read")
			}
			if *secretCalls != 0 {
				t.Error("no prompting may happen once the tier gate refused")
			}
			if !strings.Contains(out, "server-admin key") || !strings.Contains(out, "ctx backends seed") {
				t.Errorf("the hint must name the missing tier and the later seed path; got:\n%s", out)
			}
			if !strings.Contains(out, "docs/operations.md#backends") {
				t.Errorf("the hint must point at the runbook anchor; got:\n%s", out)
			}
		})
	}
}

// Negative probe 2: a populated but dead pool is a named hint, not an abort —
// and the env-era default rows are named as what they are.
func TestInitBackendsStepNamesDeadLegacyRows(t *testing.T) {
	rows := []map[string]any{
		{"id": "id-chat", "name": "herbert-chat", "scope": "_global", "enabled": true,
			"base_url": legacyDefaultHost, "roles": []string{"synthesis", "chat"}},
		{"id": "id-embed", "name": "llama-embed", "scope": "_global", "enabled": true,
			"base_url": legacyDefaultHost, "roles": []string{"embed"}},
	}
	m := newInitMock(t, serverAdmin, rows)
	ask, _ := scriptedAsk(nil, "")

	var ok bool
	out := captureStdout(t, func() { ok = runBackendsStep(m.client(), ask) })
	if ok {
		t.Error("dead rows must not pass as a serving pool")
	}
	if len(m.creates) != 0 {
		t.Errorf("a populated pool must not be seeded over: %v", m.creates)
	}
	if !strings.Contains(out, "herbert-chat") || !strings.Contains(out, "unreachable") {
		t.Errorf("the dead row must be named; got:\n%s", out)
	}
	// The remediation has to be one that actually removes the legacy rows.
	// `seed --force` only skips the foreign-row guard and CREATES the target
	// rows — the legacy pair would stay enabled at priority 100 next to them,
	// dead members of every failover chain.
	if !strings.Contains(out, "ctx backends delete id-chat") {
		t.Errorf("the legacy fingerprint must name the removal command; got:\n%s", out)
	}
	if strings.Contains(out, "replace it with `ctx backends seed --force`") {
		t.Errorf("the hint must not promise that --force replaces the legacy rows; got:\n%s", out)
	}
}

// A tenant row of the same role does not make the shared pipelines serve.
func TestPoolRowForRoleIgnoresTenantRows(t *testing.T) {
	rows := []backendRow{
		{ID: "t", Name: "tenant-chat", Scope: "acme", Enabled: true, Roles: []string{"synthesis"}},
		{ID: "d", Name: "disabled", Scope: "_global", Enabled: false, Roles: []string{"embed"}},
	}
	if _, ok := poolRowForRole(rows, "synthesis"); ok {
		t.Error("a tenant-scoped row must not count as serving the _global pool")
	}
	if _, ok := poolRowForRole(rows, "embed"); ok {
		t.Error("a disabled row must not count as serving")
	}
	if got := missingSeedRoles(rows); len(got) != 2 {
		t.Errorf("missingSeedRoles = %v, want both roles missing", got)
	}
}

// A row held by an ACTIVE disable-profile is out of every chain (Chain's
// disabledBy arm) although its enabled column still says true. Counting it as
// serving is the false-green case: the wizard reports a verified pool while
// every query dies at the embedding step.
func TestPoolRowForRoleExcludesProfileDisabled(t *testing.T) {
	rows := []backendRow{
		{ID: "e", Name: "embed-primary", Scope: "_global", Enabled: true, Roles: []string{"embed"},
			EffectiveState: "profile-disabled", DisabledByProfiles: []string{"eject"}},
		{ID: "c", Name: "chat-primary", Scope: "_global", Enabled: true, Roles: []string{"synthesis"},
			EffectiveState: "cooldown"},
	}
	if _, ok := poolRowForRole(rows, "embed"); ok {
		t.Error("a profile-disabled row must not count as serving — it is out of every chain")
	}
	if _, ok := poolRowForRole(rows, "synthesis"); !ok {
		t.Error("cooldown only reorders the chain, it must not disqualify a row")
	}
	if got := missingSeedRoles(rows); len(got) != 1 || got[0] != "embed" {
		t.Errorf("missingSeedRoles = %v, want exactly [embed]", got)
	}
}

// backend-list renders the pool snapshot in load order (ORDER BY scope, name),
// NOT priority DESC. The probe must therefore SELECT the chain head instead of
// taking the first match, or a dead primary hides behind an alphabetically
// earlier failover row (false green) and vice versa (false red).
func TestPoolRowForRolePicksHighestPriority(t *testing.T) {
	rows := []backendRow{
		{ID: "a", Name: "aaa-failover", Scope: "_global", Enabled: true, Roles: []string{"synthesis"}, Priority: 10},
		{ID: "z", Name: "zzz-primary", Scope: "_global", Enabled: true, Roles: []string{"synthesis"}, Priority: 100},
	}
	row, ok := poolRowForRole(rows, "synthesis")
	if !ok || row.ID != "z" {
		t.Errorf("poolRowForRole picked %+v, want the priority-100 chain head zzz-primary", row)
	}
	// Equal priority falls back to Chain's tie-break: name ASC.
	rows[0].Priority = 100
	if row, _ = poolRowForRole(rows, "synthesis"); row.ID != "a" {
		t.Errorf("equal priority must tie-break by name like Chain does, got %+v", row)
	}
}

func TestDeadRowHintDistinguishesLegacyFingerprint(t *testing.T) {
	legacy := backendRow{ID: "x", Name: "llama-embed", BaseURL: legacyDefaultHost + "/"}
	if !legacyDefaultRow(legacy) {
		t.Fatal("a trailing slash must not hide the fingerprint")
	}
	// The legacy repair is a REMOVAL, not `seed --force`: --force skips the
	// foreign-row guard and creates the target rows, leaving the legacy pair
	// enabled at priority 100 in every failover chain.
	hint := deadRowHint(legacy)
	if !contains(hint, "ctx backends delete x") {
		t.Errorf("legacy hint lacks the removal command: %s", hint)
	}
	if contains(hint, "replace it with") {
		t.Errorf("legacy hint still promises a replacement --force does not perform: %s", hint)
	}
	own := backendRow{ID: "019e", Name: "chat-primary", BaseURL: "http://gpu:11434"}
	if legacyDefaultRow(own) {
		t.Error("a seeded row must never be mistaken for the env-era default")
	}
	if hint := deadRowHint(own); contains(hint, "--force") || !contains(hint, "ctx backends test 019e") {
		t.Errorf("a configured row is repaired, not replaced: %s", hint)
	}
}

// The step never turns into a non-zero exit: its verdict stays out of the
// summary, so a non-admin's init still ends "ready".
func TestInitSummaryIgnoresBackendsVerdict(t *testing.T) {
	base := initResult{ConfigOK: true, ServerOK: true, KeyOK: true, HooksOK: true, StatuslineOK: true}
	if !initSummaryOK(base) {
		t.Fatal("baseline should be ready")
	}
	if !initSummaryOK(initResult{ConfigOK: true, ServerOK: true, KeyOK: true, HooksOK: true, StatuslineOK: true, BackendsOK: false}) {
		t.Error("a degraded backends step must not flip the summary — init stays usable for non-admins")
	}
	if initSummaryOK(initResult{ConfigOK: true, ServerOK: true, KeyOK: true, HooksOK: false, StatuslineOK: true, BackendsOK: true}) {
		t.Error("the steps init actually owns must still count")
	}
}

// PIN (design/02 §4.1b, corrected review finding): a fresh install after the
// cut answers /health with 503 + status "unhealthy" because its pool is empty.
// stepServer reads the BODY and ignores the status code — exactly that keeps
// the wizard usable as the onboarding path. A later "hardening" that refuses
// unhealthy or checks resp.StatusCode would break the only seeding route and
// must fail here first.
func TestStepServerAcceptsUnhealthy503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"unhealthy","services":{"database":"ok"}}`))
	}))
	defer srv.Close()

	var ok bool
	out := captureStdout(t, func() { ok = stepServer(Config{BaseURL: srv.URL, Key: "k"}) })
	if !ok {
		t.Fatalf("init must accept unhealthy/503 — an empty pool is the state it exists to fix; output:\n%s", out)
	}
	if !strings.Contains(out, "unhealthy") {
		t.Errorf("the state must still be shown, not swallowed; got:\n%s", out)
	}
}

func TestStepServerRejectsUnparseableBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	var ok bool
	_ = captureStdout(t, func() { ok = stepServer(Config{BaseURL: srv.URL, Key: "k"}) })
	if ok {
		t.Error("an unparseable /health body is not a reachable ctx server")
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

// TestInitBackendsStepAnthropicAsksEmbedLegSeparately: choosing the anthropic
// wire turns the single-host wizard into a two-host one — the Messages API
// serves no embeddings — and collects the window and the key the chat leg
// needs without the "does the host need a key?" detour (it always does).
func TestInitBackendsStepAnthropicAsksEmbedLegSeparately(t *testing.T) {
	m := newInitMock(t, serverAdmin, nil)
	ask, secretCalls := scriptedAsk([]string{
		"https://api.anthropic.com", "anthropic", "claude-opus-5-5", "qwen3-embedding",
		"",                 // context window → default 100000
		"http://gpu:11434", // embedding host
		"",                 // embedding protocol → ollama default
		"n",                // embedding host needs no key
	}, "sk-ant-live")

	var ok bool
	out := captureStdout(t, func() { ok = runBackendsStep(m.client(), ask) })
	if !ok {
		t.Fatalf("anthropic seed must succeed; output:\n%s", out)
	}
	if *secretCalls != 1 {
		t.Errorf("the key prompt must run exactly once (%d calls)", *secretCalls)
	}
	if strings.Contains(out, "sk-ant-live") {
		t.Error("the key was echoed")
	}
	byName := map[string]map[string]any{}
	for _, c := range m.creates {
		name, _ := c["name"].(string)
		byName[name] = c
	}
	chat, embed := byName[seedChatName], byName[seedEmbedName]
	if chat == nil || embed == nil {
		t.Fatalf("wizard did not create both rows: %v", m.creates)
	}
	if chat["protocol"] != "anthropic" || chat["base_url"] != "https://api.anthropic.com" {
		t.Errorf("chat row = %v", chat)
	}
	if n, _ := chat["num_ctx"].(float64); int(n) != 100000 {
		// The mock round-trips the payload through JSON, hence float64.
		if n2, ok := chat["num_ctx"].(int); !ok || n2 != 100000 {
			t.Errorf("chat num_ctx = %v, want the 100000 default", chat["num_ctx"])
		}
	}
	if chat["api_key_ref"] != seedChatName+"-key" {
		t.Errorf("chat api_key_ref = %v", chat["api_key_ref"])
	}
	if embed["protocol"] != "ollama" || embed["base_url"] != "http://gpu:11434" {
		t.Errorf("embed row = %v, want the separately asked ollama host", embed)
	}
	if _, has := embed["api_key_ref"]; has {
		t.Errorf("the Anthropic key must not be attached to the embed row: %v", embed)
	}
}

// TestInitBackendsStepAnthropicRejectsBadWindow: a non-numeric context
// window aborts the step before anything is sealed or written — a silent
// default would seed the wrong prompt budget.
func TestInitBackendsStepAnthropicRejectsBadWindow(t *testing.T) {
	m := newInitMock(t, serverAdmin, nil)
	ask, secretCalls := scriptedAsk([]string{
		"https://api.anthropic.com", "anthropic", "claude-opus-5-5", "qwen3-embedding", "100k",
	}, "sk-ant-live")
	var ok bool
	out := captureStdout(t, func() { ok = runBackendsStep(m.client(), ask) })
	if ok {
		t.Fatalf("a bad window must fail the step; output:\n%s", out)
	}
	if !strings.Contains(out, "not a positive integer") {
		t.Errorf("output lacks the reason:\n%s", out)
	}
	if *secretCalls != 0 || len(m.creates) != 0 {
		t.Errorf("secret prompts %d, creates %d — want nothing asked or written", *secretCalls, len(m.creates))
	}
}

// ── API key: form check + authenticated probe (#40 Bug 8b) ─────────
//
// Before this, any non-empty string became CTX_KEY and the only network check
// was the unauthenticated /health — a typo got a green "healthy" and a broken
// config. The contract now: a malformed key is named by its counted deviation
// and re-asked, a well-formed one is probed against an authenticated endpoint
// before it is saved, and the offline path still saves (unverified).

const goodTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// keyServer is a ctxd stand-in for GET /api/whoami: the one key it knows gets
// 200 + identity, everything else the Auth middleware's 401.
type keyServer struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string // X-Context-Key of every whoami request
}

func newKeyServer(t *testing.T, accepted string) *keyServer {
	t.Helper()
	ks := &keyServer{}
	ks.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/whoami" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		key := r.Header.Get("X-Context-Key")
		ks.mu.Lock()
		ks.calls = append(ks.calls, key)
		ks.mu.Unlock()
		if key != accepted {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		writeMockJSON(w, map[string]any{"success": true, "label": "laptop", "home_scope": "private"})
	}))
	t.Cleanup(ks.srv.Close)
	return ks
}

func (ks *keyServer) probes() []string {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return append([]string(nil), ks.calls...)
}

// isolatedConfigHome points the client config at an empty temp dir and clears
// every environment source clientconfig.Load would otherwise read.
func isolatedConfigHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("CTX_BASE_URL", "")
	t.Setenv("CTX_KEY", "")
	t.Setenv("WEBHOOK_BASE_URL", "")
	return filepath.Join(dir, "ctx", "config")
}

func scriptedLines(lines ...string) func(string) string {
	i := 0
	return func(string) string {
		if i >= len(lines) {
			return ""
		}
		answer := lines[i]
		i++
		return answer
	}
}

func TestKeyFormProblem(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want []string // substrings; empty = well-formed
	}{
		{"real key", goodTestKey, nil},
		{"one char short", goodTestKey[:63], []string{"63 characters, expected 64"}},
		{"one char long", goodTestKey + "0", []string{"65 characters, expected 64"}},
		{"two typos", goodTestKey[:4] + "g" + goodTestKey[5:16] + "z" + goodTestKey[17:],
			[]string{"2 non-hex characters at positions 5, 17"}},
		{"one typo, singular", "x" + goodTestKey[1:], []string{"1 non-hex character at position 1"}},
		{"uppercase", "AB" + goodTestKey[2:], []string{"2 uppercase characters at positions 1, 2", "lowercase"}},
		{"short and typo", goodTestKey[:10] + "-" + goodTestKey[11:63],
			[]string{"63 characters, expected 64", "1 non-hex character at position 11"}},
		{"multibyte counts as one character", "ä" + goodTestKey[1:],
			[]string{"1 non-hex character at position 1"}},
		{"oauth access token", "ctxt_" + goodTestKey, []string{"OAuth token", "API key"}},
		{"oauth refresh token", "ctxr_" + goodTestKey, []string{"OAuth token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := keyFormProblem(tc.key)
			if len(tc.want) == 0 {
				if got != "" {
					t.Errorf("well-formed key reported as %q", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("keyFormProblem = %q, want it to contain %q", got, w)
				}
			}
		})
	}
	// A long run of garbage lists a bounded number of positions, not 60.
	if got := keyFormProblem(strings.Repeat("z", 64)); strings.Count(got, ",") > 10 {
		t.Errorf("position list must stay bounded: %q", got)
	}
}

// The prefixes mirror the server's credential grammar (resolveCredential).
func TestOAuthPrefixesMirrorServer(t *testing.T) {
	if oauthAccessTokenPrefix != store.AccessTokenPrefix || oauthRefreshTokenPrefix != store.RefreshTokenPrefix {
		t.Errorf("cli prefixes %q/%q drifted from store %q/%q", oauthAccessTokenPrefix, oauthRefreshTokenPrefix,
			store.AccessTokenPrefix, store.RefreshTokenPrefix)
	}
}

// A malformed key is re-asked with the counted deviation and never reaches the
// server; the corrected key is probed and saved.
func TestConfigStepReasksMalformedKey(t *testing.T) {
	cfgPath := isolatedConfigHome(t)
	ks := newKeyServer(t, goodTestKey)

	var cfg Config
	var ok bool
	out := captureStdout(t, func() {
		cfg, ok = runConfigStep(scriptedLines(ks.srv.URL, goodTestKey[:63], goodTestKey))
	})
	if !ok || cfg.Key != goodTestKey {
		t.Fatalf("the corrected key must be accepted, got ok=%v key=%q; output:\n%s", ok, cfg.Key, out)
	}
	if !strings.Contains(out, "63 characters, expected 64") {
		t.Errorf("the deviation must be counted; output:\n%s", out)
	}
	if got := ks.probes(); len(got) != 1 || got[0] != goodTestKey {
		t.Errorf("only the well-formed key may be probed, got %q", got)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(data), "CTX_KEY="+goodTestKey+"\n") || strings.Contains(string(data), goodTestKey[:63]+"\n") {
		t.Errorf("config must carry exactly the accepted key:\n%s", data)
	}
}

// A well-formed key the server rejects is never written: "saved" means the key
// works. Three rejections end the step without a config.
func TestConfigStepRefusesRejectedKey(t *testing.T) {
	cfgPath := isolatedConfigHome(t)
	ks := newKeyServer(t, goodTestKey)
	wrong := strings.Repeat("e", 64)

	var ok bool
	out := captureStdout(t, func() {
		_, ok = runConfigStep(scriptedLines(ks.srv.URL, wrong, wrong, wrong, goodTestKey))
	})
	if ok {
		t.Fatalf("a rejected key must not end the step green; output:\n%s", out)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("a rejected key must never be written (stat err = %v)", err)
	}
	if !strings.Contains(out, "401") {
		t.Errorf("the rejection must be named; output:\n%s", out)
	}
	if got := len(ks.probes()); got != 3 {
		t.Errorf("want exactly 3 attempts, got %d probes", got)
	}
}

// The offline path stays: an unreachable server cannot verify the key, the
// config is written anyway and the line says the key is unverified.
func TestConfigStepSavesUnverifiedWhenUnreachable(t *testing.T) {
	cfgPath := isolatedConfigHome(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	var ok bool
	out := captureStdout(t, func() {
		_, ok = runConfigStep(scriptedLines(deadURL, goodTestKey))
	})
	if !ok {
		t.Fatalf("offline must still save the config; output:\n%s", out)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("config must be written on the offline path: %v", err)
	}
	if !strings.Contains(out, "not verified") {
		t.Errorf("the line must say the key is unverified; output:\n%s", out)
	}
}

// An existing config is probed too — a re-run must not report a broken key
// as ready.
func TestStepKeyProbesExistingConfig(t *testing.T) {
	isolatedConfigHome(t)
	ks := newKeyServer(t, goodTestKey)

	var ok bool
	out := captureStdout(t, func() { ok = stepKey(Config{BaseURL: ks.srv.URL, Key: goodTestKey}) })
	if !ok || !strings.Contains(out, "laptop") {
		t.Errorf("accepted key must pass and name its label; ok=%v output:\n%s", ok, out)
	}

	out = captureStdout(t, func() { ok = stepKey(Config{BaseURL: ks.srv.URL, Key: goodTestKey[:63]}) })
	if ok {
		t.Errorf("a rejected key must fail the step; output:\n%s", out)
	}
	for _, want := range []string{"rejected", "401", "63 characters, expected 64", "CTX_KEY in"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestInitSummaryCountsKeyVerdict(t *testing.T) {
	if initSummaryOK(initResult{ConfigOK: true, ServerOK: true, KeyOK: false, HooksOK: true, StatuslineOK: true}) {
		t.Error("a key the server does not accept must not end init as ready")
	}
}

// ── settings.json: backup before rewrite, foreign statusline (#49 Punkt 3) ──
//
// The rewrite re-sorts and re-indents the whole file, so an existing file
// whose content changes is first copied next to itself; a statusline that is
// not ctx's is shown and only replaced on an explicit yes.

// fixedSettingsClock pins the backup timestamp for one test.
func fixedSettingsClock(t *testing.T) {
	t.Helper()
	orig := settingsClock
	settingsClock = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.FixedZone("CEST", 2*3600)) }
	t.Cleanup(func() { settingsClock = orig })
}

// backups lists the settings.json.bak-* files in dir.
func backups(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "settings.json.bak-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func writeSettingsFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile's mode is cut by the umask
		t.Fatal(err)
	}
}

func TestSettingsSaveBacksUpChangedFile(t *testing.T) {
	fixedSettingsClock(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := "{\"model\":\"opus\",   \"env\":{\"A\":\"1\"}}"
	writeSettingsFile(t, path, original, 0o640)

	s, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	s.raw["statusLine"] = map[string]any{"type": "command", "command": statuslineCmd2}
	backup, err := s.save(path)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// UTC, not the clock's zone: 12:00 CEST is 10:00Z.
	want := filepath.Join(dir, "settings.json.bak-20261022T100000Z")
	if backup != want {
		t.Fatalf("backup = %q, want %q", backup, want)
	}
	data, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("backup not written: %v", err)
	}
	if string(data) != original {
		t.Errorf("backup must be the byte-exact original, got %q", data)
	}
	info, err := os.Stat(backup)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Errorf("backup mode = %v, want the original's 0640", info.Mode().Perm())
	}
	if now, _ := os.ReadFile(path); !strings.Contains(string(now), statuslineCmd2) {
		t.Errorf("the rewrite itself must still happen: %s", now)
	}
}

func TestSettingsSaveUnchangedFileIsNotTouched(t *testing.T) {
	fixedSettingsClock(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	canonical := "{\n  \"model\": \"opus\"\n}\n" // exactly what save would write
	writeSettingsFile(t, path, canonical, 0o600)

	s, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := s.save(path)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if backup != "" || len(backups(t, dir)) != 0 {
		t.Errorf("an unchanged file needs no backup, got %q / %v", backup, backups(t, dir))
	}
}

func TestSettingsSaveNewFileHasNoBackup(t *testing.T) {
	fixedSettingsClock(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	backup, err := (&settingsJSON{raw: map[string]any{"a": 1}}).save(path)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if backup != "" || len(backups(t, dir)) != 0 {
		t.Errorf("nothing existed, nothing to back up: %q / %v", backup, backups(t, dir))
	}
}

// Hooks and statusline are two rewrites in the same run, usually in the same
// second. The second backup must not replace the first — that one holds the
// pre-init original.
func TestSettingsSaveNeverOverwritesABackup(t *testing.T) {
	fixedSettingsClock(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := `{"model":"opus"}`
	writeSettingsFile(t, path, original, 0o600)

	s, _ := loadSettings(path)
	s.getHooksMap()["SubagentStart"] = []any{newHookGroup(hookCmdBrief)}
	first, err := s.save(path)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, _ := os.ReadFile(path)
	s2, _ := loadSettings(path)
	s2.raw["statusLine"] = map[string]any{"type": "command", "command": statuslineCmd2}
	second, err := s2.save(path)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("want two distinct backups, got %q and %q", first, second)
	}
	if got, _ := os.ReadFile(first); string(got) != original {
		t.Errorf("first backup lost the original: %q", got)
	}
	if got, _ := os.ReadFile(second); string(got) != string(intermediate) {
		t.Errorf("second backup must hold the state the second rewrite replaced: %q", got)
	}
}

// A statusline that is not ctx's is shown and kept unless the answer is yes.
func TestStatuslineStepKeepsForeignByDefault(t *testing.T) {
	fixedSettingsClock(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	original := `{"statusLine":{"type":"command","command":"bash ~/.claude/my-line.sh"}}`
	writeSettingsFile(t, path, original, 0o600)

	var ok bool
	out := captureStdout(t, func() { ok = runStatuslineStep(scriptedLines("")) })
	if ok {
		t.Error("keeping a foreign statusline is a skip, not a configured ctx statusline")
	}
	if !strings.Contains(out, "bash ~/.claude/my-line.sh") {
		t.Errorf("the existing command must be shown before asking; output:\n%s", out)
	}
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Errorf("the default answer must keep the file byte-identical, got %s", got)
	}
	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("no rewrite, no backup: %v", b)
	}
}

func TestStatuslineStepReplacesForeignOnYes(t *testing.T) {
	fixedSettingsClock(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	original := `{"statusLine":{"type":"command","command":"bash ~/.claude/my-line.sh"}}`
	writeSettingsFile(t, path, original, 0o600)

	var ok bool
	out := captureStdout(t, func() { ok = runStatuslineStep(scriptedLines("y")) })
	if !ok {
		t.Fatalf("an explicit yes installs the ctx statusline; output:\n%s", out)
	}
	s, _ := loadSettings(path)
	sl, _ := s.raw["statusLine"].(map[string]any)
	if sl["command"] != statuslineCmd2 {
		t.Errorf("statusLine = %v, want %q", s.raw["statusLine"], statuslineCmd2)
	}
	b := backups(t, dir)
	if len(b) != 1 {
		t.Fatalf("want exactly one backup in CLAUDE_CONFIG_DIR, got %v", b)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, b[0])); string(got) != original {
		t.Errorf("backup must hold the replaced statusline: %s", got)
	}
	if !strings.Contains(out, b[0]) {
		t.Errorf("the step must name the backup; output:\n%s", out)
	}
}

// A statusLine value of an unexpected shape is foreign too — it must not be
// overwritten as if nothing were configured.
func TestStatuslineStepTreatsOddShapeAsForeign(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	path := filepath.Join(dir, "settings.json")
	original := `{"statusLine":"my-line"}`
	writeSettingsFile(t, path, original, 0o600)

	out := captureStdout(t, func() { _ = runStatuslineStep(scriptedLines("")) })
	if got, _ := os.ReadFile(path); string(got) != original {
		t.Errorf("an unknown statusLine value must be kept on the default answer, got %s; output:\n%s", got, out)
	}
}

func TestStatuslineStepInstallsWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	var ok bool
	out := captureStdout(t, func() { ok = runStatuslineStep(scriptedLines("")) })
	if !ok {
		t.Fatalf("no statusline yet: the default answer installs it; output:\n%s", out)
	}
	s, _ := loadSettings(filepath.Join(dir, "settings.json"))
	if sl, _ := s.raw["statusLine"].(map[string]any); sl["command"] != statuslineCmd2 {
		t.Errorf("statusLine = %v", s.raw["statusLine"])
	}
}

package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// ── Step 3: backend pool (A02-W3) ──────────────────────────────────
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
	base := initResult{ConfigOK: true, ServerOK: true, HooksOK: true, StatuslineOK: true}
	if !initSummaryOK(base) {
		t.Fatal("baseline should be ready")
	}
	if !initSummaryOK(initResult{ConfigOK: true, ServerOK: true, HooksOK: true, StatuslineOK: true, BackendsOK: false}) {
		t.Error("a degraded backends step must not flip the summary — init stays usable for non-admins")
	}
	if initSummaryOK(initResult{ConfigOK: true, ServerOK: true, HooksOK: false, StatuslineOK: true, BackendsOK: true}) {
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

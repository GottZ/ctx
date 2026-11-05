package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// exportableRolesKey is the export filter of the tenant-alias chain
// (design/02 §4.4.2, §4.4.3 no. 3; wave W02-07).
const exportableRolesKey = "gateway.exportable_roles"

// isEmptyList reports whether a rendered value is an empty, non-nil string
// list — the form that marshals to [] rather than null.
func isEmptyList(v any) bool {
	l, ok := v.([]string)
	return ok && l != nil && len(l) == 0
}

// TestExportableRolesRegisteredEmpty is gate G6-Config (a): the key exists
// with the empty default, typed as a role list (not "scopes"), hot and
// global-only, and the boot record "config: effective" shows the empty
// default as [] — through the JSON handler the operator reads it from.
func TestExportableRolesRegisteredEmpty(t *testing.T) {
	ki, ok := KeyByName(exportableRolesKey)
	if !ok {
		t.Fatalf("%s: key not registered", exportableRolesKey)
	}
	if ki.Type != "roles" {
		t.Errorf("type %q, want roles", ki.Type)
	}
	if ki.EnvVar != "CTX_GATEWAY_EXPORTABLE_ROLES" {
		t.Errorf("env var %q, want CTX_GATEWAY_EXPORTABLE_ROLES", ki.EnvVar)
	}
	if ki.Mutability != "hot" || ki.Tenancy != TenancyGlobalOnly {
		t.Errorf("mutability %q tenancy %q, want hot global-only", ki.Mutability, ki.Tenancy)
	}
	if !isEmptyList(ki.Default) {
		t.Errorf("default %#v, want an empty list", ki.Default)
	}

	resetBuildEnv(t)
	c, issues := FromEnv()
	for _, is := range issues {
		if is.Field == exportableRolesKey {
			t.Errorf("issue on the default: %s: %s", is.Severity, is.Msg)
		}
	}
	if src := c.Source(exportableRolesKey); src != "default" {
		t.Errorf("source %q, want default", src)
	}
	if got := c.GatewaySettings().ExportableRoles; len(got) != 0 {
		t.Errorf("GatewaySettings().ExportableRoles = %v, want empty", got)
	}

	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("config: effective", BootDumpArgs(c, issues)...)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("boot record is no JSON: %v", err)
	}
	gw, _ := rec["gateway"].(map[string]any)
	got, present := gw["exportable_roles"]
	if !present {
		t.Fatal("boot record carries no gateway.exportable_roles")
	}
	if arr, isArr := got.([]any); !isArr || len(arr) != 0 {
		t.Errorf("boot record renders gateway.exportable_roles as %#v, want []", got)
	}
}

// TestExportableRolesSettingsWriteResetsToEmpty is gate G6-Config (b): a
// settings write can set the list and put it back to empty. Build is the
// candidate build HandlePut runs (settings.BuildFromRowsScoped → config.Build;
// settings.ScalarValue passes the JSON string "" through as ""), so an
// override that Build does not apply is the 422 of the write.
func TestExportableRolesSettingsWriteResetsToEmpty(t *testing.T) {
	resetBuildEnv(t)
	set := func(raw string) *Config {
		t.Helper()
		c, issues := Build([]Override{{Key: exportableRolesKey, Value: raw}}, nil)
		for _, is := range issues {
			if is.Field == exportableRolesKey {
				t.Errorf("write %q: %s: %s", raw, is.Severity, is.Msg)
			}
		}
		if src := c.Source(exportableRolesKey); src != SourceSettings {
			t.Errorf("write %q: source %q, want %s — the settings write would be a 422", raw, src, SourceSettings)
		}
		return c
	}

	c := set("chat,embed")
	if got := []string(c.Gateway.ExportableRoles); !slices.Equal(got, []string{"chat", "embed"}) {
		t.Errorf("after chat,embed: %v", got)
	}
	if got := c.GatewaySettings().ExportableRoles; !slices.Equal(got, []string{"chat", "embed"}) {
		t.Errorf("after chat,embed: GatewaySettings().ExportableRoles = %v", got)
	}

	c = set("")
	if got := c.Gateway.ExportableRoles; len(got) != 0 {
		t.Errorf("after the reset: %v, want empty", got)
	}
	if got := c.GatewaySettings().ExportableRoles; len(got) != 0 {
		t.Errorf("after the reset: GatewaySettings().ExportableRoles = %v, want empty", got)
	}
	if v, _ := RenderValue(c, exportableRolesKey); !isEmptyList(v) {
		t.Errorf("after the reset the settings API renders %#v, want []", v)
	}
}

// TestExportableRolesSettingsCopy: the oai snapshot owns its slice — a
// consumer appending to or editing it cannot reach the config snapshot.
func TestExportableRolesSettingsCopy(t *testing.T) {
	resetBuildEnv(t)
	c, _ := Build([]Override{{Key: exportableRolesKey, Value: "chat,embed"}}, nil)
	s := c.GatewaySettings()
	s.ExportableRoles[0] = "mutated"
	if got := c.Gateway.ExportableRoles[0]; got != "chat" {
		t.Errorf("config snapshot changed through GatewaySettings(): %q", got)
	}
}

// TestParseRoleListValue pins the parser of the role list type: empty is a
// value (the empty list), parts are comma-split and trimmed, empty parts and
// repeats drop out, and every part must be a pool role by the
// ck_model_aliases_role rule.
func TestParseRoleListValue(t *testing.T) {
	long := strings.Repeat("a", 64)
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"   ", []string{}},
		{",", []string{}},
		{"chat", []string{"chat"}},
		{" chat , embed ", []string{"chat", "embed"}},
		{"chat,,embed,", []string{"chat", "embed"}},
		{"chat,embed,chat", []string{"chat", "embed"}},
		{"dream-embed,tenant:fast,a_b,9x", []string{"dream-embed", "tenant:fast", "a_b", "9x"}},
		{long, []string{long}},
	} {
		v, err := parseRoleListValue(tc.in, nil)
		if err != nil {
			t.Errorf("parse(%q): %v", tc.in, err)
			continue
		}
		got, ok := v.(RoleList)
		if !ok || got == nil || !slices.Equal([]string(got), tc.want) {
			t.Errorf("parse(%q) = %#v, want %q", tc.in, v, tc.want)
		}
	}
	for _, in := range []string{
		"chat,bad role!",
		"Chat",
		"-chat",
		"_chat",
		"chat;embed",
		"chat/embed",
		long + "a",
	} {
		if v, err := parseRoleListValue(in, nil); err == nil {
			t.Errorf("parse(%q) = %#v, want an error", in, v)
		}
	}
}

// TestExportableRolesInvalidRoleRefused: an invalid role is refused on both
// source paths — the settings write (Build ⇒ WARN, override not applied ⇒
// 422) and env (tolerant key: WARN, the empty default stays, fail-closed:
// nothing is exported). The control case with valid roles applies.
func TestExportableRolesInvalidRoleRefused(t *testing.T) {
	resetBuildEnv(t)
	c, issues := Build([]Override{{Key: exportableRolesKey, Value: "chat,bad role!"}}, nil)
	warnFor(t, issues, exportableRolesKey, `invalid role "bad role!"`)
	if src := c.Source(exportableRolesKey); src == SourceSettings {
		t.Errorf("an invalid role list was applied from settings")
	}
	if got := c.Gateway.ExportableRoles; len(got) != 0 {
		t.Errorf("invalid write left %v, want the empty default", got)
	}

	t.Setenv("CTX_GATEWAY_EXPORTABLE_ROLES", "chat,bad role!")
	c, issues = FromEnv()
	warnFor(t, issues, exportableRolesKey, `invalid role "bad role!"`)
	if got := c.Gateway.ExportableRoles; len(got) != 0 {
		t.Errorf("invalid env left %v, want the empty default", got)
	}

	t.Setenv("CTX_GATEWAY_EXPORTABLE_ROLES", "chat,embed")
	c, issues = FromEnv()
	for _, is := range issues {
		if is.Field == exportableRolesKey {
			t.Errorf("valid env: %s: %s", is.Severity, is.Msg)
		}
	}
	if got := c.GatewaySettings().ExportableRoles; !slices.Equal(got, []string{"chat", "embed"}) {
		t.Errorf("valid env: %v, want [chat embed]", got)
	}
}

// TestRoleNameRuleIsTheAliasRoleCheck: the role rule of the parser is the
// CHECK ck_model_aliases_role of migration 162 (which handler/model_alias_manage.go
// mirrors as modelAliasRoleRE) — the export filter compares against
// alias.Role, so a role that rule refuses could never match an alias. config
// cannot import the handler (the handler imports config), hence the pin
// against the frozen migration instead of a shared symbol.
func TestRoleNameRuleIsTheAliasRoleCheck(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "162_model_aliases.sql"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`CONSTRAINT ck_model_aliases_role\s+CHECK \(role ~ '([^']+)'\)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("ck_model_aliases_role not found in migration 162")
	}
	if got := roleNameRE.String(); got != string(m[1]) {
		t.Errorf("roleNameRE %q, ck_model_aliases_role %q", got, m[1])
	}
}

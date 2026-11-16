package config

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// releasedMCPInstructions is the frozen record of every shipped wording
// (#315): id, the release that first shipped it, and the sha256 of its text.
// The history is append-only — a stored value equal to any wording follows
// the newest one, and a custom text names its base by id — so a released entry
// that changes would silently turn some installation's text into a custom
// one, or point its base at the wrong wording. Extend this list when a release
// appends a wording; never edit an existing line.
var releasedMCPInstructions = []struct {
	id     int
	since  string
	sha256 string
}{
	{1, "v0.26.0", "87edfd0f3976a8b349551a3664479208f5079d3946fe828617cca7f1ed6b8da4"},
	{2, "v5.49.0", "1d97f291c6ebd395e4272435f052a07e98d0224d86a7de9a4120d82f985c36b0"},
}

func TestShippedMCPInstructionsHistoryIsFrozen(t *testing.T) {
	h := ShippedMCPInstructions()
	if len(h) < len(releasedMCPInstructions) {
		t.Fatalf("history has %d wordings, %d were released — a released wording was removed", len(h), len(releasedMCPInstructions))
	}
	for i, r := range releasedMCPInstructions {
		sum := sha256.Sum256([]byte(h[i].Text))
		if h[i].ID != r.id || h[i].Since != r.since || hex.EncodeToString(sum[:]) != r.sha256 {
			t.Errorf("released wording #%d changed: id %d since %q sha256 %s, frozen as id %d since %q sha256 %s",
				i+1, h[i].ID, h[i].Since, hex.EncodeToString(sum[:]), r.id, r.since, r.sha256)
		}
	}
	for i, e := range h {
		if e.ID != i+1 {
			t.Errorf("wording at position %d has id %d; ids count from 1 without gaps", i, e.ID)
		}
		if e.Text == "" || e.Since == "" {
			t.Errorf("wording #%d lacks its text or its release", e.ID)
		}
		if issues := validateMCPInstructions(&Config{MCP: MCPServerConfig{Instructions: e.Text}}); len(issues) != 0 {
			t.Errorf("wording #%d fails its own validation: %v", e.ID, issues)
		}
	}
}

// The registry default of mcp.instructions IS the newest shipped wording —
// one place in the code, every surface that renders the default agrees.
func TestMCPInstructionsDefaultIsTheNewestShippedWording(t *testing.T) {
	h := ShippedMCPInstructions()
	info, ok := KeyByName("mcp.instructions")
	if !ok {
		t.Fatal("mcp.instructions is not in the registry")
	}
	if info.Default != h[len(h)-1].Text {
		t.Errorf("default = %q, want the newest shipped wording %q", info.Default, h[len(h)-1].Text)
	}
	if got := Defaults().MCPInstructions(); got.State != MCPInstructionsShipped || got.Sent != h[len(h)-1].Text {
		t.Errorf("defaults resolve to %+v, want the newest shipped wording sent", got)
	}
}

func TestResolveMCPInstructions(t *testing.T) {
	h := []ShippedInstructions{{1, "v1", "old"}, {2, "v2", "mid"}, {3, "v3", "new"}}
	cases := []struct {
		name       string
		enabled    bool
		configured string
		state      string
		sent       string
		matches    int
	}{
		{"default (newest) is shipped", true, "new", MCPInstructionsShipped, "new", 3},
		{"a stored older wording follows the newest", true, "old", MCPInstructionsShipped, "new", 1},
		{"own text is sent as is", true, "own\nrules", MCPInstructionsCustom, "own\nrules", 0},
		{"a near miss is a custom text", true, "new ", MCPInstructionsCustom, "new ", 0},
		{"off sends nothing", false, "new", MCPInstructionsOff, "", 3},
		{"off keeps a custom text unsent", false, "own", MCPInstructionsOff, "", 0},
	}
	for _, c := range cases {
		got := ResolveMCPInstructions(c.enabled, c.configured, h)
		if got.State != c.state || got.Sent != c.sent || got.Matches != c.matches || got.Current.ID != 3 {
			t.Errorf("%s: got %+v, want state %s sent %q matches %d current 3", c.name, got, c.state, c.sent, c.matches)
		}
	}
}

// V60: the text that reaches foreign models is bounded at the write.
func TestValidateMCPInstructions(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"at the cap in characters", strings.Repeat("ä", MCPInstructionsMaxChars), ""},
		{"one over the cap", strings.Repeat("x", MCPInstructionsMaxChars+1), "at most 2048"},
		{"emoji at the cap (two units each)", strings.Repeat("\U0001F600", MCPInstructionsMaxChars/2), ""},
		{"emoji one over the cap", strings.Repeat("\U0001F600", MCPInstructionsMaxChars/2) + "x", "at most 2048"},
		{"blank", " \n\t", "blank"},
		{"empty", "", "blank"},
		{"escape sequence", "look\x1b[2J", "control character U+001B"},
		{"NUL", "a\x00b", "control character U+0000"},
		{"invalid UTF-8", "a\xffb", "not valid UTF-8"},
		{"tabs and line breaks", "rule 1\n\trule 2\r\n", ""},
	}
	for _, c := range cases {
		issues := validateMCPInstructions(&Config{MCP: MCPServerConfig{Instructions: c.text}})
		switch {
		case c.want == "" && len(issues) != 0:
			t.Errorf("%s: unexpected issues %v", c.name, issues)
		case c.want != "" && (len(issues) != 1 || issues[0].Severity != SeverityError || issues[0].Field != "mcp.instructions" || !strings.Contains(issues[0].Msg, c.want)):
			t.Errorf("%s: issues %v, want one error on mcp.instructions naming %q", c.name, issues, c.want)
		}
	}
}

// The @shipped default tag belongs to mcp.instructions alone.
func TestShippedDefaultTagIsMCPInstructionsOnly(t *testing.T) {
	type bad struct {
		X string `key:"other.text" env:"-" default:"@shipped" mut:"hot" tenancy:"global-only"`
	}
	if _, err := buildRegistry(reflect.TypeOf(bad{})); err == nil || !strings.Contains(err.Error(), "mcp.instructions alone") {
		t.Errorf("buildRegistry on another key with @shipped: err = %v, want a refusal", err)
	}
}

package config

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// ShippedInstructions is one wording of the MCP server instructions that a
// ctx release sent with initialize (GottZ/ctx#315). The instructions are the
// one text every connecting agent gets before it has read any documentation,
// so an operator who replaced it needs to see when ctx changed its own.
type ShippedInstructions struct {
	// ID is the wording's position in the history, counted from 1. It is the
	// stable identity a custom text records as its base (context_settings
	// metadata "shipped_base") and never changes once released.
	ID int `json:"id"`
	// Since is the ctx release that first shipped the wording.
	Since string `json:"since"`
	// Text is the wording, byte for byte as it was sent.
	Text string `json:"text"`
}

// shippedMCPInstructions is the history of every wording ctx has shipped, in
// release order. It is APPEND-ONLY: an entry, once released, is never edited
// or removed, because a stored value equal to any entry is read as "no custom
// text" (it follows the newest wording), and a custom text names its base by
// ID. A new wording is a new entry at the end; the newest entry is the default
// of mcp.instructions (registry shippedDefaultTag). TestShippedMCPInstructions
// HistoryIsFrozen pins every released entry.
var shippedMCPInstructions = []ShippedInstructions{
	{ID: 1, Since: "v0.26.0", Text: "ctx is a persistent knowledge store with hybrid retrieval. Use 'query' for questions, 'store' to save knowledge, 'search' for lightweight lookups."},
	{ID: 2, Since: "v5.49.0", Text: "ctx is your memory across sessions. Before you answer from memory or start work, look it up: 'search' for a lightweight lookup, 'query' for a question that needs a synthesized answer. When you learn something that should outlast this session, save it with 'store' and put its source into metadata.source."},
}

// ShippedMCPInstructions returns a copy of the shipped history, oldest first.
func ShippedMCPInstructions() []ShippedInstructions {
	return append([]ShippedInstructions(nil), shippedMCPInstructions...)
}

// MCPInstructionsMaxChars caps an operator's instructions text, in characters
// counted as UTF-16 code units — the way JavaScript, and so Claude Code,
// counts them (a character outside the Basic Multilingual Plane, most emoji
// among them, counts two). That count is never below the number of runes, so
// the cap is conservative for any client that counts characters.
// The text reaches the context of every agent of every tenant on every
// connection, unfiltered, so its size is a cost each session pays before its
// first turn. 2,048 is the largest length that every Claude Code release since
// 2.1.84 passes to the model uncut (its cap on server instructions was 2,048
// characters until 2.1.296 and is 4,096 since; the rest is dropped without a
// warning) — and about eight times the shipped wording, room for a team's
// working rules. A longer text is refused when it is written, never cut when
// it is sent.
const MCPInstructionsMaxChars = 2048

// MCP instructions states, as the settings surfaces name them.
const (
	MCPInstructionsShipped = "shipped" // sends the newest shipped wording
	MCPInstructionsCustom  = "custom"  // sends the operator's own text
	MCPInstructionsOff     = "off"     // sends nothing
)

// MCPInstructionsState is the resolved state of the two mcp.instructions
// keys: what the server sends with initialize and why. One function computes
// it for every reader — the MCP server, the settings API and the boot dump.
type MCPInstructionsState struct {
	State string // MCPInstructionsShipped | MCPInstructionsCustom | MCPInstructionsOff
	// Sent is the text the next initialize answer carries; empty when Off.
	Sent string
	// Current is the newest shipped wording.
	Current ShippedInstructions
	// Matches is the ID of the shipped wording the configured text equals
	// byte for byte, 0 when it equals none. A configured text that equals any
	// shipped wording is not a custom text: it follows the newest one.
	Matches int
}

// ResolveMCPInstructions resolves enabled and the configured text against a
// shipped history (ShippedMCPInstructions in production). The configured
// text is the snapshot value of mcp.instructions: the registry default (the
// newest wording at registry build) when nobody set it, or the stored
// override.
func ResolveMCPInstructions(enabled bool, configured string, history []ShippedInstructions) MCPInstructionsState {
	st := MCPInstructionsState{Current: history[len(history)-1]}
	for _, h := range history {
		if h.Text == configured {
			st.Matches = h.ID
		}
	}
	switch {
	case !enabled:
		st.State = MCPInstructionsOff
	case st.Matches != 0:
		st.State = MCPInstructionsShipped
		st.Sent = st.Current.Text
	default:
		st.State = MCPInstructionsCustom
		st.Sent = configured
	}
	return st
}

// MCPInstructions resolves this snapshot's two mcp.instructions keys against
// the shipped history.
func (c *Config) MCPInstructions() MCPInstructionsState {
	return ResolveMCPInstructions(c.MCP.InstructionsEnabled, c.MCP.Instructions, shippedMCPInstructions)
}

// validateMCPInstructions is V60: the instructions text is sent unfiltered
// to foreign models, so it must be valid UTF-8 without control characters
// other than tab and line breaks, at most MCPInstructionsMaxChars long, and
// not blank. A blank text has two meanings an operator could intend — send
// nothing, or send the shipped wording — and each has its own explicit form
// (mcp.instructions_enabled false; removing the override), so a blank value
// is refused rather than read as either. Fatal: a settings PUT is a 422, a
// stored row is dropped at boot with a WARN.
func validateMCPInstructions(c *Config) []Issue {
	t := c.MCP.Instructions
	fail := func(format string, args ...any) []Issue {
		return []Issue{{Field: "mcp.instructions", Severity: SeverityError, Msg: fmt.Sprintf(format, args...)}}
	}
	if !utf8.ValidString(t) {
		return fail("the instructions text is not valid UTF-8")
	}
	if strings.TrimSpace(t) == "" {
		return fail("the instructions text is blank — to send nothing set mcp.instructions_enabled to false, to send the text ctx ships remove the override (ctx settings unset mcp.instructions)")
	}
	if n := len(utf16.Encode([]rune(t))); n > MCPInstructionsMaxChars {
		return fail("the instructions text has %d characters (UTF-16 code units; an emoji counts two), at most %d are allowed", n, MCPInstructionsMaxChars)
	}
	for i, r := range t {
		if unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return fail("the instructions text contains the control character U+%04X at byte %d; only tab and line breaks are allowed", r, i)
		}
	}
	return nil
}

// H13a — negative probe for the claimed capability boundary "the chat's
// closing call carries NO tools array" (design/04 §4.8 table row 1, §2.5-g).
//
// The claim rests on ONE struct tag: openAIStreamRequest.Tools carries
// `json:"tools,omitempty"` (stream.go:220). Without omitempty the wire body
// would carry `"tools":null` — a key the model side is free to interpret,
// and the E4 reason for omitting the array (tool_choice:"none" makes models
// emit tool syntax as plain text) would be silently undone.
//
// This probe asserts the ABSENCE of the key on the marshalled body, on all
// three shapes a caller can produce (nil slice, empty non-nil slice, and the
// extraBody merge path that round-trips the body through map[string]any) —
// plus the contrast case that a real tool DOES reach the wire, so the
// absence assertions cannot go vacuum-green.
//
// Mutation that turns this RED: drop `,omitempty` from stream.go:220.
package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// bodyKeys marshals a stream request body and returns it decoded as a generic
// object plus the raw text — key PRESENCE is the assertion, so a `"tools":null`
// must be visible (a typed decode would erase it).
func bodyKeys(t *testing.T, tools []ToolDef, extra map[string]any) (map[string]any, string) {
	t.Helper()
	raw, err := buildStreamBody("m", []ChatMsg{{Role: "user", Content: "hi"}}, tools, Options{}, extra)
	if err != nil {
		t.Fatalf("buildStreamBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v (%s)", err, raw)
	}
	return m, string(raw)
}

func TestBuildStreamBodyOmitsToolsKeyWithoutTools(t *testing.T) {
	cases := []struct {
		name  string
		tools []ToolDef
		extra map[string]any
	}{
		// engine.go:519 finalCall → runStream(..., toolsEnabled=false, ...):
		// runStream leaves `tools` at its nil zero value.
		{"nil slice", nil, nil},
		// A non-nil but empty slice must behave identically — omitempty
		// treats len==0 the same, and no call site may rely on the difference.
		{"empty slice", []ToolDef{}, nil},
		// extraBody re-marshals the body through map[string]any (stream.go
		// merge path). A `"tools":null` would SURVIVE that round trip, so the
		// merge path needs its own assertion.
		{"nil slice with extraBody", nil, map[string]any{"provider": map[string]any{"zdr": true}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, raw := bodyKeys(t, tc.tools, tc.extra)
			if v, has := m["tools"]; has {
				t.Fatalf("wire body carries the key \"tools\" (= %v) — the closing call must OMIT it (E4); body: %s", v, raw)
			}
			if strings.Contains(raw, `"tools"`) {
				t.Fatalf("raw body mentions \"tools\": %s", raw)
			}
		})
	}
}

// TestBuildStreamBodyCarriesToolsWhenGiven is the contrast half: the absence
// assertions above only mean something if the same builder DOES emit the array
// when a tool is passed.
func TestBuildStreamBodyCarriesToolsWhenGiven(t *testing.T) {
	tool := ToolDef{Type: "function", Function: ToolDefFunction{
		Name:        "ctx_query",
		Description: "probe",
		Parameters:  json.RawMessage(`{"type":"object"}`),
	}}
	m, raw := bodyKeys(t, []ToolDef{tool}, nil)
	arr, ok := m["tools"].([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("tools = %v, want a 1-entry array; body: %s", m["tools"], raw)
	}
	fn, _ := arr[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "ctx_query" {
		t.Fatalf("tool name = %v, want ctx_query", fn["name"])
	}
}

package handler

import (
	"encoding/json"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/store"
)

func intp(v int) *int { return &v }

// TestContentWindow pins the window over code points: every cut lands on a
// character boundary, the length counts code points, and truncated says the
// content goes on after the window.
func TestContentWindow(t *testing.T) {
	const s = "aä€😀b" // 1, 2, 3, 4 and 1 bytes
	for _, c := range []struct {
		name          string
		content       string
		offset, limit int
		want          string
		length        int
		truncated     bool
	}{
		{"empty content", "", 0, -1, "", 0, false},
		{"empty content with a limit", "", 0, 3, "", 0, false},
		{"whole", s, 0, -1, s, 5, false},
		{"start", s, 0, 2, "aä", 5, true},
		{"middle", s, 1, 3, "ä€😀", 5, true},
		{"four bytes alone", s, 3, 1, "😀", 5, true},
		{"end", s, 3, 10, "😀b", 5, false},
		{"exactly to the end", s, 3, 2, "😀b", 5, false},
		{"at the end", s, 5, 2, "", 5, false},
		{"past the end", s, 9, -1, "", 5, false},
		{"limit 0", s, 0, 0, "", 5, true},
		{"limit 0 on the last character", s, 4, 0, "", 5, true},
		{"limits near the int range do not overflow", s, 2, math.MaxInt, "€😀b", 5, false},
		{"offsets near the int range", s, math.MaxInt, math.MaxInt, "", 5, false},
	} {
		got, length, truncated := contentWindow(c.content, c.offset, c.limit)
		if got != c.want || length != c.length || truncated != c.truncated || !utf8.ValidString(got) {
			t.Errorf("%s: contentWindow(%q, %d, %d) = (%q, %d, %t), want (%q, %d, %t)",
				c.name, c.content, c.offset, c.limit, got, length, truncated, c.want, c.length, c.truncated)
		}
	}
}

// TestNewGetOptions pins the checks both surfaces run before any lookup.
func TestNewGetOptions(t *testing.T) {
	if o, err := newGetOptions(nil, nil, nil); o != nil || err != nil {
		t.Errorf("no options = (%+v, %v), want (nil, nil): the full read", o, err)
	}
	if o, err := newGetOptions([]string{}, nil, nil); o != nil || err != nil {
		t.Errorf("an empty list = (%+v, %v), want (nil, nil): the full read", o, err)
	}
	if o, err := newGetOptions([]string{"title", "title", "id"}, nil, nil); err != nil || len(o.fields) != 2 || o.window {
		t.Errorf("fields [title title id] = (%+v, %v)", o, err)
	}
	if o, err := newGetOptions(nil, intp(0), nil); err != nil || !o.window || o.fields != nil || o.limit != -1 {
		t.Errorf("offset only = (%+v, %v), want a window to the end over the whole block", o, err)
	}
	if o, err := newGetOptions([]string{}, nil, intp(3)); err != nil || !o.window || o.fields != nil {
		t.Errorf("an empty list with a window = (%+v, %v), want the window over the whole block", o, err)
	}
	if o, err := newGetOptions([]string{"content"}, nil, intp(0)); err != nil || !o.window || o.limit != 0 {
		t.Errorf("limit 0 = (%+v, %v), want an empty window", o, err)
	}
	_, err := newGetOptions([]string{"title", "Title"}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `unknown field "Title"`) || !strings.Contains(err.Error(), strings.Join(getFieldNames, ", ")) {
		t.Errorf("an unknown name = %v, want it named with every valid name", err)
	}
	for _, c := range []struct {
		name          string
		fields        []string
		offset, limit *int
		want          string
	}{
		{"negative offset", nil, intp(-1), nil, "content_offset must be 0 or more"},
		{"negative limit", nil, nil, intp(-1), "content_limit must be 0 or more"},
		{"window beside fields without content", []string{"title"}, nil, intp(5), getWindowNeedsContentMsg},
		{"offset beside fields without content", []string{"metadata"}, intp(0), nil, getWindowNeedsContentMsg},
	} {
		if o, err := newGetOptions(c.fields, c.offset, c.limit); o != nil || err == nil || err.Error() != c.want {
			t.Errorf("%s = (%+v, %v), want %q", c.name, o, err, c.want)
		}
	}
}

// TestGetOptionsOmit pins which columns the store read leaves unread.
func TestGetOptionsOmit(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields []string
		limit  *int
		want   store.BlockReadOmit
	}{
		{"no options", nil, nil, store.BlockReadOmit{}},
		{"a window over the whole block", nil, intp(3), store.BlockReadOmit{}},
		{"title", []string{"title"}, nil, store.BlockReadOmit{Content: true, Metadata: true}},
		{"content", []string{"content"}, nil, store.BlockReadOmit{Metadata: true}},
		{"content with a window", []string{"content"}, intp(3), store.BlockReadOmit{Metadata: true}},
		{"metadata", []string{"metadata", "type"}, nil, store.BlockReadOmit{Content: true}},
		{"both", []string{"metadata", "content"}, nil, store.BlockReadOmit{}},
	} {
		o, err := newGetOptions(c.fields, nil, c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := o.omit(); got != c.want {
			t.Errorf("%s: omit = %+v, want %+v", c.name, got, c.want)
		}
	}
}

// TestGetOptionsAnswer pins the projection on a block value: the named keys
// and the framing keys, encoded and ordered as on the full answer, the window
// keys right after content; nil options answer the block itself.
func TestGetOptionsAnswer(t *testing.T) {
	b := &store.Block{ID: "019f0000-0000-7000-8000-000000000407", Title: "t", Content: "abc", Tags: []string{"x"},
		Untrusted: true, UntrustedOrigin: "checkpoint", Archived: true, TypeName: "knowledge"}
	var none *getOptions
	if got, err := none.answer(b); err != nil || got != any(b) {
		t.Errorf("nil options answer = (%v, %v), want the block itself", got, err)
	}
	for _, c := range []struct {
		name   string
		fields []string
		limit  *int
		want   string
	}{
		{"title", []string{"title"}, nil,
			`{"id":"019f0000-0000-7000-8000-000000000407","title":"t","untrusted":true,"untrusted_origin":"checkpoint","archived":true}`},
		{"type and content with a window, named in any order", []string{"type", "content", "title"}, intp(2),
			`{"id":"019f0000-0000-7000-8000-000000000407","title":"t","content":"ab","content_length":3,"content_truncated":true,"type":"knowledge","untrusted":true,"untrusted_origin":"checkpoint","archived":true}`},
	} {
		o, err := newGetOptions(c.fields, nil, c.limit)
		if err != nil {
			t.Fatal(err)
		}
		got, err := o.answer(b)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		if string(raw) != c.want {
			t.Errorf("%s = %s, want %s", c.name, raw, c.want)
		}
		indented, err := json.MarshalIndent(got, "", "  ")
		if err != nil || !json.Valid(indented) || !strings.HasPrefix(string(indented), "{\n  \"id\": ") {
			t.Errorf("%s indented = %s (%v), want an object led by id", c.name, indented, err)
		}
	}
	if b.Content != "abc" {
		t.Errorf("the answer changed the block it read: content %q", b.Content)
	}
}

// TestGetFieldNamesMatchTheBlock pins getFieldNames against the keys
// store.Block encodes, so a key added to the block is either answerable by
// name or listed here as one the by-id read never fills — and against the
// MCP field description, which lists the names for the model.
func TestGetFieldNamesMatchTheBlock(t *testing.T) {
	neverFilled := []string{"guard_status", "workflow_status"}
	var keys []string
	bt := reflect.TypeFor[store.Block]()
	for i := 0; i < bt.NumField(); i++ {
		name, _, _ := strings.Cut(bt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" && !slices.Contains(neverFilled, name) {
			keys = append(keys, name)
		}
	}
	if !reflect.DeepEqual(keys, getFieldNames) {
		t.Errorf("store.Block keys = %v\ngetFieldNames   = %v", keys, getFieldNames)
	}
	f, _ := reflect.TypeFor[getInput]().FieldByName("Fields")
	if desc := f.Tag.Get("jsonschema"); !strings.Contains(desc, strings.Join(getFieldNames, ", ")) {
		t.Errorf("the fields description does not list the names in order: %q", desc)
	}
	for k := range getFramingKeys {
		if !slices.Contains(getFieldNames, k) {
			t.Errorf("framing key %q is no field name", k)
		}
	}
}

// TestMCPGetSchemaAdvertisesFieldsAndWindow reads tools/list over the real
// transport (issue #407): `fields` is an optional list of strings,
// content_offset and content_limit are optional integers, and id stays the one
// required argument.
func TestMCPGetSchemaAdvertisesFieldsAndWindow(t *testing.T) {
	var list struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				InputSchema struct {
					Properties map[string]struct {
						Type        any    `json:"type"`
						Description string `json:"description"`
						Items       *struct {
							Type any `json:"type"`
						} `json:"items"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpWireCall(t, MCPConfig{}, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), &list); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	// A JSON-schema type is a name or a list of names (a pointer field may
	// add "null").
	hasType := func(typ any, want string) bool {
		switch v := typ.(type) {
		case string:
			return v == want
		case []any:
			for _, x := range v {
				if x == want {
					return true
				}
			}
		}
		return false
	}
	for _, tool := range list.Result.Tools {
		if tool.Name != "get" {
			continue
		}
		props := tool.InputSchema.Properties
		fields, ok := props["fields"]
		if !ok || !hasType(fields.Type, "array") || fields.Items == nil || !hasType(fields.Items.Type, "string") {
			t.Errorf("fields = %+v; want an optional list of strings", fields)
		}
		for _, name := range []string{"content", "untrusted", "created_at"} {
			if !strings.Contains(fields.Description, name) {
				t.Errorf("fields does not name %q: %q", name, fields.Description)
			}
		}
		for _, name := range []string{"content_offset", "content_limit"} {
			p, ok := props[name]
			if !ok || !hasType(p.Type, "integer") {
				t.Errorf("%s = %+v; want an optional integer", name, p)
			}
			if !strings.Contains(p.Description, "content_length") || !strings.Contains(p.Description, "content_truncated") {
				t.Errorf("%s does not name the two answer keys: %q", name, p.Description)
			}
		}
		if !reflect.DeepEqual(tool.InputSchema.Required, []string{"id"}) {
			t.Errorf("required = %v, want [id]", tool.InputSchema.Required)
		}
		return
	}
	t.Fatal("tools/list has no get tool")
}

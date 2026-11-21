//go:build integration

// Issue #407 integration probes (real PG18 testcontainer): `fields` and the
// content window of MCP `get` and REST manage `get`. Every MCP call goes through
// the real transport by tool NAME and every REST call through HandleManage with
// a JSON body, so the probes compile against a server without the options and
// fail there at run time.
//
//   - TestGetFields: each field alone, several fields, an unknown name refused
//     with the list of valid names, an empty list, and the framing marks that
//     come along whatever was named.
//   - TestGetContentWindow: windows at the start, in the middle, at the end and
//     past the end, a limit of 0, multi-byte content never cut inside a
//     character, and the refusals of the window's arguments.
//   - TestGetOptionsUnreadable: a block the key cannot read answers exactly as
//     without the options, and a malformed option is refused alike for a
//     readable and an unreadable id.
//   - TestGetOptionsPrefixAndArchived: an ambiguous prefix answers its matches
//     list unchanged, a unique prefix answers the projection under the full id,
//     and include_archived combines with fields.
//   - TestGetOptionsReadRecord: a projected or windowed get books the read
//     record of a full read; a refusal books nothing.
//   - TestGetOptionsLeaveColumnsUnread: the statement each surface runs leaves
//     the content and metadata columns out exactly when the answer does not
//     carry them, on the live and on the archived read.
//   - TestGetWithoutOptionsBytes: a call without the options answers the bytes
//     the server answered before they existed.
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestGet(Fields|ContentWindow|Options|WithoutOptions)' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i407Names is every name `fields` accepts: the keys a get answer can carry,
// in answer order.
var i407Names = []string{
	"id", "category", "tags", "title", "content", "metadata", "scope",
	"sensitivity", "sensitivity_source", "type", "lifecycle_state", "type_source",
	"untrusted", "untrusted_origin", "archived", "created_at", "updated_at",
}

type i407Fixture struct {
	*i201Fixture
	manage *ManageHandler
}

func newI407Fixture(t *testing.T) *i407Fixture {
	t.Helper()
	f := newI201Fixture(t)
	return &i407Fixture{i201Fixture: f, manage: NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)}
}

// storeBlock writes one block over the MCP store tool — the production write
// path — and returns its id.
func (f *i407Fixture) storeBlock(t *testing.T, args map[string]any) string {
	t.Helper()
	a := f.call(t, f.direct, "store", args)
	if a.RPCError != "" || a.IsError {
		t.Fatalf("fixture store %v: %+v", args["title"], a)
	}
	m := i201StoredID.FindStringSubmatch(a.Text)
	if m == nil {
		t.Fatalf("fixture store %v: no id in %q", args["title"], a.Text)
	}
	return m[1]
}

// rest posts one manage get as the given key and answers status and raw body.
func (f *i407Fixture) rest(t *testing.T, ctx context.Context, args map[string]any) (int, []byte) {
	t.Helper()
	body := map[string]any{"action": "get"}
	for k, v := range args {
		body[k] = v
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/manage", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	f.manage.HandleManage(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// i407Block decodes a block object into its keys with compacted raw values, so
// the two surfaces' answers compare key for key.
func i407Block(t *testing.T, where string, raw []byte) map[string]string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: answer is not a JSON object: %v (%s)", where, err, raw)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			t.Fatalf("%s: key %s: %v", where, k, err)
		}
		out[k] = buf.String()
	}
	return out
}

// both reads one block through MCP get and REST manage get with the same
// arguments as the direct key, asserts that both answer it and that both answer
// the same block object, and returns it.
func (f *i407Fixture) both(t *testing.T, args map[string]any) map[string]string {
	t.Helper()
	a := f.call(t, f.direct, "get", args)
	if a.RPCError != "" || a.IsError {
		t.Fatalf("mcp get %v = %+v", args, a)
	}
	viaMCP := i407Block(t, "mcp get", []byte(a.Text))

	code, body := f.rest(t, f.direct, args)
	var resp struct {
		Success bool            `json:"success"`
		Block   json.RawMessage `json:"block"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || code != http.StatusOK || !resp.Success {
		t.Fatalf("manage get %v = %d %s (%v)", args, code, body, err)
	}
	viaREST := i407Block(t, "manage get", resp.Block)

	if !i407Same(viaMCP, viaREST) {
		t.Errorf("the surfaces answer different blocks for %v:\n mcp  %v\n rest %v", args, viaMCP, viaREST)
	}
	return viaMCP
}

// refused calls both surfaces and asserts that both refuse the call as a caller
// error carrying want in its text: a tool error on MCP, a 400 on REST.
func (f *i407Fixture) refused(t *testing.T, ctx context.Context, args map[string]any, want string) (string, string) {
	t.Helper()
	a := f.call(t, ctx, "get", args)
	if a.RPCError != "" || !a.IsError || !strings.Contains(a.Text, want) {
		t.Errorf("mcp get %v = %+v, want a tool error containing %q", args, a, want)
	}
	code, body := f.rest(t, ctx, args)
	var resp map[string]any
	_ = json.Unmarshal(body, &resp)
	msg, _ := resp["error"].(string)
	if code != http.StatusBadRequest || resp["success"] != false || !strings.Contains(msg, want) {
		t.Errorf("manage get %v = %d %s, want a 400 containing %q", args, code, body, want)
	}
	return a.Text, string(body)
}

func i407Same(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func i407Keys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// i407Want is the projection of full onto id plus the given keys, each key only
// where the full answer carries it.
func i407Want(full map[string]string, keys ...string) map[string]string {
	want := map[string]string{"id": full["id"]}
	for _, k := range keys {
		if v, ok := full[k]; ok {
			want[k] = v
		}
	}
	return want
}

func TestGetFields(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	ctx := context.Background()

	id := f.storeBlock(t, map[string]any{
		"category": "test", "title": "i407-fields", "content": "content of i407-fields",
		"tags": []string{"i407", "fields"}, "metadata": map[string]any{"source": "i407"}, "sensitivity": "internal",
	})
	full := f.both(t, map[string]any{"id": id})
	for _, k := range []string{"id", "category", "tags", "title", "content", "metadata", "scope", "sensitivity", "type", "created_at", "updated_at"} {
		if _, ok := full[k]; !ok {
			t.Fatalf("the full answer carries no %s (fixture broken): %v", k, full)
		}
	}

	t.Run("each field alone answers id and that field", func(t *testing.T) {
		for _, name := range i407Names {
			got := f.both(t, map[string]any{"id": id, "fields": []string{name}})
			if want := i407Want(full, name); !i407Same(got, want) {
				t.Errorf("fields [%s] = %v, want %v", name, got, want)
			}
		}
	})

	t.Run("several fields answer id and exactly those", func(t *testing.T) {
		got := f.both(t, map[string]any{"id": id, "fields": []string{"title", "tags", "created_at", "title"}})
		if want := i407Want(full, "title", "tags", "created_at"); !i407Same(got, want) {
			t.Errorf("fields [title tags created_at title] = %v, want %v", i407Keys(got), i407Keys(want))
		}
		got = f.both(t, map[string]any{"id": id, "fields": []string{"metadata", "content"}})
		if want := i407Want(full, "metadata", "content"); !i407Same(got, want) {
			t.Errorf("fields [metadata content] = %v, want %v", got, want)
		}
	})

	t.Run("every field named answers the whole block", func(t *testing.T) {
		if got := f.both(t, map[string]any{"id": id, "fields": i407Names}); !i407Same(got, full) {
			t.Errorf("every field = %v, want the full answer %v", got, full)
		}
	})

	t.Run("an empty list answers the whole block, byte for byte", func(t *testing.T) {
		if got := f.both(t, map[string]any{"id": id, "fields": []string{}}); !i407Same(got, full) {
			t.Errorf("fields [] = %v, want the full answer %v", got, full)
		}
		plain := f.call(t, f.direct, "get", map[string]any{"id": id})
		if a := f.call(t, f.direct, "get", map[string]any{"id": id, "fields": []string{}}); a != plain {
			t.Errorf("mcp fields [] = %q, want the bytes without fields %q", a.Text, plain.Text)
		}
		_, plainBody := f.rest(t, f.direct, map[string]any{"id": id})
		if _, body := f.rest(t, f.direct, map[string]any{"id": id, "fields": []string{}}); !bytes.Equal(body, plainBody) {
			t.Errorf("manage fields [] = %s, want the bytes without fields %s", body, plainBody)
		}
	})

	t.Run("an unknown field is refused with the list of valid names", func(t *testing.T) {
		text, body := f.refused(t, f.direct, map[string]any{"id": id, "fields": []string{"title", "body"}}, `unknown field "body"`)
		for _, name := range i407Names {
			if !strings.Contains(text, name) || !strings.Contains(body, name) {
				t.Errorf("the refusal does not list %q: mcp %q, rest %s", name, text, body)
			}
		}
		for _, notAField := range []string{"guard_status", "workflow_status", "graph_visible", "content_length"} {
			f.refused(t, f.direct, map[string]any{"id": id, "fields": []string{notAField}}, `unknown field "`+notAField+`"`)
		}
	})

	t.Run("the framing marks come along whatever was named", func(t *testing.T) {
		// A block of a type marked untrusted, through the production store
		// function, and one re-typed out of such a type through the MCP update
		// tool, so it carries the untrusted_origin record.
		untrusted, err := store.UpsertBlock(ctx, f.pool, "test", "i407-untrusted", "foreign text", nil, nil, "private", true,
			store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, "checkpoint")
		if err != nil {
			t.Fatalf("upsert untrusted: %v", err)
		}
		retyped, err := store.UpsertBlock(ctx, f.pool, "test", "i407-retyped", "agent text", nil, nil, "private", true,
			store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, "checkpoint")
		if err != nil {
			t.Fatalf("upsert retyped: %v", err)
		}
		if a := f.call(t, f.direct, "update", map[string]any{"id": retyped.ID, "type": "knowledge"}); a.RPCError != "" || a.IsError {
			t.Fatalf("re-type: %+v", a)
		}
		archived := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-archived", "content": "archived text", "sensitivity": "internal"})
		if _, err := store.DeleteBlock(ctx, f.pool, archived, []string{"private"}); err != nil {
			t.Fatalf("archive: %v", err)
		}

		for _, c := range []struct {
			name, id, mark string
			extra          map[string]any
		}{
			{"untrusted", untrusted.ID, "untrusted", nil},
			{"untrusted_origin", retyped.ID, "untrusted_origin", nil},
			{"archived", archived, "archived", map[string]any{"include_archived": true}},
		} {
			whole := map[string]any{"id": c.id}
			for k, v := range c.extra {
				whole[k] = v
			}
			fullOf := f.both(t, whole)
			if _, ok := fullOf[c.mark]; !ok {
				t.Fatalf("%s: the full answer carries no %s (fixture broken): %v", c.name, c.mark, fullOf)
			}
			args := map[string]any{"id": c.id, "fields": []string{"title"}}
			for k, v := range c.extra {
				args[k] = v
			}
			got := f.both(t, args)
			if want := i407Want(fullOf, "title", c.mark); !i407Same(got, want) {
				t.Errorf("%s: fields [title] = %v, want %v", c.name, got, want)
			}
		}
	})
}

// i407Content holds one- to four-byte characters: 18 code points, 32 bytes.
const i407Content = "aä€😀b" + "öööööööööö" + "end"

func TestGetContentWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	id := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-window", "content": i407Content, "sensitivity": "internal"})
	full := f.both(t, map[string]any{"id": id})
	length := utf8.RuneCountInString(i407Content)

	for _, c := range []struct {
		name      string
		offset    any // nil = not sent
		limit     any
		want      string
		truncated bool
	}{
		{"at the start", nil, 3, "aä€", true},
		{"at the start with offset 0", 0, 5, "aä€😀b", true},
		{"in the middle", 2, 3, "€😀b", true},
		{"a four-byte character alone", 3, 1, "😀", true},
		{"at the end", 15, 10, "end", false},
		{"exactly to the end", 15, 3, "end", false},
		{"offset only reads to the end", 15, nil, "end", false},
		{"limit only reads from the start", nil, 100, i407Content, false},
		{"past the end", 30, 5, "", false},
		{"at the end with limit 0", 18, 0, "", false},
		{"limit 0", 0, 0, "", true},
		{"limit 0 in the middle", 4, 0, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := map[string]any{"id": id}
			if c.offset != nil {
				args["content_offset"] = c.offset
			}
			if c.limit != nil {
				args["content_limit"] = c.limit
			}
			got := f.both(t, args)
			var content string
			if err := json.Unmarshal([]byte(got["content"]), &content); err != nil {
				t.Fatalf("content = %s: %v", got["content"], err)
			}
			if !utf8.ValidString(content) || content != c.want {
				t.Errorf("content = %q (valid UTF-8 %t), want %q", content, utf8.ValidString(content), c.want)
			}
			if got["content_length"] != "18" || length != 18 {
				t.Errorf("content_length = %s, want 18 code points", got["content_length"])
			}
			wantTrunc := "false"
			if c.truncated {
				wantTrunc = "true"
			}
			if got["content_truncated"] != wantTrunc {
				t.Errorf("content_truncated = %s, want %s", got["content_truncated"], wantTrunc)
			}
			// Without fields the window answers the whole block around it.
			for k, v := range full {
				if k != "content" && got[k] != v {
					t.Errorf("key %s = %s, want the full answer's %s", k, got[k], v)
				}
			}
			if len(got) != len(full)+2 {
				t.Errorf("keys = %v, want the full answer's plus content_length and content_truncated", i407Keys(got))
			}
		})
	}

	t.Run("the window combines with fields naming content", func(t *testing.T) {
		got := f.both(t, map[string]any{"id": id, "fields": []string{"content", "title"}, "content_offset": 15})
		want := map[string]string{"id": full["id"], "title": full["title"], "content": `"end"`, "content_length": "18", "content_truncated": "false"}
		if !i407Same(got, want) {
			t.Errorf("fields [content title] with a window = %v, want %v", got, want)
		}
	})

	t.Run("an empty fields list beside the window is the whole block", func(t *testing.T) {
		got := f.both(t, map[string]any{"id": id, "fields": []string{}, "content_offset": 15})
		if got["content"] != `"end"` || got["title"] != full["title"] || len(got) != len(full)+2 {
			t.Errorf("fields [] with a window = %v", got)
		}
	})

	t.Run("the window without content among fields is refused", func(t *testing.T) {
		f.refused(t, f.direct, map[string]any{"id": id, "fields": []string{"title"}, "content_limit": 10}, "content")
		f.refused(t, f.direct, map[string]any{"id": id, "fields": []string{"title"}, "content_offset": 0}, "content")
	})

	t.Run("negative offsets and limits are refused", func(t *testing.T) {
		f.refused(t, f.direct, map[string]any{"id": id, "content_offset": -1}, "content_offset")
		f.refused(t, f.direct, map[string]any{"id": id, "content_limit": -1}, "content_limit")
	})
}

func TestGetOptionsUnreadable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	ctx := context.Background()

	readable := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-readable", "content": "readable", "sensitivity": "internal"})
	foreign := f.store(t, f.other, "i407-foreign")
	archived := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-unread-archived", "content": "archived", "sensitivity": "internal"})
	if _, err := store.DeleteBlock(ctx, f.pool, archived, []string{"private"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	options := map[string]any{"fields": []string{"title", "content"}, "content_offset": 1, "content_limit": 2}

	t.Run("a block the key cannot read answers exactly as without the options", func(t *testing.T) {
		for name, id := range map[string]string{"foreign": foreign, "unknown": i201Unknown, "archived": archived} {
			args := map[string]any{"id": id}
			plainMCP := f.call(t, f.direct, "get", args)
			plainCode, plainBody := f.rest(t, f.direct, args)
			if !plainMCP.IsError || plainCode != http.StatusOK || !strings.Contains(string(plainBody), "Block not found") {
				t.Fatalf("%s: the plain read is not a miss (fixture broken): %+v / %d %s", name, plainMCP, plainCode, plainBody)
			}
			for k, v := range options {
				args[k] = v
			}
			if a := f.call(t, f.direct, "get", args); a != plainMCP {
				t.Errorf("%s: mcp get with options = %+v, want %+v", name, a, plainMCP)
			}
			if code, body := f.rest(t, f.direct, args); code != plainCode || !bytes.Equal(body, plainBody) {
				t.Errorf("%s: manage get with options = %d %s, want %d %s", name, code, body, plainCode, plainBody)
			}
		}
	})

	t.Run("a malformed option is refused alike for a readable and an unreadable id", func(t *testing.T) {
		for _, bad := range []map[string]any{
			{"fields": []string{"nope"}},
			{"content_offset": -3},
			{"fields": []string{"title"}, "content_limit": 1},
		} {
			var texts, bodies []string
			for _, id := range []string{readable, foreign, i201Unknown, archived} {
				args := map[string]any{"id": id}
				for k, v := range bad {
					args[k] = v
				}
				text, body := f.refused(t, f.direct, args, "")
				texts = append(texts, text)
				bodies = append(bodies, body)
			}
			for i := 1; i < len(texts); i++ {
				if texts[i] != texts[0] || bodies[i] != bodies[0] {
					t.Errorf("%v: the refusal differs by id: mcp %q, rest %q", bad, texts, bodies)
					break
				}
			}
		}
	})

	t.Run("options of the wrong shape are refused on both surfaces", func(t *testing.T) {
		for _, bad := range []map[string]any{
			{"fields": "title"},
			{"fields": []any{"title", 3}},
			{"content_offset": "3"},
			{"content_limit": 1.5},
		} {
			args := map[string]any{"id": readable}
			for k, v := range bad {
				args[k] = v
			}
			code, body := f.rest(t, f.direct, args)
			if code != http.StatusBadRequest || !strings.Contains(string(body), `"success":false`) {
				t.Errorf("manage get %v = %d %s, want a 400", bad, code, body)
			}
			// MCP: the SDK's schema check refuses the call before the tool runs.
			if a := f.call(t, f.direct, "get", args); a.RPCError == "" && !a.IsError {
				t.Errorf("mcp get %v = %+v, want a refusal", bad, a)
			}
		}
	})

	t.Run("REST reads null as an option not sent", func(t *testing.T) {
		_, plain := f.rest(t, f.direct, map[string]any{"id": readable})
		code, body := f.rest(t, f.direct, map[string]any{"id": readable, "fields": nil, "content_offset": nil, "content_limit": nil})
		if code != http.StatusOK || !bytes.Equal(body, plain) {
			t.Errorf("manage get with null options = %d %s, want the plain answer %s", code, body, plain)
		}
	})

	t.Run("a block visible through a grant answers the projection", func(t *testing.T) {
		granted := f.store(t, f.other, "i407-granted")
		if _, err := store.CreateBlockGrant(ctx, f.pool, granted, store.DefaultTenantID, ""); err != nil {
			t.Fatalf("grant: %v", err)
		}
		full := f.both(t, map[string]any{"id": granted})
		got := f.both(t, map[string]any{"id": granted, "fields": []string{"title", "scope"}})
		if want := i407Want(full, "title", "scope"); !i407Same(got, want) || got["scope"] != `"i201-b"` {
			t.Errorf("granted block with fields = %v, want %v", got, want)
		}
	})

	t.Run("another manage action decodes the new keys as before", func(t *testing.T) {
		post := func(extra map[string]any) (int, []byte) {
			body := map[string]any{"action": "list-categories"}
			for k, v := range extra {
				body[k] = v
			}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequestWithContext(f.direct, http.MethodPost, "/api/manage", bytes.NewReader(raw))
			rec := httptest.NewRecorder()
			f.manage.HandleManage(rec, req)
			return rec.Code, rec.Body.Bytes()
		}
		plainCode, plainBody := post(nil)
		code, body := post(map[string]any{"fields": "anything", "content_offset": "x", "content_limit": map[string]any{}})
		if plainCode != http.StatusOK || code != plainCode || !bytes.Equal(body, plainBody) {
			t.Errorf("list-categories with foreign keys = %d %s, want %d %s", code, body, plainCode, plainBody)
		}
	})
}

func TestGetOptionsPrefixAndArchived(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	ctx := context.Background()

	// Two blocks whose ids share at least store.MinIDPrefixLen characters:
	// UUIDv7 ids written together share their time prefix.
	var a, b, prefix string
	for i := 0; i < 4 && prefix == ""; i++ {
		x := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-prefix-a" + strings.Repeat("x", i), "content": "a", "sensitivity": "internal"})
		y := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-prefix-b" + strings.Repeat("x", i), "content": "b", "sensitivity": "internal"})
		n := 0
		for n < len(x) && x[n] == y[n] {
			n++
		}
		if n >= store.MinIDPrefixLen {
			a, b, prefix = x, y, x[:store.MinIDPrefixLen]
		}
	}
	if prefix == "" {
		t.Fatal("no two fixture ids share a prefix of store.MinIDPrefixLen")
	}

	t.Run("an ambiguous prefix answers its matches unchanged", func(t *testing.T) {
		plain := f.call(t, f.direct, "get", map[string]any{"id": prefix})
		if !plain.IsError || !strings.Contains(plain.Text, a) || !strings.Contains(plain.Text, b) {
			t.Fatalf("plain prefix get = %+v, want the matches list", plain)
		}
		plainCode, plainBody := f.rest(t, f.direct, map[string]any{"id": prefix})
		opts := map[string]any{"id": prefix, "fields": []string{"title", "content"}, "content_limit": 1}
		if got := f.call(t, f.direct, "get", opts); got != plain {
			t.Errorf("mcp prefix get with options = %+v, want %+v", got, plain)
		}
		if code, body := f.rest(t, f.direct, opts); code != plainCode || !bytes.Equal(body, plainBody) {
			t.Errorf("manage prefix get with options = %d %s, want %d %s", code, body, plainCode, plainBody)
		}
	})

	t.Run("a unique prefix answers the projection under the full id", func(t *testing.T) {
		full := f.both(t, map[string]any{"id": a})
		got := f.both(t, map[string]any{"id": a[:20], "fields": []string{"title"}})
		if want := i407Want(full, "title"); !i407Same(got, want) || got["id"] != `"`+a+`"` {
			t.Errorf("unique prefix with fields = %v, want %v", got, want)
		}
	})

	t.Run("include_archived combines with fields and the window", func(t *testing.T) {
		own := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-archived-read", "content": "archived body", "sensitivity": "internal"})
		if _, err := store.DeleteBlock(ctx, f.pool, own, []string{"private"}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		got := f.both(t, map[string]any{"id": own, "include_archived": true, "fields": []string{"content"}, "content_offset": 9})
		want := map[string]string{"id": `"` + own + `"`, "content": `"body"`, "archived": "true", "content_length": "13", "content_truncated": "false"}
		if !i407Same(got, want) {
			t.Errorf("archived read with options = %v, want %v", got, want)
		}
		code, body := f.rest(t, f.direct, map[string]any{"id": own, "include_archived": true, "fields": []string{"title"}})
		var resp map[string]any
		_ = json.Unmarshal(body, &resp)
		if code != http.StatusOK || resp["graph_visible"] != false {
			t.Errorf("manage get archived with fields = %d %s, want graph_visible false", code, body)
		}
		// Without include_archived the archived block stays a miss.
		if a := f.call(t, f.direct, "get", map[string]any{"id": own, "fields": []string{"title"}}); !a.IsError || a.Text != "block not found" {
			t.Errorf("archived block with fields but without include_archived = %+v", a)
		}
	})

	t.Run("REST graph_visible stays beside a projection that leaves out the type", func(t *testing.T) {
		_, plain := f.rest(t, f.direct, map[string]any{"id": a})
		_, projected := f.rest(t, f.direct, map[string]any{"id": a, "fields": []string{"title"}})
		var p, q map[string]any
		_ = json.Unmarshal(plain, &p)
		_ = json.Unmarshal(projected, &q)
		if p["graph_visible"] == nil || q["graph_visible"] != p["graph_visible"] || q["action"] != "get" || q["success"] != true {
			t.Errorf("projected envelope = %s, plain %s", projected, plain)
		}
	})
}

func TestGetOptionsReadRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	id := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-record", "content": "recorded", "sensitivity": "internal"})

	// Refusals first: they must book nothing.
	f.refused(t, f.direct, map[string]any{"id": id, "fields": []string{"nope"}}, "nope")
	f.refused(t, f.direct, map[string]any{"id": id, "content_offset": -1}, "content_offset")

	f.both(t, map[string]any{"id": id, "fields": []string{"title"}})
	f.both(t, map[string]any{"id": id, "content_limit": 0})

	for _, action := range []string{store.ActionMCPGet, store.ActionManageGet} {
		rows := i345Rows(t, f.pool, action)
		if len(rows) != 2 {
			t.Fatalf("%s booked %d rows (%v), want one per answered read (2)", action, len(rows), i345BlockIDs(rows))
		}
		for _, r := range rows {
			if i345Str(r.BlockID) != id || r.Meta["type"] == nil || r.Meta["block_created_at"] == nil {
				t.Errorf("%s row = block %s meta %v, want the full read's row for %s", action, i345Str(r.BlockID), r.Meta, id)
			}
		}
	}
}

// i407Tracer records the SQL text of every statement a pool runs.
type i407Tracer struct {
	mu  sync.Mutex
	sql []string
}

func (tr *i407Tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	tr.sql = append(tr.sql, d.SQL)
	tr.mu.Unlock()
	return ctx
}

func (tr *i407Tracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// reads takes the by-id block reads recorded since the last call: the
// statements with the select list of store.readBlockByID.
func (tr *i407Tracer) reads() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var out []string
	for _, s := range tr.sql {
		if strings.Contains(s, "sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at") &&
			strings.Contains(s, "WHERE id = $1") {
			out = append(out, s)
		}
	}
	tr.sql = nil
	return out
}

func TestGetOptionsLeaveColumnsUnread(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	ctx := context.Background()

	// The same database through a second pool that records every statement;
	// both surfaces read through it.
	tr := &i407Tracer{}
	cfg := f.pool.Config()
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(func() {
		_ = store.SettleBlockReads(context.Background(), traced)
		traced.Close()
	})
	base := *f.i201Fixture
	base.pool = traced
	base.cfg.Pool = traced
	base.mcp = NewMCPHandler(base.cfg)
	g := &i407Fixture{i201Fixture: &base, manage: NewManageHandler(traced, nil, nil, nil, nil, nil, nil, base.cfg.Blocktypes)}

	live := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-columns", "content": "a long body",
		"metadata": map[string]any{"source": "i407"}, "sensitivity": "internal"})
	archived := f.storeBlock(t, map[string]any{"category": "test", "title": "i407-columns-archived", "content": "an archived body",
		"metadata": map[string]any{"source": "i407"}, "sensitivity": "internal"})
	if _, err := store.DeleteBlock(ctx, f.pool, archived, []string{"private"}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	for _, c := range []struct {
		name              string
		args              map[string]any
		content, metadata bool // read from the database
		statements        int  // by-id reads over both surfaces
	}{
		{"no options", map[string]any{"id": live}, true, true, 2},
		{"title", map[string]any{"id": live, "fields": []string{"title"}}, false, false, 2},
		{"content", map[string]any{"id": live, "fields": []string{"content", "type"}}, true, false, 2},
		{"metadata", map[string]any{"id": live, "fields": []string{"metadata"}}, false, true, 2},
		{"a window over the whole block", map[string]any{"id": live, "content_limit": 3}, true, true, 2},
		{"a window with fields", map[string]any{"id": live, "fields": []string{"content"}, "content_offset": 2}, true, false, 2},
		// The live read misses, the archived read answers: two per surface.
		{"archived, title", map[string]any{"id": archived, "include_archived": true, "fields": []string{"title"}}, false, false, 4},
		{"archived, metadata", map[string]any{"id": archived, "include_archived": true, "fields": []string{"metadata"}}, false, true, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr.reads()
			g.both(t, c.args)
			reads := tr.reads()
			if len(reads) != c.statements {
				t.Fatalf("%d by-id reads recorded, want %d: %q", len(reads), c.statements, reads)
			}
			for _, s := range reads {
				readsContent := strings.Contains(s, "title, content, ")
				skipsContent := strings.Contains(s, "''::text AS content")
				readsMetadata := strings.Contains(s, "context_blocks.metadata - 'guard_matched_id'")
				skipsMetadata := strings.Contains(s, "NULL::jsonb AS metadata")
				if readsContent != c.content || skipsContent == c.content || readsMetadata != c.metadata || skipsMetadata == c.metadata {
					t.Errorf("statement reads content %t (skips %t), metadata %t (skips %t); want content %t, metadata %t:\n%s",
						readsContent, skipsContent, readsMetadata, skipsMetadata, c.content, c.metadata, s)
				}
			}
		})
	}
}

// i407Golden is the answer of a get without options as the server gave it
// before issue #407, with the id and the two timestamps replaced.
const i407GoldenMCP = `{
  "id": "<id>",
  "category": "test",
  "tags": [
    "i407",
    "golden"
  ],
  "title": "i407-golden",
  "content": "golden \u003ccontent\u003e \u0026 ä😀",
  "metadata": {
    "nested": {
      "n": 1
    },
    "source": "i407"
  },
  "scope": "private",
  "sensitivity": "internal",
  "sensitivity_source": "manual",
  "type": "knowledge",
  "lifecycle_state": "knowledge",
  "type_source": "auto",
  "created_at": "<ts>",
  "updated_at": "<ts>"
}`

const i407GoldenREST = `{"action":"get","block":{"id":"<id>","category":"test","tags":["i407","golden"],"title":"i407-golden","content":"golden \u003ccontent\u003e \u0026 ä😀","metadata":{"nested":{"n":1},"source":"i407"},"scope":"private","sensitivity":"internal","sensitivity_source":"manual","type":"knowledge","lifecycle_state":"knowledge","type_source":"auto","created_at":"<ts>","updated_at":"<ts>"},"graph_visible":true,"success":true}
`

var i407Timestamp = regexp.MustCompile(`"20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:.]+(Z|[+-][0-9:]+)"`)

func TestGetWithoutOptionsBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI407Fixture(t)
	id := f.storeBlock(t, map[string]any{
		"category": "test", "title": "i407-golden", "content": "golden <content> & ä😀",
		"tags": []string{"i407", "golden"}, "metadata": map[string]any{"source": "i407", "nested": map[string]any{"n": 1}},
		"sensitivity": "internal",
	})
	normalize := func(s string) string {
		return i407Timestamp.ReplaceAllString(strings.ReplaceAll(s, id, "<id>"), `"<ts>"`)
	}

	a := f.call(t, f.direct, "get", map[string]any{"id": id})
	if got := normalize(a.Text); a.IsError || got != i407GoldenMCP {
		t.Errorf("mcp get without options:\n%s\nwant\n%s", got, i407GoldenMCP)
	}
	code, body := f.rest(t, f.direct, map[string]any{"id": id})
	if got := normalize(string(body)); code != http.StatusOK || got != i407GoldenREST {
		t.Errorf("manage get without options (%d):\n%s\nwant\n%s", code, got, i407GoldenREST)
	}
}

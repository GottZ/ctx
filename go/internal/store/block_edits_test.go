// Issue #406, the text side of an update's edits: ApplyContentEdits and
// CheckUpdatePatch without a database. The store integration test
// (update_edits_integration_test.go) drives the same rules through
// UpdateBlock under the row lock; this file pins the matching rules
// themselves, where every case is one call.
package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func editOf(old, repl string) ContentEdit { return ContentEdit{Old: old, New: &repl} }

func editAllOf(old, repl string) ContentEdit {
	return ContentEdit{Old: old, New: &repl, ReplaceAll: true}
}

func TestApplyContentEdits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		edits   []ContentEdit
		want    string
	}{
		{"a unique match is replaced", "alpha beta gamma", []ContentEdit{editOf("beta", "BETA")}, "alpha BETA gamma"},
		{"new may be empty: the text is deleted", "alpha beta gamma", []ContentEdit{editOf(" beta", "")}, "alpha gamma"},
		{"replace_all replaces every occurrence", "a-b-a-b", []ContentEdit{editAllOf("a", "x")}, "x-b-x-b"},
		{"replace_all with one occurrence", "a-b", []ContentEdit{editAllOf("a", "x")}, "x-b"},
		{"edits apply in order: the second matches the first one's text", "one", []ContentEdit{editOf("one", "two"), editOf("two", "three")}, "three"},
		{"an edit whose new equals old asserts its match and changes nothing", "keep me", []ContentEdit{editOf("keep", "keep")}, "keep me"},
		{"matching is bytes of UTF-8, no normalisation: the composed a-umlaut matches", "Gr\xc3\xa4tsche", []ContentEdit{editOf("\xc3\xa4", "ae")}, "Graetsche"},
		{"multi-line old", "a\nb\nc\n", []ContentEdit{editOf("b\nc", "B\nC")}, "a\nB\nC\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyContentEdits(tc.content, tc.edits)
			if err != nil {
				t.Fatalf("ApplyContentEdits: %v", err)
			}
			if got != tc.want {
				t.Fatalf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestApplyContentEditsRefusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		content    string
		edits      []ContentEdit
		index      int
		matches    int
		replaceAll bool
	}{
		{"no match", "alpha", []ContentEdit{editOf("beta", "x")}, 0, 0, false},
		{"two matches without replace_all", "ab ab", []ContentEdit{editOf("ab", "x")}, 0, 2, false},
		{"an overlapping second occurrence makes the match ambiguous", "aaa", []ContentEdit{editOf("aa", "b")}, 0, 2, false},
		{"replace_all with no match", "alpha", []ContentEdit{editAllOf("beta", "x")}, 0, 0, true},
		{"the failing edit is named by its index, after the edits before it applied", "one two", []ContentEdit{editOf("one", "two"), editOf("two", "x")}, 1, 2, false},
		{"a decomposed a+combining diaeresis does not match the composed form", "Gr\xc3\xa4tsche", []ContentEdit{editOf("a\xcc\x88", "ae")}, 0, 0, false},
		{"matching is case-sensitive", "Alpha", []ContentEdit{editOf("alpha", "x")}, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyContentEdits(tc.content, tc.edits)
			var ee *ContentEditError
			if !errors.As(err, &ee) {
				t.Fatalf("err = %v, want *ContentEditError", err)
			}
			if ee.Index != tc.index || ee.Matches != tc.matches || ee.ReplaceAll != tc.replaceAll {
				t.Fatalf("error = %+v, want index %d, matches %d, replace_all %v", *ee, tc.index, tc.matches, tc.replaceAll)
			}
			msg := ee.Error()
			if !strings.Contains(msg, "edits[") || !strings.Contains(msg, "matches") {
				t.Fatalf("message %q names neither the edit nor its matches", msg)
			}
		})
	}
}

// The cap holds after every edit, before the grown text is built: a
// replace_all that would multiply the content is refused by arithmetic, not
// after allocating the result.
func TestApplyContentEditsSizeCap(t *testing.T) {
	big := strings.Repeat("x", MaxBlockContentBytes)
	for _, tc := range []struct {
		name    string
		content string
		edits   []ContentEdit
		index   int
	}{
		{"one edit past the cap", strings.Repeat("x", MaxBlockContentBytes-1) + "a", []ContentEdit{editOf("a", "bb")}, 0},
		{"replace_all multiplying the content", strings.Repeat("a", 1000), []ContentEdit{editAllOf("a", big)}, 0},
		{"a later edit past the cap", "a", []ContentEdit{editOf("a", "b"), editOf("b", big+"c")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ApplyContentEdits(tc.content, tc.edits)
			if !errors.Is(err, ErrEditedContentTooLarge) {
				t.Fatalf("err = %v, want ErrEditedContentTooLarge", err)
			}
			if want := "edits[" + string(rune('0'+tc.index)) + "]"; !strings.Contains(err.Error(), want) {
				t.Fatalf("message %q does not name %s", err.Error(), want)
			}
		})
	}
	// Exactly at the cap is within it.
	if _, err := ApplyContentEdits(strings.Repeat("x", MaxBlockContentBytes-2)+"a", []ContentEdit{editOf("a", "bb")}); err != nil {
		t.Fatalf("an edit that lands exactly on the cap: %v", err)
	}
	if _, err := ApplyContentEdits(strings.Repeat("a", 10)+"b", []ContentEdit{editAllOf("b", strings.Repeat("c", MaxBlockContentBytes-10))}); err != nil {
		t.Fatalf("a replace_all that lands exactly on the cap: %v", err)
	}
}

func TestCheckUpdatePatch(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		name string
		data UpdateBlockData
		want string // substring of the refusal, "" = admissible
	}{
		{"content alone", UpdateBlockData{Content: s("x")}, ""},
		{"edits alone", UpdateBlockData{Edits: []ContentEdit{editOf("a", "b")}}, ""},
		{"metadata_merge alone", UpdateBlockData{MetadataMerge: map[string]any{"k": nil}}, ""},
		{"an empty merge changes no key and is admissible", UpdateBlockData{MetadataMerge: map[string]any{}}, ""},
		{"edits with metadata_merge", UpdateBlockData{Edits: []ContentEdit{editOf("a", "b")}, MetadataMerge: map[string]any{"k": 1}}, ""},
		{"content and edits", UpdateBlockData{Content: s("x"), Edits: []ContentEdit{editOf("a", "b")}}, "content and edits"},
		{"metadata and metadata_merge", UpdateBlockData{Metadata: map[string]any{}, MetadataMerge: map[string]any{"k": 1}}, "metadata and metadata_merge"},
		{"an empty edit list", UpdateBlockData{Edits: []ContentEdit{}}, "at least one edit"},
		{"an empty old", UpdateBlockData{Edits: []ContentEdit{editOf("a", "b"), editOf("", "b")}}, "edits[1]: old is empty"},
		{"an empty old with replace_all", UpdateBlockData{Edits: []ContentEdit{editAllOf("", "b")}}, "edits[0]: old is empty"},
		{"a missing new", UpdateBlockData{Edits: []ContentEdit{{Old: "a"}}}, "edits[0]: new is missing"},
		{"edits at the cap", UpdateBlockData{Edits: editsOfLen(MaxContentEdits)}, ""},
		{"edits past the cap", UpdateBlockData{Edits: editsOfLen(MaxContentEdits + 1)}, "exceed the cap of 100"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckUpdatePatch(tc.data)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CheckUpdatePatch = %v, want admissible", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckUpdatePatch = %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

// A REST client builds the edit list as JSON: a missing "new" decodes to nil
// and is refused instead of deleting the text, "new": "" deletes it.
func TestContentEditJSONKeepsAMissingNewApart(t *testing.T) {
	var d UpdateBlockData
	if err := json.Unmarshal([]byte(`{"edits":[{"old":"a"},{"old":"b","new":""},{"old":"c","new":"C","replace_all":true}]}`), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Edits) != 3 {
		t.Fatalf("edits = %+v", d.Edits)
	}
	if d.Edits[0].New != nil {
		t.Fatalf("a missing new decoded to %q, want nil", *d.Edits[0].New)
	}
	if d.Edits[1].New == nil || *d.Edits[1].New != "" {
		t.Fatalf("\"new\": \"\" decoded to %v, want the empty string", d.Edits[1].New)
	}
	if !d.Edits[2].ReplaceAll {
		t.Fatal("replace_all did not decode")
	}
	if err := CheckUpdatePatch(d); err == nil || !strings.Contains(err.Error(), "edits[0]: new is missing") {
		t.Fatalf("CheckUpdatePatch = %v, want the missing new of edits[0] refused", err)
	}
}

func editsOfLen(n int) []ContentEdit {
	out := make([]ContentEdit, n)
	for i := range out {
		out[i] = editOf("x", "x")
	}
	return out
}

// BenchmarkApplyContentEditsAtTheCaps is the cost MaxContentEdits bounds: a
// full edit list against a content at the size cap, the work an update does
// while it holds the block's row lock.
func BenchmarkApplyContentEditsAtTheCaps(b *testing.B) {
	content := strings.Repeat("abcdefghij", MaxBlockContentBytes/10-2) + "unique-tail" // 51 191 bytes
	edits := make([]ContentEdit, MaxContentEdits)
	for i := range edits {
		edits[i] = editOf("unique-tail", "unique-tail")
	}
	for b.Loop() {
		if _, err := ApplyContentEdits(content, edits); err != nil {
			b.Fatal(err)
		}
	}
}

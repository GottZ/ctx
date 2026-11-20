// Issue #407: `fields` and the content window of the two get surfaces — MCP
// `get` and REST manage `get`. A reader names the keys it needs and the part of
// the content it reads; the store leaves the content and metadata columns
// unread when the answer does not carry them. Both surfaces parse the options
// with newGetOptions and answer the block with getOptions.answer, so they
// accept, refuse and answer alike. Without the options nothing changes: the
// read is the full read and the answer is the block as before.
package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/store"
)

// getFieldNames is every name `fields` accepts: the keys a get answer can carry,
// in answer order. guard_status and workflow_status are keys of store.Block
// that the by-id read never fills, so they are not among them; the getInput
// field description lists the same names (get_fields_test.go pins both).
var getFieldNames = []string{
	"id", "category", "tags", "title", "content", "metadata", "scope",
	"sensitivity", "sensitivity_source", "type", "lifecycle_state", "type_source",
	"untrusted", "untrusted_origin", "archived", "created_at", "updated_at",
}

// getFramingKeys are statements about the block rather than data of it: the
// V-11 framing (untrusted, and the untrusted_origin record of issue #280) and
// the mark of an archived read (#201). A projection carries each of them
// whenever it applies, named or not, so that taking part of a block never
// drops the framing the whole block carries. Each is absent when it does not
// apply (omitempty), as on the full answer.
var getFramingKeys = map[string]bool{"untrusted": true, "untrusted_origin": true, "archived": true}

// getWindowNeedsContentMsg refuses a content window beside a `fields` list
// that does not name content: the window cuts the content, which that answer
// would not carry.
const getWindowNeedsContentMsg = `content_offset and content_limit cut the content: name "content" in fields or leave fields out`

// getOptions is what a get asks for beyond the id. A nil *getOptions is a call
// without the options: the full read, answered as before.
type getOptions struct {
	fields map[string]bool // the named keys; nil = every key
	window bool            // content_offset or content_limit given
	offset int             // first code point of the window
	limit  int             // code points in the window; < 0 = to the end
}

// newGetOptions checks the options of one get before any lookup, so a refusal
// depends on the arguments only and never on what is stored. An empty fields
// list is no list: the whole block (["id"] asks for the id alone). It answers
// nil when no option is given.
func newGetOptions(fields []string, offset, limit *int) (*getOptions, error) {
	if len(fields) == 0 && offset == nil && limit == nil {
		return nil, nil
	}
	o := &getOptions{limit: -1}
	if len(fields) > 0 {
		o.fields = make(map[string]bool, len(fields))
		for _, name := range fields {
			if !slices.Contains(getFieldNames, name) {
				return nil, fmt.Errorf("unknown field %q in fields; valid fields: %s", name, strings.Join(getFieldNames, ", "))
			}
			o.fields[name] = true
		}
	}
	if offset != nil {
		if *offset < 0 {
			return nil, errors.New("content_offset must be 0 or more")
		}
		o.window, o.offset = true, *offset
	}
	if limit != nil {
		if *limit < 0 {
			return nil, errors.New("content_limit must be 0 or more")
		}
		o.window, o.limit = true, *limit
	}
	if o.window && o.fields != nil && !o.fields["content"] {
		return nil, errors.New(getWindowNeedsContentMsg)
	}
	return o, nil
}

// decodeGetOptions reads the options of a manage get. manageRequest keeps the
// three keys raw, like `after`, so only get decodes them: a body of another
// action decodes as it did before the keys existed, whatever it carries there.
func decodeGetOptions(req manageRequest) (*getOptions, error) {
	var fields []string
	var offset, limit *int
	for _, opt := range []struct {
		raw json.RawMessage
		dst any
		msg string
	}{
		{req.Fields, &fields, "fields must be a list of field names"},
		{req.ContentOffset, &offset, "content_offset must be a whole number"},
		{req.ContentLimit, &limit, "content_limit must be a whole number"},
	} {
		if len(opt.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(opt.raw, opt.dst); err != nil {
			return nil, errors.New(opt.msg)
		}
	}
	return newGetOptions(fields, offset, limit)
}

// omit is the store read's column selection: content and metadata stay unread
// when the answer does not carry them. A window always reads the content —
// content_length counts the whole of it.
func (o *getOptions) omit() store.BlockReadOmit {
	if o == nil || o.fields == nil {
		return store.BlockReadOmit{}
	}
	return store.BlockReadOmit{Content: !o.fields["content"], Metadata: !o.fields["metadata"]}
}

// answer is the block object a get answers. Without options it is the block
// itself, so the bytes stay those of the full read. With them it is the id,
// the named keys and the framing keys that apply, each encoded exactly as on
// the full answer and in its order, plus content_length and content_truncated
// right after content when a window was given.
func (o *getOptions) answer(b *store.Block) (any, error) {
	if o == nil {
		return b, nil
	}
	cut := *b
	var length int
	var truncated bool
	if o.window {
		cut.Content, length, truncated = contentWindow(b.Content, o.offset, o.limit)
	}
	raw, err := json.Marshal(&cut)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	out := make(getAnswer, 0, len(all)+2)
	for _, key := range getFieldNames {
		value, ok := all[key]
		if !ok || !o.carries(key) {
			continue
		}
		out = append(out, getAnswerKey{key, value})
		if key == "content" && o.window {
			out = append(out,
				getAnswerKey{"content_length", json.RawMessage(strconv.Itoa(length))},
				getAnswerKey{"content_truncated", json.RawMessage(strconv.FormatBool(truncated))})
		}
	}
	return out, nil
}

// carries reports whether the answer carries key: every key without a fields
// list, otherwise the id, the named keys and the framing keys.
func (o *getOptions) carries(key string) bool {
	return o.fields == nil || o.fields[key] || key == "id" || getFramingKeys[key]
}

// getAnswer is a projected block: a JSON object whose keys keep the order they
// were appended in — the order of the full answer — where a map would sort
// them.
type getAnswer []getAnswerKey

type getAnswerKey struct {
	name  string
	value json.RawMessage
}

// MarshalJSON writes the keys in order. Names are code-owned and values are
// valid JSON from json.Marshal, so the object is valid JSON.
func (a getAnswer) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range a {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(strconv.Quote(k.name))
		buf.WriteByte(':')
		buf.Write(k.value)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// contentWindow cuts content to the window that starts at code point offset
// and holds at most limit code points (limit < 0: to the end), so a cut never
// falls inside a character. It answers the window, the content's length in
// code points, and whether the content goes on after the window — when it
// does, the window was full and the next one starts at offset + limit. An
// offset at or past the end answers an empty window that is not truncated.
func contentWindow(content string, offset, limit int) (window string, length int, truncated bool) {
	length = utf8.RuneCountInString(content)
	if offset >= length {
		return "", length, false
	}
	start := runeByteIndex(content, offset)
	if limit < 0 || limit >= length-offset {
		return content[start:], length, false
	}
	return content[start : start+runeByteIndex(content[start:], limit)], length, true
}

// runeByteIndex is the byte index at which the n-th code point of s starts;
// n at or past the last code point answers len(s).
func runeByteIndex(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

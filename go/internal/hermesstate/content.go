// content.go — the decoder for hermes' content column.
//
// hermes stores a message content that is not a plain string (multimodal parts:
// text, image_url, base64 blobs) as the sentinel "\x00json:" followed by the
// JSON serialisation, with no truncation and no length cap. A reader that
// passes that through hands a downstream credential scanner a base64 image to
// classify and hands a substring comparison JSON escapes instead of prose. So
// the fold happens here, at the edge of the package: text parts only, no NUL,
// and a payload that does not parse costs its row rather than being guessed at.
//
// Source: https://github.com/GottZ/ctx
package hermesstate

import (
	"encoding/json"
	"strings"
)

// contentJSONPrefix mirrors _CONTENT_JSON_PREFIX in hermes_state.py.
const contentJSONPrefix = "\x00json:"

// maxPartDepth bounds the recursion into a foreign JSON document. Nesting
// beyond this is not a content shape hermes writes; it is a hostile input.
const maxPartDepth = 8

// decodeContent folds a stored content value into plain text. The second return
// value is false when the row must be dropped, which happens for exactly one
// reason: the sentinel is present and the payload behind it does not parse.
func decodeContent(raw string) (string, bool) {
	if !strings.HasPrefix(raw, contentJSONPrefix) {
		return stripNUL(raw), true
	}
	var doc any
	if err := json.Unmarshal([]byte(raw[len(contentJSONPrefix):]), &doc); err != nil {
		return "", false
	}
	var b strings.Builder
	foldText(&b, doc, 0)
	return stripNUL(b.String()), true
}

// foldText appends the text-bearing parts of doc to b, in document order.
// Non-textual parts — image_url, input_audio, base64 blobs of any kind — are
// dropped, not transcoded and not summarised.
func foldText(b *strings.Builder, doc any, depth int) {
	if depth > maxPartDepth {
		return
	}
	switch v := doc.(type) {
	case string:
		writePart(b, v)
	case []any:
		for _, part := range v {
			foldText(b, part, depth+1)
		}
	case map[string]any:
		if kind, ok := v["type"].(string); ok && kind != "text" {
			return
		}
		if text, ok := v["text"].(string); ok {
			writePart(b, text)
		}
	}
}

func writePart(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(s)
}

// stripNUL removes NUL bytes unconditionally — also from content that never
// carried the sentinel. A NUL that reaches a prompt, a block or a substring
// comparison truncates it somewhere downstream, and no legitimate tool output
// depends on carrying one.
func stripNUL(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

package oai_test

// H-9 over the route (FOLGE-P hardening; Delta-Review W02-09 F3-1): a
// refusal that names a client key stays a few hundred bytes however long the
// key — before, the name went into param and message uncut and the answer's
// writer HTML-escaped it, an 8 MiB key of "<" was an answer of 100 MB.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// echoAnswerMax bounds the answer of a refusal that names a client key: the
// envelope plus param and message, the name in each cut to EchoNameMax bytes
// and escaped by the writer at most six bytes per byte.
const echoAnswerMax = 4 << 10

// echoForms are refusals naming one key of about lim bytes, on every level.
func echoForms(lim int) []struct{ name, body, code, param string } {
	msg := `{"model":"chat","messages":[{"role":"user","content":"x"}]`
	lt := func(n int) string { return strings.Repeat("<", n) }
	cut := lt(oai.EchoNameMax) + "…"
	escLt := strings.Repeat(fmt.Sprintf("%cu003c", '\\'), (lim-len(msg)-8)/6)
	half := lt((lim - len(msg) - 64) / 2)
	return []struct{ name, body, code, param string }{
		{"top level", msg + `,"` + lt(lim-len(msg)-8) + `":0}`, oai.CodeUnsupportedParameter, cut},
		{"top level, escaped", msg + `,"` + escLt + `":0}`, oai.CodeUnsupportedParameter, cut},
		{"top level, repeated", msg + `,"` + half + `":0,"` + half + `":1}`, oai.CodeInvalidJSON, cut},
		{"message", `{"model":"chat","messages":[{"role":"user","content":"x","` + lt(lim-80) + `":0}]}`, oai.CodeUnsupportedParameter, "messages[0]." + cut},
		{"part", `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x","` + lt(lim-100) + `":0}]}]}`, oai.CodeUnsupportedParameter, "messages[0].content[0]." + cut},
		{"image_url", `{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","` + lt(lim-160) + `":0}}]}]}`,
			oai.CodeUnsupportedParameter, "messages[0].content[0].image_url." + cut},
		{"hull, repeated", msg + `,"tool_choice":{"type":"function","` + half + `":0,"` + half + `":1}}`, oai.CodeInvalidJSON, "tool_choice." + cut},
		{"logit_bias, repeated", msg + `,"logit_bias":{"` + half + `":0,"` + half + `":1}}`, oai.CodeInvalidJSON, "logit_bias." + cut},
	}
}

// TestEchoedKeyKeepsTheAnswerSmall: an 8 MiB key on every level is a 400
// whose answer is at most echoAnswerMax bytes and names the key cut.
func TestEchoedKeyKeepsTheAnswerSmall(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	for _, f := range echoForms(8<<20 - 16) {
		a := rig.post(t, f.body)
		var e struct {
			Error struct {
				Code, Param, Message string
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(a.body), &e); err != nil || a.status != http.StatusBadRequest {
			t.Errorf("%s: answer %d (%d bytes, %v), want 400", f.name, a.status, len(a.body), err)
			continue
		}
		if len(a.body) > echoAnswerMax {
			t.Errorf("%s: an answer of %d bytes for a body of %d, want ≤ %d", f.name, len(a.body), len(f.body), echoAnswerMax)
		}
		if e.Error.Code != f.code || e.Error.Param != f.param || !strings.Contains(e.Error.Message, f.param) {
			t.Errorf("%s: %s %.120q %.120q, want %s %q", f.name, e.Error.Code, e.Error.Param, e.Error.Message, f.code, f.param)
		}
	}
}

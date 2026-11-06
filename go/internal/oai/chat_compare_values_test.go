package oai_test

// Hardening 6, P-1 and P-2 (Delta-Review 5 M-1, L-3): response_format.type
// and stream are compared in place, no longer decoded. The answers stay what
// they were — for every allowed name in any JSON spelling, for short unknown
// names, for values past the longest allowed name, for type errors. With
// CTX_FOLGEP_H6_DUMP the normalized answers go to a file for a byte
// comparison of two builds (report §Härtung 6).

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// perRequest are the answer fields that differ per request (the answer's id
// is the request's uuidv7, created its clock).
var perRequest = regexp.MustCompile(`"id":"chatcmpl-[^"]*"|"created":[0-9]+`)

// rfBody is a request with response_format {"type": raw} (raw is the JSON
// value as sent) and, for json_schema, its hull.
func rfBody(raw string) string {
	return `{"model":"chat","messages":[{"role":"user","content":"hi"}],"response_format":{"type":` + raw + `,"json_schema":{"name":"s"}}}`
}

func streamValueBody(raw string) string {
	return `{"model":"chat","messages":[{"role":"user","content":"hi"}],"stream":` + raw + `}`
}

// The cases and the status each answers with (on the base and here).
var compareValueCases = []struct {
	name, body string
	status     int
	code       string // the error code of a refusal
}{
	{"rf text", rfBody(`"text"`), http.StatusOK, ""},
	{"rf json_object", rfBody(`"json_object"`), http.StatusOK, ""},
	{"rf json_schema", rfBody(`"json_schema"`), http.StatusOK, ""},
	{"rf text escaped", rfBody(`"text"`), http.StatusOK, ""},
	{"rf json_schema, every character escaped (66 raw bytes)", rfBody(`"` + escapeAll("json_schema") + `"`), http.StatusOK, ""},
	{"rf json_schema escaped, one byte past (67)", rfBody(`"` + escapeAll("json_schema") + ` "`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf text, whitespace around", rfBody(" \"text\" \n"), http.StatusOK, ""},
	{"rf bogus", rfBody(`"bogus"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf TEXT", rfBody(`"TEXT"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf text with a space", rfBody(`"text "`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf empty", rfBody(`""`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf lone surrogate", rfBody(`"\ud800"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf invalid UTF-8", rfBody("\"te\xffxt\""), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf 67 bytes plain", rfBody(`"` + strings.Repeat("a", 67) + `"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf 64 KiB invalid UTF-8", rfBody(`"` + strings.Repeat("\xff", 65536-len(`{"type":"","json_schema":{"name":"s"}}`)) + `"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf 64 KiB lone surrogates", rfBody(`"` + strings.Repeat(`\ud800`, (65536-len(`{"type":"","json_schema":{"name":"s"}}`))/6) + `"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf past the policy cap", rfBody(`"` + strings.Repeat("a", 65536) + `"`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf null", rfBody(`null`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf 1", rfBody(`1`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf true", rfBody(`true`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf array", rfBody(`["text"]`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf object", rfBody(`{"text":1}`), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf deep array", rfBody(strings.Repeat("[", 9990) + strings.Repeat("]", 9990)), http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"rf type absent", `{"model":"chat","messages":[{"role":"user","content":"hi"}],"response_format":{"json_schema":{"name":"s"}}}`, http.StatusBadRequest, oai.CodeUnsupportedValue},
	{"stream false", streamValueBody(`false`), http.StatusOK, ""},
	{"stream false, whitespace", streamValueBody(" false\n"), http.StatusOK, ""},
	{"stream null", streamValueBody(`null`), http.StatusOK, ""},
	{"stream true", streamValueBody(`true`), http.StatusOK, ""}, // W02-08: served as a stream (FOLGE-P alone: 400 unsupported_value)
	{"stream \"true\"", streamValueBody(`"true"`), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream 1", streamValueBody(`1`), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream 0", streamValueBody(`0`), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream object", streamValueBody(`{}`), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream array", streamValueBody(`[true]`), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream deep array", streamValueBody(strings.Repeat("[", 9997) + strings.Repeat("]", 9997)), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream deep object", streamValueBody(strings.Repeat(`{"a":`, 9997) + "0" + strings.Repeat("}", 9997)), http.StatusBadRequest, oai.CodeInvalidType},
	{"stream long string", streamValueBody(`"` + strings.Repeat("\xff", 1<<16) + `"`), http.StatusBadRequest, oai.CodeInvalidType},
}

// escapeAll writes s with every character as a \u escape.
func escapeAll(s string) string {
	var b strings.Builder
	for _, c := range s {
		fmt.Fprintf(&b, `\u%04x`, c)
	}
	return b.String()
}

func TestComparedValuesAnswerAsBefore(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	// A stream request (W02-08) gets a stream; every other one the answer.
	stream := sseRespond(streamShapes[0].steps(), nil, nil)
	u.setRespond(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		last := u.bodies[len(u.bodies)-1]
		u.mu.Unlock()
		if last["stream"] == true {
			stream(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	})
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	var dump strings.Builder
	for _, tc := range compareValueCases {
		a := rig.post(t, tc.body)
		code := ""
		if a.status != http.StatusOK {
			code, _, _ = a.envelope(t)
		}
		if a.status != tc.status || code != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, a.status, a.body, tc.status, tc.code)
		}
		fmt.Fprintf(&dump, "== %s\n%s\n", tc.name, perRequest.ReplaceAllString(normalized(a), "…"))
	}
	if p := os.Getenv("CTX_FOLGEP_H6_DUMP"); p != "" {
		if err := os.WriteFile(p, []byte(dump.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

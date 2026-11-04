package fedhop

// Gates of F-W2 (design/04 §4.1, §4.2, §5.2 S10) on the Go side:
//
//   - TestIdentityNeverRendersTheSecret: no fmt verb, no wrapper (exported or
//     unexported field, slice, map, pointer), no log/slog handler and no
//     encoding/json path renders the secret in any spelling (hex, base64,
//     fmt's byte-slice forms, the raw bytes). Red against the design's plain
//     `secret []byte` field without the formatting methods.
//   - TestIdentityRendersItsID: every one of those paths prints the id, and
//     only the id. Red against an Identity without String/GoString/LogValue/
//     Format (it prints its fields).
//   - TestIdentityFlowsThroughTheRequestContext: two routers in one process
//     with different identities — a handler reads its own router's identity
//     through IdentityFrom(r.Context()); a detached context reads the boot
//     default; before any default, the zero Identity; a router built with
//     the zero Identity reads the default of the moment.
//   - TestNewIdentityCopiesTheSecret: the caller's slice is not aliased.
//
//	go test -count=1 -run 'TestIdentity|TestNewIdentity' ./internal/fedhop/

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testID = "6f1c2c9e-4d0b-4a52-9a43-0c3f5e1d2b7a"

// testSecret is 32 bytes no rendering of an id or of fmt's punctuation can
// contain by accident.
var testSecret = []byte{
	0xd3, 0x41, 0x9e, 0x07, 0xbb, 0x5c, 0x12, 0xf0, 0x88, 0x2a, 0x6d, 0xe1, 0x37, 0xc4, 0x90, 0x5b,
	0x0e, 0xa7, 0x73, 0x19, 0xfd, 0x62, 0xce, 0x84, 0x2f, 0xb6, 0x58, 0x03, 0x9a, 0xe9, 0x4c, 0x71,
}

// renderings: every spelling under which the secret could surface.
func renderings(s []byte) []string {
	h := hex.EncodeToString(s)
	return []string{
		h, strings.ToUpper(h), h[:16], h[len(h)-16:],
		base64.StdEncoding.EncodeToString(s), base64.RawStdEncoding.EncodeToString(s),
		base64.URLEncoding.EncodeToString(s), base64.RawURLEncoding.EncodeToString(s),
		strings.Trim(fmt.Sprintf("%d", s), "[]"), // "211 65 158 …" — fmt's %v (= %d) of a byte slice, nested or not
		strings.Trim(fmt.Sprintf("%#v", s), "[]byte{}"), // "0xd3, 0x41, …"
		fmt.Sprintf("%x", s[:8]), fmt.Sprintf("% x", s[:8]),
		string(s[:8]), // %s of a byte slice: the raw bytes
	}
}

func leaks(out string) []string {
	var hits []string
	for _, r := range renderings(testSecret) {
		if strings.Contains(out, r) {
			hits = append(hits, r)
		}
	}
	return hits
}

// renderAll returns every rendering this file probes, by name.
func renderAll(t *testing.T, ident Identity) map[string]string {
	t.Helper()
	type exported struct{ I Identity }
	type unexported struct{ i Identity }
	out := map[string]string{}
	for _, v := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%o", "%b", "%t", "%10.3v", "%-40s"} {
		out["fmt "+v] = fmt.Sprintf(v, ident)
		out["fmt "+v+" pointer"] = fmt.Sprintf(v, &ident)
		out["fmt "+v+" exported field"] = fmt.Sprintf(v, exported{ident})
		out["fmt "+v+" unexported field"] = fmt.Sprintf(v, unexported{ident})
		out["fmt "+v+" slice"] = fmt.Sprintf(v, []Identity{ident})
		out["fmt "+v+" map"] = fmt.Sprintf(v, map[string]Identity{"a": ident})
	}
	out["fmt Sprint"] = fmt.Sprint(ident, &ident)
	out["fmt Sprintln"] = fmt.Sprintln(ident, unexported{ident})
	out["fmt Errorf"] = fmt.Errorf("boot: %v (%+v, %#v)", ident, unexported{ident}, &ident).Error()
	for name, h := range map[string]func(io.Writer) slog.Handler{
		"slog text": func(w io.Writer) slog.Handler { return slog.NewTextHandler(w, nil) },
		"slog json": func(w io.Writer) slog.Handler { return slog.NewJSONHandler(w, nil) },
	} {
		var buf bytes.Buffer
		l := slog.New(h(&buf))
		l.Info("identity", "ident", ident, "ptr", &ident, "exported", exported{ident}, "unexported", unexported{ident},
			slog.Any("any", ident), slog.Group("g", "ident", ident))
		l.With("ident", ident).Error("boot", "err", fmt.Errorf("x: %v", ident))
		out[name] = buf.String()
	}
	for name, v := range map[string]any{"value": ident, "pointer": &ident, "exported field": exported{ident}, "slice": []Identity{ident}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("json.Marshal(%s): %v", name, err)
		}
		out["json "+name] = string(b)
	}
	return out
}

func TestIdentityNeverRendersTheSecret(t *testing.T) {
	ident := NewIdentity(testID, testSecret)
	if !bytes.Equal(testKey(ident), testSecret) {
		t.Fatal("NewIdentity did not keep the secret — the probe would be blind")
	}
	for name, out := range renderAll(t, ident) {
		if hits := leaks(out); len(hits) > 0 {
			t.Errorf("%s renders the secret (%v): %q", name, hits, out)
		}
	}
}

func TestIdentityRendersItsID(t *testing.T) {
	ident := NewIdentity(testID, testSecret)
	for _, c := range []struct{ name, got, want string }{
		{"String", stringerOf(ident), testID},
		{"GoString", goStringerOf(ident), `fedhop.Identity{ID:"` + testID + `"}`},
		{"%v", fmt.Sprintf("%v", ident), testID},
		{"%+v", fmt.Sprintf("%+v", ident), testID},
		{"%s", fmt.Sprintf("%s", ident), testID},
		{"%d", fmt.Sprintf("%d", ident), testID},
		{"%x", fmt.Sprintf("%x", ident), testID},
		{"%q", fmt.Sprintf("%q", ident), `"` + testID + `"`},
		{"%#v", fmt.Sprintf("%#v", ident), `fedhop.Identity{ID:"` + testID + `"}`},
		{"%v pointer", fmt.Sprintf("%v", &ident), testID},
		{"LogValue", logValueOf(ident), testID},
		{"json", func() string { b, _ := json.Marshal(ident); return string(b) }(), `{"ID":"` + testID + `"}`},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("boot", "instance", ident)
	if !strings.Contains(buf.String(), " instance="+testID+"\n") {
		t.Errorf("slog text: %q, want instance=%s", buf.String(), testID)
	}
}

// stringerOf, goStringerOf and logValueOf call the method through its
// interface, so a mutation without the method fails here instead of not
// compiling.
func stringerOf(v any) string {
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return "<no String method>"
}

func goStringerOf(v any) string {
	if s, ok := v.(fmt.GoStringer); ok {
		return s.GoString()
	}
	return "<no GoString method>"
}

func logValueOf(v any) string {
	if s, ok := v.(slog.LogValuer); ok {
		return s.LogValue().String()
	}
	return "<no LogValue method>"
}

// testKey reads the key of i (nil for the zero Identity).
func testKey(i Identity) []byte {
	if i.key == nil {
		return nil
	}
	return i.key()
}

// resetDefault forgets the boot default (test-only; the default is global).
func resetDefault(t *testing.T) {
	t.Helper()
	defaultIdentity.Store(nil)
	t.Cleanup(func() { defaultIdentity.Store(nil) })
}

func TestIdentityFlowsThroughTheRequestContext(t *testing.T) {
	resetDefault(t)
	if got := IdentityFrom(context.Background()); got.ID != "" || testKey(got) != nil {
		t.Fatalf("before any default: %q, want the zero Identity", got.ID)
	}
	boot := NewIdentity("00000000-0000-4000-8000-00000000000d", bytes.Repeat([]byte{1}, 32))
	SetDefaultIdentity(boot)

	a := NewIdentity("00000000-0000-4000-8000-00000000000a", bytes.Repeat([]byte{2}, 32))
	b := NewIdentity("00000000-0000-4000-8000-00000000000b", bytes.Repeat([]byte{3}, 32))
	detached := make(chan string, 2)
	router := func(ident Identity) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("/who", func(w http.ResponseWriter, r *http.Request) {
			// A copy detached from the request's cancellation keeps its values;
			// a fresh background context (a scheduler arm) has none.
			detached <- IdentityFrom(context.WithoutCancel(r.Context())).ID + "|" + IdentityFrom(context.Background()).ID
			_, _ = io.WriteString(w, IdentityFrom(r.Context()).ID)
		})
		return IdentityMiddleware(ident)(mux)
	}
	ra, rb := router(a), router(b)
	for _, c := range []struct {
		h    http.Handler
		want Identity
	}{{ra, a}, {rb, b}, {ra, a}} {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/who", nil))
		if got := rec.Body.String(); got != c.want.ID {
			t.Errorf("handler behind the router of %s read %q", c.want.ID, got)
		}
		if got, want := <-detached, c.want.ID+"|"+boot.ID; got != want {
			t.Errorf("detached contexts read %q, want %q (request copy | background = boot default)", got, want)
		}
	}
	// The identity in the context is the one the middleware was built with,
	// whatever the default says later.
	SetDefaultIdentity(b)
	rec := httptest.NewRecorder()
	ra.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/who", nil))
	<-detached
	if rec.Body.String() != a.ID {
		t.Errorf("after a new default the router of %s reads %q", a.ID, rec.Body.String())
	}
	// A router built with the zero Identity does not freeze it: its requests
	// read the default of the moment.
	zero := router(Identity{})
	rec = httptest.NewRecorder()
	zero.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/who", nil))
	<-detached
	if rec.Body.String() != b.ID {
		t.Errorf("router built with the zero Identity read %q, want the default %s", rec.Body.String(), b.ID)
	}
	if got := IdentityFrom(WithIdentity(context.Background(), a)); got.ID != a.ID || !bytes.Equal(testKey(got), testKey(a)) {
		t.Errorf("WithIdentity/IdentityFrom round trip: %q", got.ID)
	}
}

func TestNewIdentityCopiesTheSecret(t *testing.T) {
	in := bytes.Clone(testSecret)
	ident := NewIdentity(testID, in)
	in[0] ^= 0xff
	if !bytes.Equal(testKey(ident), testSecret) {
		t.Error("NewIdentity aliases the caller's slice: a later write to it changes the key")
	}
}

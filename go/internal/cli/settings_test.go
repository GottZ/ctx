package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The settings commands must convert an API-level failure into a command
// error (cobra: stderr + exit 1) — never the PrintJSON-and-exit-0 shape of
// the older endpoint commands. checkSettingsEnvelope is that gate.
func TestCheckSettingsEnvelope(t *testing.T) {
	cases := []struct {
		name, body string
		wantErr    string // "" = success
	}{
		{"success", `{"success":true,"settings":[]}`, ""},
		{"validation 422", `{"success":false,"error":"validation: rerank.blend_weight must be in [0,1]"}`,
			"must be in [0,1]"},
		{"restart 409", `{"success":false,"error":"dream.parallelism is restart-only; set CTX_DREAM_PARALLELISM and restart"}`,
			"restart-only"},
		{"admin 403", `{"success":false,"error":"admin key required"}`, "admin key required"},
		{"error without message", `{"success":false}`, "request failed"},
		{"unparseable", `<html>proxy error</html>`, "unparseable response"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkSettingsEnvelope([]byte(c.body))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, c.wantErr)
			}
		})
	}
}

// toJSONValue decides the transport shape; the server normalizes the type.
// Literal scalars pass through, arbitrary text becomes a JSON string.
func TestToJSONValue(t *testing.T) {
	cases := map[string]string{
		"0.7":          `0.7`,
		"42":           `42`,
		"true":         `true`,
		`"quoted"`:     `"quoted"`,
		"qwen3.5:9b":   `"qwen3.5:9b"`,
		"45d":          `"45d"`,
		"hello world":  `"hello world"`,
		"[1,2]":        `"[1,2]"`, // arrays are not scalars — stringified, server 422s
		`{"a":1}`:      `"{\"a\":1}"`,
		"private,work": `"private,work"`,
	}
	for in, want := range cases {
		if got := string(toJSONValue(in)); got != want {
			t.Errorf("toJSONValue(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRenderCell(t *testing.T) {
	if got := renderCell([]any{"private", "shared"}); got != "private,shared" {
		t.Errorf("slice cell = %q", got)
	}
	if got := renderCell(0.7); got != "0.7" {
		t.Errorf("float cell = %q", got)
	}
	if got := renderCell("(set via env)"); got != "(set via env)" {
		t.Errorf("string cell = %q", got)
	}
}

// Client.Do carries method, path, auth header and body to the REST surface
// and returns body + status untouched.
func TestClientDo(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("X-Context-Key")
		buf := make([]byte, 256)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"success":false,"error":"validation: nope"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL + "/", Key: "test-key", HTTPClient: srv.Client()}
	resp, status, err := c.Do(http.MethodPut, "/api/settings/rerank.blend_weight",
		map[string]any{"value": toJSONValue("1.5")})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if gotMethod != http.MethodPut || gotPath != "/api/settings/rerank.blend_weight" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotKey != "test-key" {
		t.Errorf("auth header = %q", gotKey)
	}
	if gotBody != `{"value":1.5}` {
		t.Errorf("body = %s", gotBody)
	}
	if status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", status)
	}
	// The 422 envelope must turn into a command error downstream.
	if err := checkSettingsEnvelope(resp); err == nil || !strings.Contains(err.Error(), "validation") {
		t.Errorf("envelope err = %v", err)
	}
}

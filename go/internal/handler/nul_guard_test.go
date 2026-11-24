package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// nulEsc is the JSON escape of a NUL character, assembled so no source line
// carries it.
const nulEsc = "\\" + "u0000"

func TestJSONNULPath(t *testing.T) {
	cases := []struct {
		name, raw string
		skip      string
		want      string
		found     bool
	}{
		{"none", `{"a":"b","c":[1,2,{"d":"e"}]}`, "", "", false},
		{"top-level field", `{"a":"b","title":"x` + nulEsc + `"}`, "", "title", true},
		{"raw byte in a string", "{\"content\":\"a\x00b\"}", "", "content", true},
		{"escaped backslash is no NUL", `{"content":"a\\` + nulEsc[1:] + `"}`, "", "", false},
		{"odd run is a NUL", `{"content":"a\\\\` + nulEsc + `"}`, "", "content", true},
		{"nested object", `{"metadata":{"a":1,"source":"x` + nulEsc + `"}}`, "", "metadata.source", true},
		{"list of strings", `{"tags":["a","b` + nulEsc + `"]}`, "", "tags[1]", true},
		{"list of objects", `{"items":[{"title":"t"},{"title":"x","content":"` + nulEsc + `"}]}`, "", "items[1]: content", true},
		{"deep in an item", `{"items":[{"metadata":{"k":"` + nulEsc + `"}}]}`, "", "items[0]: metadata.k", true},
		{"a key names its object", `{"metadata":{"a` + nulEsc + `":"v"}}`, "", "metadata", true},
		{"a top-level key", `{"a` + nulEsc + `":"v"}`, "", "", true},
		{"escaped key text", `{"me\u0074a":{"k":"` + nulEsc + `"}}`, "", "meta.k", true},
		{"exempt field, then another", `{"value":"` + nulEsc + `","name":"` + nulEsc + `"}`, "value", "name", true},
		{"exempt field only", `{"value":"` + nulEsc + `"}`, "value", "", false},
		{"after the first value", `{"a":"b"} {"c":"` + nulEsc + `"}`, "", "", false},
		{"invalid before the NUL", `{"a":"b" "c":"` + nulEsc + `"}`, "", "", false},
		{"string top level", `"` + nulEsc + `"`, "", "", true},
		{"other escapes", `{"a":"\n\t\"\u00e4\/","b":"` + nulEsc + `"}`, "", "b", true},
		{"numbers and literals", `{"a":1.5e3,"b":true,"c":null,"d":[false,"` + nulEsc + `"]}`, "", "d[1]", true},
	}
	for _, tc := range cases {
		skip := func(p string) bool { return tc.skip != "" && p == tc.skip }
		got, found := jsonNULPath([]byte(tc.raw), skip)
		if got != tc.want || found != tc.found {
			t.Errorf("%s: jsonNULPath(%s) = %q, %v; want %q, %v", tc.name, tc.raw, got, found, tc.want, tc.found)
		}
	}
}

// nulGuardServe runs one request through a chi router that mounts the guard
// at pattern, and reports the status, the answer and what the handler read.
func nulGuardServe(t *testing.T, guard func(http.Handler) http.Handler, method, pattern string, req *http.Request) (int, string, []byte, bool) {
	t.Helper()
	var got []byte
	var ran bool
	r := chi.NewRouter()
	r.With(guard).MethodFunc(method, pattern, func(w http.ResponseWriter, r *http.Request) {
		ran = true
		b, err := io.ReadAll(r.Body)
		got = b
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusTeapot)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String(), got, ran
}

func TestNULGuardRefusesAndNames(t *testing.T) {
	json := RefuseNUL(NULBodyJSON, 1<<20)
	cases := []struct {
		name, method, pattern, url, body, ctype string
		guard                                   func(http.Handler) http.Handler
		want                                    string // "" = passes
	}{
		{"body field", http.MethodPost, "/api/store", "/api/store", `{"title":"a` + nulEsc + `"}`, "application/json", json, "title"},
		{"body raw byte", http.MethodPost, "/api/store", "/api/store", "{\"title\":\"a\x00\"}", "application/json", json, "title"},
		{"JSON read whatever the content type", http.MethodPost, "/api/store", "/api/store", `{"content":"` + nulEsc + `"}`, "application/x-www-form-urlencoded", json, "content"},
		{"literal percent zero zero in JSON passes", http.MethodPost, "/api/store", "/api/store", `{"content":"50%00 off"}`, "application/x-www-form-urlencoded", json, ""},
		{"path parameter", http.MethodDelete, "/api/secrets/{name}", "/api/secrets/a%00b", "", "", json, "{name}"},
		{"query parameter", http.MethodGet, "/api/graph/ego", "/api/graph/ego?id=a%00b", "", "", json, "?id"},
		{"query key", http.MethodGet, "/api/graph/ego", "/api/graph/ego?a%00b=1", "", "", json, "#1 (its name)"},
		{"exempt secret value", http.MethodPut, "/api/secrets/{name}", "/api/secrets/s", `{"value":"` + nulEsc + `"}`, "application/json", json, ""},
		{"secret value on another route", http.MethodPut, "/api/settings/{key}", "/api/settings/k", `{"value":"` + nulEsc + `"}`, "application/json", json, "value"},
		{"an ingest chunk is the handler's", http.MethodPost, "/api/ingest", "/api/ingest", `{"chunks":[{"title":"t","content":"` + nulEsc + `"}]}`, "application/json", json, ""},
		{"an ingest field is the guard's", http.MethodPost, "/api/ingest", "/api/ingest", `{"target_scope":"` + nulEsc + `","chunks":[]}`, "application/json", json, "target_scope"},
		{"manage refuses on its own", http.MethodPost, "/api/manage", "/api/manage", `{"action":"get","data":{"id":"` + nulEsc + `"}}`, "application/json", json, ""},
		{"form value", http.MethodPost, "/token", "/token", "grant_type=x&client_id=a%00b", "application/x-www-form-urlencoded",
			RefuseNULOAuth(NULBodyForm, AuthFlowBodyCap, "invalid_request"), "client_id"},
		{"form raw byte", http.MethodPost, "/token", "/token", "client_id=a\x00b", "application/x-www-form-urlencoded",
			RefuseNULOAuth(NULBodyForm, AuthFlowBodyCap, "invalid_request"), "client_id"},
		{"multipart raw byte", http.MethodPost, "/token", "/token", "--x\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nb\x00\r\n--x--\r\n", "multipart/form-data; boundary=x",
			RefuseNULOAuth(NULBodyForm, AuthFlowBodyCap, "invalid_request"), "body"},
		{"form route, JSON body is not a form", http.MethodPost, "/token", "/token", `{"client_id":"` + nulEsc + `"}`, "application/json",
			RefuseNULOAuth(NULBodyForm, AuthFlowBodyCap, "invalid_request"), ""},
		{"no body read", http.MethodPost, "/mcp", "/mcp", `{"params":{"arguments":{"a":"` + nulEsc + `"}}}`, "application/json", RefuseNUL(NULBodyNone, 0), ""},
	}
	for _, tc := range cases {
		var body io.Reader
		if tc.body != "" {
			body = strings.NewReader(tc.body)
		}
		req := httptest.NewRequest(tc.method, tc.url, body)
		if tc.ctype != "" {
			req.Header.Set("Content-Type", tc.ctype)
		}
		status, answer, got, ran := nulGuardServe(t, tc.guard, tc.method, tc.pattern, req)
		if tc.want == "" {
			if !ran || status != http.StatusNoContent || string(got) != tc.body {
				t.Errorf("%s: want the handler to read the body unchanged, got ran=%v %d %q (read %q)", tc.name, ran, status, answer, got)
			}
			continue
		}
		if ran || status != http.StatusBadRequest || !strings.Contains(answer, tc.want+" "+nulFieldMessage) {
			t.Errorf("%s: want 400 naming %q before the handler, got ran=%v %d %s", tc.name, tc.want, ran, status, answer)
		}
		if strings.Contains(answer, nulEsc) {
			t.Errorf("%s: the answer echoes the NUL: %s", tc.name, answer)
		}
	}
}

func TestNULGuardHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req.Header.Set("X-B", "ok")
	req.Header["User-Agent"] = []string{"a\x00b"}
	status, answer, _, ran := nulGuardServe(t, RefuseNUL(NULBodyNone, 0), http.MethodGet, "/api/whoami", req)
	if ran || status != http.StatusBadRequest || !strings.Contains(answer, "header User-Agent "+nulFieldMessage) {
		t.Errorf("want 400 naming the header, got ran=%v %d %s", ran, status, answer)
	}
}

func TestNULGuardEnvelopes(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(`{"title":"`+nulEsc+`"}`))
	_, answer, _, _ := nulGuardServe(t, RefuseNUL(NULBodyJSON, 1<<20), http.MethodPost, "/api/store", req)
	var house map[string]any
	if err := json.Unmarshal([]byte(answer), &house); err != nil || house["code"] != "invalid_body" || house["success"] != false {
		t.Errorf("house envelope: %s", answer)
	}
	req = httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"client_uri":"`+nulEsc+`"}`))
	_, answer, _, _ = nulGuardServe(t, RefuseNULOAuth(NULBodyJSON, AuthFlowBodyCap, "invalid_client_metadata"), http.MethodPost, "/register", req)
	var oauth map[string]string
	if err := json.Unmarshal([]byte(answer), &oauth); err != nil || oauth["error"] != "invalid_client_metadata" ||
		oauth["error_description"] != "client_uri "+nulFieldMessage {
		t.Errorf("OAuth envelope: %s", answer)
	}
}

// TestNULGuardBodyAtTheCap: a body the group's cap ends is handed on as it
// came — the handler sees the same bytes and the same error — and one past
// the guard's own limit is handed on whole, unscanned.
func TestNULGuardBodyAtTheCap(t *testing.T) {
	const capBytes = 64
	over := `{"content":"` + strings.Repeat("a", 80) + nulEsc + `"}`
	r := chi.NewRouter()
	var got []byte
	var gotErr error
	r.With(MaxBodySize(capBytes), RefuseNUL(NULBodyJSON, capBytes)).Post("/x", func(w http.ResponseWriter, r *http.Request) {
		got, gotErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(over)))
	var mbe *http.MaxBytesError
	if !errors.As(gotErr, &mbe) || mbe.Limit != capBytes || string(got) != over[:capBytes] {
		t.Errorf("over the cap: the handler read %d bytes, err %v; want the first %d and the cap's error", len(got), gotErr, capBytes)
	}

	// No group cap (the auth flow's routes cap inside the handler): past the
	// guard's limit the body goes on whole.
	r = chi.NewRouter()
	r.With(RefuseNUL(NULBodyJSON, capBytes)).Post("/y", func(w http.ResponseWriter, r *http.Request) {
		got, gotErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/y", strings.NewReader(over)))
	if gotErr != nil || string(got) != over || rec.Code != http.StatusNoContent {
		t.Errorf("past the limit: read %q err %v status %d", got, gotErr, rec.Code)
	}
}

// BenchmarkNULGuard measures the guard on a store body without a NUL (the
// cost every write request pays): the body read into one buffer and one scan,
// against the same handler without the guard.
func BenchmarkNULGuard(b *testing.B) {
	for _, size := range []int{1 << 10, 100 << 10, 1 << 20} {
		content := strings.Repeat("lorem ipsum dolor sit amet, ", size/28+1)[:size-80]
		body, _ := json.Marshal(map[string]any{"category": "learnings", "title": "bench", "content": content, "tags": []string{"a"}})
		decode := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req storeRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				b.Fatal(err)
			}
			w.WriteHeader(http.StatusNoContent)
		})
		for _, guarded := range []bool{false, true} {
			name := fmt.Sprintf("%dkB/without", len(body)>>10)
			h := http.Handler(decode)
			if guarded {
				name = fmt.Sprintf("%dkB/guard", len(body)>>10)
				h = RefuseNUL(NULBodyJSON, 1<<21)(decode)
			}
			b.Run(name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(body)))
				for b.Loop() {
					req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(body))
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
				}
			})
		}
	}
}

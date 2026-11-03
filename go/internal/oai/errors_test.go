package oai_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// writerCases are the fixed writers of W02-02 with what each must put on the
// wire (design/02 §4.10.2 with masterplan K5 for the A-internal 503).
var writerCases = []struct {
	name        string
	write       func(w http.ResponseWriter)
	status      int
	errType     string
	code        string
	shouldRetry string
	headers     map[string]string
}{
	{"NotFound", func(w http.ResponseWriter) { oai.NotFound(w, nil) },
		http.StatusNotFound, "invalid_request_error", "not_found", "false", nil},
	{"MethodNotAllowed", func(w http.ResponseWriter) { oai.MethodNotAllowed(w, nil) },
		http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "false", nil},
	{"WriteInvalidAPIKey", oai.WriteInvalidAPIKey,
		http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "false",
		map[string]string{"WWW-Authenticate": `Bearer realm="ctx"`}},
	{"WriteRequestTooLarge", func(w http.ResponseWriter) { oai.WriteRequestTooLarge(w, 8<<20) },
		http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "false", nil},
	{"WriteOverloaded", oai.WriteOverloaded,
		http.StatusServiceUnavailable, "service_unavailable_error", "server_overloaded", "true",
		map[string]string{"Retry-After": "1"}},
	{"WriteInternal", oai.WriteInternal,
		http.StatusServiceUnavailable, "service_unavailable_error", "internal_error", "false", nil},
}

// TestWritersEnvelope: every writer sends the four keys (param null where it
// does not apply), an explicit x-should-retry, JSON content type, and a code
// that is in the exported vocabulary with the same status and type.
func TestWritersEnvelope(t *testing.T) {
	for _, tc := range writerCases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.write(rec)
			if rec.Code != tc.status {
				t.Errorf("status %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Header().Get("x-should-retry"); got != tc.shouldRetry {
				t.Errorf("x-should-retry %q, want %q", got, tc.shouldRetry)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type %q, want application/json", got)
			}
			for k, want := range tc.headers {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("%s %q, want %q", k, got, want)
				}
			}
			var raw struct {
				Error map[string]json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("not an envelope: %v (%q)", err, rec.Body.String())
			}
			for _, k := range []string{"message", "type", "param", "code"} {
				if _, ok := raw.Error[k]; !ok {
					t.Errorf("key %q missing from error object %s", k, rec.Body.String())
				}
			}
			if len(raw.Error) != 4 {
				t.Errorf("error object has %d keys, want exactly the four", len(raw.Error))
			}
			if string(raw.Error["param"]) != "null" {
				t.Errorf("param %s, want null", raw.Error["param"])
			}
			var code, typ, msg string
			_ = json.Unmarshal(raw.Error["code"], &code)
			_ = json.Unmarshal(raw.Error["type"], &typ)
			_ = json.Unmarshal(raw.Error["message"], &msg)
			if code != tc.code || typ != tc.errType || msg == "" {
				t.Errorf("code %q type %q message %q, want %q / %q / non-empty", code, typ, msg, tc.code, tc.errType)
			}
			spec, ok := vocabulary()[code]
			if !ok {
				t.Fatalf("code %q is not in oai.Codes", code)
			}
			if spec.Status != tc.status || spec.Type != tc.errType {
				t.Errorf("oai.Codes says %d/%s for %q, the writer sends %d/%s", spec.Status, spec.Type, code, tc.status, tc.errType)
			}
		})
	}
}

func vocabulary() map[string]oai.CodeSpec {
	m := map[string]oai.CodeSpec{}
	for _, c := range oai.Codes() {
		m[c.Code] = c
	}
	return m
}

// TestCodesVocabulary pins the exported code table W02-11 checks its
// classifier against: unique codes, each with an error status and one of the
// envelope types, and the members the design names (§4.10.2, K6, 06 §4.4,
// 04 §4.6). 500 belongs to exactly one code — the relayed upstream 500.
func TestCodesVocabulary(t *testing.T) {
	types := map[string]bool{
		oai.TypeInvalidRequest: true, oai.TypeRateLimit: true, oai.TypeServiceUnavailable: true,
		oai.TypeServer: true, oai.TypeInsufficientQuota: true,
	}
	seen := map[string]bool{}
	var fiveHundred []string
	for _, c := range oai.Codes() {
		if seen[c.Code] {
			t.Errorf("code %q listed twice", c.Code)
		}
		seen[c.Code] = true
		if c.Status < 400 || c.Status > 599 {
			t.Errorf("%s: status %d is not an error status", c.Code, c.Status)
		}
		if !types[c.Type] {
			t.Errorf("%s: type %q is not an envelope type", c.Code, c.Type)
		}
		if c.Status == http.StatusInternalServerError {
			fiveHundred = append(fiveHundred, c.Code)
		}
	}
	if len(fiveHundred) != 1 || fiveHundred[0] != oai.CodeUpstreamError {
		t.Errorf("500 carriers %v, want only %q (K5: A-internal faults are 503)", fiveHundred, oai.CodeUpstreamError)
	}
	for _, want := range []string{
		"not_found", "method_not_allowed", "invalid_api_key", "permission_denied", "rate_limit_exceeded", "server_busy",
		"concurrency_limit_exceeded", "insufficient_quota", "internal_error", "request_too_large", "server_overloaded",
		"service_unavailable", "invalid_json", "missing_required_parameter", "invalid_type", "unsupported_parameter",
		"unsupported_value", "model_not_found", "model_not_supported", "model_unavailable", "no_eligible_backend",
		"upstream_unavailable", "upstream_rejected", "context_length_exceeded", "upstream_error", "upstream_timeout",
		"federation_header_invalid", "federation_loop", "federation_depth", "federation_hop_required", "peer_instance_mismatch",
	} {
		if !seen[want] {
			t.Errorf("code %q missing from oai.Codes", want)
		}
	}
	got := oai.Codes()
	got[0].Status = 0
	if oai.Codes()[0].Status == 0 {
		t.Error("Codes() hands out the table itself — a caller changed the vocabulary")
	}
	if seen["tokens_exceed_limit"] {
		t.Error("tokens_exceed_limit is retired (design/06 E-06-9 = (a), Finding 31) and must not be in the vocabulary")
	}
}

// TestWriteGeneral: the general writer renders an empty code and param as
// null, a set param as a string, and x-should-retry from the flag.
func TestWriteGeneral(t *testing.T) {
	rec := httptest.NewRecorder()
	oai.Write(rec, oai.Error{Status: http.StatusBadRequest, Type: oai.TypeInvalidRequest, Code: oai.CodeUnsupportedValue,
		Param: "messages[2].content", Message: "Unsupported value."})
	want := `{"error":{"message":"Unsupported value.","type":"invalid_request_error","param":"messages[2].content","code":"unsupported_value"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("body %q\nwant %q", got, want)
	}
	if got := rec.Header().Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry %q, want false", got)
	}
	rec = httptest.NewRecorder()
	oai.Write(rec, oai.Error{Status: http.StatusServiceUnavailable, Type: oai.TypeServiceUnavailable, Message: "m", ShouldRetry: true})
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"param":null,"code":null`)) {
		t.Errorf("empty param/code not rendered as null: %s", rec.Body.String())
	}
	if got := rec.Header().Get("x-should-retry"); got != "true" {
		t.Errorf("x-should-retry %q, want true", got)
	}
}

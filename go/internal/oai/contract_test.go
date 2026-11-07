package oai_test

// Contract rig of the model API (wave W06-19, design/06 §4.14 "Konformität
// Stufe 1", masterplan W06-19 gate "CI-Job rot ohne Envelope"): the manifest
// conformance/oai/contract.json names every route of the model groups, every
// answer form it sends and the vendored schema each form is validated
// against. The tests here are what the required CI job "Conformance" runs
// (with the route census in cmd/ctxd, TestContractCoversEveryModelRoute):
//
//   - TestContractSchemaSetComplete: every form has a schema, every schema
//     file is pinned by SHA-256 and resolves, no vendored file is unlisted,
//     every pinning test exists;
//   - TestContractEnvelopeEveryCode: every code of the vocabulary (oai.Codes)
//     as WriteCode sends it — both x-should-retry values — parses in the
//     official Go SDK with its status, type and code, carries the explicit
//     x-should-retry and a JSON content type, and validates against the
//     vendored ErrorResponse; the rig's own counterproof feeds it answers
//     that are deliberately wrong and must see every one of them fail;
//   - TestContractRetryAfterBand: the L2 Retry-After of the /v1 admission
//     writer stays inside [step, ceil(step × (1 + jitter))] for every step of
//     the default ladder, and the jitter spreads;
//   - TestContractCIJobs: the CI wiring of the two stages — the required
//     "Conformance" job on every pull request and the nightly
//     "gateway-sdk-smoke" job — and the required-check list in GOVERNANCE.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
)

// contractManifest is conformance/oai/contract.json.
const contractManifest = "../../conformance/oai/contract.json"

// contractTestdata is the directory of the vendored schema files.
const contractTestdata = "../../conformance/oai/testdata"

type contractForm struct {
	Form     string   `json:"form"`
	File     *string  `json:"file"`
	Schema   *string  `json:"schema"`
	PinnedBy []string `json:"pinned_by"`
}

type contractRoute struct {
	Method string         `json:"method"`
	Path   string         `json:"path"`
	Forms  []contractForm `json:"forms"`
}

type contractDoc struct {
	SchemaCommit string            `json:"schema_commit"`
	Files        map[string]string `json:"files"`
	Routes       []contractRoute   `json:"routes"`
}

func loadContract(t testing.TB) contractDoc {
	t.Helper()
	raw, err := os.ReadFile(contractManifest)
	if err != nil {
		t.Fatalf("contract manifest: %v", err)
	}
	var doc contractDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("contract manifest is not valid JSON: %v", err)
	}
	return doc
}

// testFuncs are the names of the Test functions declared in this package's
// test files (the pinning tests the manifest names).
func testFuncs(t testing.TB) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	out := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(src, -1) {
			out[string(m[1])] = true
		}
	}
	return out
}

// schemaSetViolations is the gate of TestContractSchemaSetComplete as a
// function, so the counterproof can feed it a manifest with a gap.
func schemaSetViolations(t *testing.T, doc contractDoc) []string {
	t.Helper()
	var out []string
	tests := testFuncs(t)
	for _, rt := range doc.Routes {
		if len(rt.Forms) == 0 {
			out = append(out, fmt.Sprintf("%s %s: no answer form in the contract", rt.Method, rt.Path))
		}
		for _, f := range rt.Forms {
			where := fmt.Sprintf("%s %s form %q", rt.Method, rt.Path, f.Form)
			if f.File == nil || f.Schema == nil {
				out = append(out, where+": no vendored schema — gap in the schema set")
				continue
			}
			want, pinned := doc.Files[*f.File]
			if !pinned {
				out = append(out, fmt.Sprintf("%s: schema file %s is not pinned in \"files\"", where, *f.File))
				continue
			}
			raw, err := os.ReadFile(filepath.Join(contractTestdata, *f.File))
			if err != nil {
				out = append(out, fmt.Sprintf("%s: %v", where, err))
				continue
			}
			sum := sha256.Sum256(raw)
			if got := hex.EncodeToString(sum[:]); got != want {
				out = append(out, fmt.Sprintf("%s: %s has SHA-256 %s, the manifest pins %s", where, *f.File, got, want))
			}
			if !strings.Contains(string(raw), `"`+*f.Schema+`"`) {
				out = append(out, fmt.Sprintf("%s: schema %s is not defined in %s", where, *f.Schema, *f.File))
			}
			if len(f.PinnedBy) == 0 {
				out = append(out, where+": no pinning test")
			}
			for _, name := range f.PinnedBy {
				if !tests[name] {
					out = append(out, fmt.Sprintf("%s: pinning test %s does not exist in internal/oai", where, name))
				}
			}
		}
	}
	vendored, err := filepath.Glob(filepath.Join(contractTestdata, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range vendored {
		if _, ok := doc.Files[filepath.Base(p)]; !ok {
			out = append(out, fmt.Sprintf("vendored schema file %s is not pinned in the manifest", filepath.Base(p)))
		}
	}
	return out
}

// TestContractSchemaSetComplete is W06-19's gate against a gap in the schema
// set: every answer form of every route is validated against a pinned,
// vendored schema and pinned by a test that exists.
func TestContractSchemaSetComplete(t *testing.T) {
	doc := loadContract(t)
	if doc.SchemaCommit == "" {
		t.Error("contract manifest names no schema commit")
	}
	for _, v := range schemaSetViolations(t, doc) {
		t.Error(v)
	}
	// Every named schema resolves as JSON Schema (the loader fatals on its
	// own subtest only).
	for _, rt := range doc.Routes {
		for _, f := range rt.Forms {
			if f.File == nil || f.Schema == nil {
				continue
			}
			t.Run(rt.Method+" "+rt.Path+" "+f.Form, func(t *testing.T) {
				loadSchema(t, filepath.Join(contractTestdata, *f.File), *f.Schema)
			})
		}
	}
	// Counterproof: the gate sees a gap, a wrong SHA-256 and a missing
	// pinning test.
	t.Run("counterproof", func(t *testing.T) {
		bad := contractDoc{SchemaCommit: doc.SchemaCommit, Files: map[string]string{}}
		for k, v := range doc.Files {
			bad.Files[k] = v
		}
		file, schema := "openapi-c2aecc2f-error-response.json", "ErrorResponse"
		bad.Files[file] = strings.Repeat("0", 64)
		bad.Routes = []contractRoute{
			{Method: http.MethodPost, Path: "/v1/x", Forms: []contractForm{{Form: "success", PinnedBy: []string{"TestConformanceGoSDK"}}}},
			{Method: http.MethodGet, Path: "/v1/y", Forms: []contractForm{{Form: "error", File: &file, Schema: &schema, PinnedBy: []string{"TestNoSuchTest"}}}},
		}
		v := schemaSetViolations(t, bad)
		for _, want := range []string{"gap in the schema set", "SHA-256", "does not exist"} {
			if !slices.ContainsFunc(v, func(s string) bool { return strings.Contains(s, want) }) {
				t.Errorf("counterproof: the gate does not report %q (violations: %q)", want, v)
			}
		}
	})
}

// envelopeWriter answers code with the given x-should-retry.
type envelopeWriter func(w http.ResponseWriter, code string, retry bool)

// envelopeViolations runs every code of the vocabulary × both x-should-retry
// values through the Go SDK against a fake answering with write, and
// returns what a client would trip over — empty means conformant.
func envelopeViolations(t *testing.T, write envelopeWriter) []string {
	t.Helper()
	schema := loadErrorSchema(t)
	type seen struct {
		hits int
		body []byte
	}
	var mu sync.Mutex
	obs := map[string]*seen{}
	mux := http.NewServeMux()
	mux.HandleFunc("/c/{code}/{retry}/", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("code") + "/" + r.PathValue("retry")
		cw := &captureWriter{ResponseWriter: w}
		write(cw, r.PathValue("code"), r.PathValue("retry") == "true")
		mu.Lock()
		s := obs[key]
		if s == nil {
			s = &seen{}
			obs[key] = s
		}
		s.hits++
		s.body = cw.buf.Bytes()
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	var out []string
	for _, spec := range oai.Codes() {
		for _, retry := range []bool{false, true} {
			key := spec.Code + "/" + strconv.FormatBool(retry)
			bad := func(format string, args ...any) {
				out = append(out, key+": "+fmt.Sprintf(format, args...))
			}
			client := openai.NewClient(
				option.WithBaseURL(srv.URL+"/c/"+key+"/"),
				option.WithAPIKey("sk-contract"),
				option.WithMaxRetries(0),
				option.WithRequestTimeout(10*time.Second),
			)
			_, err := client.Models.List(t.Context())
			mu.Lock()
			s := obs[key]
			mu.Unlock()
			if s == nil || s.hits != 1 {
				bad("fake saw no single request (%v), want 1 (max retries 0)", s != nil)
				continue
			}
			var apiErr *openai.Error
			if !errors.As(err, &apiErr) {
				bad("SDK returned %v, want *openai.Error", err)
				continue
			}
			if apiErr.StatusCode != spec.Status {
				bad("status %d, want %d", apiErr.StatusCode, spec.Status)
			}
			if apiErr.Code != spec.Code {
				bad("SDK parsed code %q, want %q", apiErr.Code, spec.Code)
			}
			if apiErr.Type != spec.Type {
				bad("SDK parsed type %q, want %q", apiErr.Type, spec.Type)
			}
			if apiErr.Message == "" {
				bad("SDK parsed an empty message")
			}
			h := apiErr.Response.Header
			if got := h.Get("x-should-retry"); got != strconv.FormatBool(retry) {
				bad("x-should-retry %q, want %q", got, strconv.FormatBool(retry))
			}
			if got := h.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				bad("Content-Type %q, want application/json", got)
			}
			var v any
			if err := json.Unmarshal(s.body, &v); err != nil {
				bad("wire body is not JSON: %v (%.80q)", err, s.body)
				continue
			}
			if err := schema.Validate(v); err != nil {
				bad("wire body fails the vendored ErrorResponse: %v", err)
			}
		}
	}
	return out
}

// TestContractEnvelopeEveryCode is W06-19's Go contract (design/06 §4.14):
// every code the gateway can send — the table of design/06 §4.4 and the
// rest of the vocabulary — reaches a client as a valid ErrorResponse with
// its status, type, code and the explicit x-should-retry. The counterproof
// is the gate's red: the same rig against deliberately wrong answers.
func TestContractEnvelopeEveryCode(t *testing.T) {
	codes := oai.Codes()
	if len(codes) == 0 {
		t.Fatal("the vocabulary is empty")
	}
	v := envelopeViolations(t, func(w http.ResponseWriter, code string, retry bool) {
		oai.WriteCode(w, code, "Contract probe.", retry)
	})
	t.Logf("%d codes × 2 x-should-retry values through the Go SDK: %d violation(s)", len(codes), len(v))
	for _, s := range v {
		t.Error(s)
	}
	broken := map[string]envelopeWriter{
		// The house form of /api: a flat error string.
		"house form": func(w http.ResponseWriter, code string, retry bool) {
			spec := specOf(code)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-should-retry", strconv.FormatBool(retry))
			w.WriteHeader(spec.Status)
			_, _ = w.Write([]byte(`{"success":false,"error":"` + code + `"}`))
		},
		// The envelope without its type.
		"no type": func(w http.ResponseWriter, code string, retry bool) {
			spec := specOf(code)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("x-should-retry", strconv.FormatBool(retry))
			w.WriteHeader(spec.Status)
			_, _ = w.Write([]byte(`{"error":{"message":"m","param":null,"code":"` + code + `"}}`))
		},
		// The envelope without x-should-retry: the SDK decides retries on
		// its own (a 429 with max retries 2 becomes three requests).
		"no x-should-retry": func(w http.ResponseWriter, code string, _ bool) {
			spec := specOf(code)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(spec.Status)
			_, _ = w.Write([]byte(`{"error":{"message":"m","type":"` + spec.Type + `","param":null,"code":"` + code + `"}}`))
		},
		// Plain text, the house Recovery's 500 form.
		"plain text": func(w http.ResponseWriter, code string, _ bool) {
			http.Error(w, "Internal Server Error", specOf(code).Status)
		},
	}
	for name, write := range broken {
		t.Run("counterproof/"+name, func(t *testing.T) {
			v := envelopeViolations(t, write)
			t.Logf("deliberately wrong answer %q: %d violation(s), first %q", name, len(v), first(v))
			per := map[string]bool{}
			for _, s := range v {
				per[s[:strings.Index(s, ":")]] = true
			}
			if want := 2 * len(codes); len(per) != want {
				t.Errorf("the rig flagged %d of %d code/retry pairs of the wrong answer %q — the CI job would pass an answer without the envelope", len(per), want, name)
			}
		})
	}
}

func specOf(code string) oai.CodeSpec {
	for _, s := range oai.Codes() {
		if s.Code == code {
			return s
		}
	}
	return oai.CodeSpec{Code: code, Status: http.StatusTeapot}
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// contractRetryAfterSteps is the default ladder of the L2 Retry-After
// (dispatch.external_retry_after_steps, design/03 A03-W6: {1,2,5,10,20,30}).
var contractRetryAfterSteps = []int{1, 2, 5, 10, 20, 30}

// retryAfterOutsideBand reports a Retry-After header value outside
// [step, ceil(step × (1 + jitter))] — "" when inside.
func retryAfterOutsideBand(header string, step int, jitter float64) string {
	n, err := strconv.Atoi(header)
	if err != nil {
		return fmt.Sprintf("Retry-After %q is no whole number of seconds", header)
	}
	hi := int(float64(step)*(1+jitter) + 0.999999)
	if n < step || n > hi {
		return fmt.Sprintf("Retry-After %d outside [%d, %d] for step %d s, jitter %.2f", n, step, hi, step, jitter)
	}
	return ""
}

// TestContractRetryAfterBand is W06-19's L2 Retry-After contract: a
// dispatcher refusal for each step of the ladder leaves the /v1 admission
// writer with a Retry-After inside the step's jitter band, and the jitter
// spreads (≥ 2 distinct values over 200 refusals of the 10-s step). The
// ladder itself is the dispatcher's (TestRetryAfterStepsAreWholeSeconds,
// TestExternalRetryAfterHint in internal/dispatch).
func TestContractRetryAfterBand(t *testing.T) {
	const jitter = 0.2
	adm := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: confDispatch{},
		Settings: func() oai.Settings { return oai.Settings{RetryAfterJitter: jitter} }})
	for _, step := range contractRetryAfterSteps {
		distinct := map[string]bool{}
		for range 200 {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			adm.WriteAdmissionError(req.Context(), rec, &dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: time.Duration(step) * time.Second})
			got := rec.Header().Get("Retry-After")
			if msg := retryAfterOutsideBand(got, step, jitter); msg != "" {
				t.Fatalf("step %d s: %s", step, msg)
			}
			distinct[got] = true
		}
		if step == 10 && len(distinct) < 2 {
			t.Errorf("step 10 s, jitter %.2f: one Retry-After value over 200 refusals (%v) — no spread", jitter, distinct)
		}
	}
	// Counterproof: the band check rejects a value outside.
	if retryAfterOutsideBand("4", 2, jitter) == "" || retryAfterOutsideBand("0", 1, jitter) == "" || retryAfterOutsideBand("x", 1, jitter) == "" {
		t.Error("counterproof: the band check accepts a value outside the band")
	}
}

// ciJob returns the block of job id in the workflow text (the lines from
// "  <id>:" up to the next job at the same indentation), "" if absent.
func ciJob(workflow, id string) string {
	lines := strings.Split(workflow, "\n")
	start := -1
	jobLine := regexp.MustCompile(`^  [A-Za-z0-9_-]+:\s*$`)
	for i, l := range lines {
		if start < 0 {
			if strings.TrimRight(l, " ") == "  "+id+":" {
				start = i
			}
			continue
		}
		if jobLine.MatchString(l) {
			return strings.Join(lines[start:i], "\n")
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

// TestContractCIJobs pins the CI wiring of W06-19 (design/06 §4.14,
// masterplan W06-19 "Go-Vertragstests (required) + OpenAI-SDK-Smoke
// (nightly)"): the job "Conformance" runs the contract tests of
// internal/oai and the route census of cmd/ctxd on every pull request, and
// its name is a required check in GOVERNANCE.md; the job
// "gateway-sdk-smoke" runs the Python half (build tag pyconformance) and the
// self-test of the live tools on schedule and dispatch only.
func TestContractCIJobs(t *testing.T) {
	raw, err := os.ReadFile("../../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatalf("ci.yml: %v", err)
	}
	wf := string(raw)
	conf := ciJob(wf, "conformance")
	if conf == "" {
		t.Error(`ci.yml has no job "conformance" — the Go contract is not a CI gate`)
	} else {
		for _, want := range []string{"name: Conformance", "go test", "-run", "^Test(Contract|Conformance)", "./internal/oai/", "./cmd/ctxd/"} {
			if !strings.Contains(conf, want) {
				t.Errorf("job conformance lacks %q", want)
			}
		}
		if strings.Contains(conf, "github.event_name == 'schedule'") {
			t.Error("job conformance is gated on schedule — a required check must run on every pull request")
		}
	}
	smoke := ciJob(wf, "gateway-sdk-smoke")
	if smoke == "" {
		t.Error(`ci.yml has no job "gateway-sdk-smoke" — the nightly SDK smoke is missing`)
	} else {
		for _, want := range []string{"github.event_name == 'schedule'", "workflow_dispatch", "-tags=pyconformance", "selftest_live.py", "uv"} {
			if !strings.Contains(smoke, want) {
				t.Errorf("job gateway-sdk-smoke lacks %q", want)
			}
		}
	}
	gov, err := os.ReadFile("../../../GOVERNANCE.md")
	if err != nil {
		t.Fatalf("GOVERNANCE.md: %v", err)
	}
	i := strings.Index(string(gov), "Required status checks")
	if i < 0 {
		t.Fatal("GOVERNANCE.md has no required-check list")
	}
	list := string(gov)[i:min(len(gov), i+800)]
	if !strings.Contains(list, "`Conformance`") {
		t.Error("GOVERNANCE.md's required-check list does not name `Conformance`")
	}
	if strings.Contains(list, "`gateway-sdk-smoke`") {
		t.Error("GOVERNANCE.md lists the nightly gateway-sdk-smoke as required — nightly jobs are not required")
	}
}

//go:build pyconformance

package oai_test

// Conformance harness, Python half (design/02 §7): the official OpenAI Python
// SDK against the same fake the Go half uses. Local, per wave — CI has no
// Python job (masterplan §5c):
//
//	go test -tags=pyconformance -count=1 -v ./internal/oai/
//
// Needs uv on PATH (it fetches openai==3.42.1, cohere==7.3.1, jsonschema==4.26.0
// and pytest==9.2.0 into its own cache). Without uv the test SKIPS and says why; it never passes
// silently (W21).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// The Python packages the harness pins: openai per design/02 §7; jsonschema
// and pytest at the versions measured when the harness was built (W02-02),
// so a later run resolves the same validator and runner; cohere (the Cohere
// v2 SDK of test_rerank.py) at the current release on PyPI when W02-10 was
// built (7.3.1, uploaded 2026-11-04).
const (
	pythonSDKVersion  = "3.42.1"
	cohereSDKVersion  = "7.3.1"
	jsonschemaVersion = "4.26.0"
	pytestVersion     = "9.2.0"
)

// runPySDK starts the fake for ws and runs the pytest half against it. It
// returns pytest's combined output and its exit error (nil = all passed).
// Next to the error fake it serves the model fixture (conformanceModels,
// W02-05) under CTX_MODELS_BASE_URL for test_models.py and the chat fixture
// (conformanceChat, W02-07) under CTX_CHAT_BASE_URL for test_chat.py — the
// same in both runs, so the house-format run still fails exactly the error
// cases. The meter fixture (conformanceMeter, W06-10) is served under
// CTX_METER_BASE_URL for test_meter_retry.py, the L1 fixture
// (conformanceConcurrency, W06-11) under CTX_CONCURRENCY_BASE_URL for
// test_concurrency_retry.py, both fresh per run. The embeddings fixture
// (conformanceEmbeddings, W02-09) is served under CTX_EMBED_BASE_URL for
// test_embeddings.py, the rerank fixture (conformanceRerank, W02-10) under
// CTX_RERANK_BASE_URL for test_rerank.py.
func runPySDK(t *testing.T, ws writerSet) (string, error) {
	t.Helper()
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skipf("SKIP, not PASS: uv not on PATH (%v) — the Python SDK half cannot run here", err)
	}
	srv := newFakeServer(t, ws)
	models := httptest.NewServer(conformanceModels(t))
	t.Cleanup(models.Close)
	chat := httptest.NewServer(conformanceChat(t))
	t.Cleanup(chat.Close)
	meter := httptest.NewServer(conformanceMeter(t))
	t.Cleanup(meter.Close)
	embeddings := httptest.NewServer(conformanceEmbeddings(t))
	t.Cleanup(embeddings.Close)
	concurrency := httptest.NewServer(conformanceConcurrency(t))
	t.Cleanup(concurrency.Close)
	rerank := httptest.NewServer(conformanceRerank(t))
	t.Cleanup(rerank.Close)
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, uv, "run", "--no-project",
		"--with", "openai=="+pythonSDKVersion, "--with", "cohere=="+cohereSDKVersion,
		"--with", "jsonschema=="+jsonschemaVersion, "--with", "pytest=="+pytestVersion,
		"pytest", "-p", "no:cacheprovider", "-v", "conformance/oai")
	cmd.Dir = moduleRoot
	cmd.Env = append(childEnv(), "OPENAI_BASE_URL="+srv.URL+"/", "CTX_MODELS_BASE_URL="+models.URL+"/v1/",
		"CTX_CHAT_BASE_URL="+chat.URL+"/", "CTX_METER_BASE_URL="+meter.URL+"/", "CTX_EMBED_BASE_URL="+embeddings.URL+"/",
		"CTX_CONCURRENCY_BASE_URL="+concurrency.URL+"/", "CTX_RERANK_BASE_URL="+rerank.URL+"/", "PYTHONDONTWRITEBYTECODE=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
}

// childEnv is the environment of the uv child: the test binary's own, with
// SSL_CERT_FILE as the process started with it — TestMain (wire_tls_test.go)
// points it at the binary's CA, and uv would then trust nothing but that CA
// and fail to fetch its packages ("invalid peer certificate: UnknownIssuer").
func childEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "SSL_CERT_FILE=") {
			env = append(env, kv)
		}
	}
	if sslCertFileSet {
		env = append(env, "SSL_CERT_FILE="+sslCertFile)
	}
	return env
}

// conformanceChat is the chat fixture of test_chat.py: one chat rig per
// answer shape of the live chat chain (cfChatShapes), each under
// /<shape>/v1/chat/completions.
func conformanceChat(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	for _, sh := range cfChatShapes {
		up := newUpstream(t, sh.answer)
		rig := newChatRig(t, []backends.Backend{up.row(sh.name, backends.TrustFull, 1)}, 4, 1)
		mux.Handle("/"+sh.name+"/", http.StripPrefix("/"+sh.name, rig.router))
	}
	return mux
}

// conformanceMeter is the meter fixture of test_meter_retry.py: the chat
// route over the real meter on the real clock, 12 requests per minute — the
// 429 after the burst asks for up to five seconds, so the SDK's request
// still meets an empty bucket under host load (Fremd-Review 1 F-7).
func conformanceMeter(t *testing.T) http.Handler {
	t.Helper()
	up := newUpstream(t, answerSGLang)
	m := keyquota.New(time.Now)
	m.MarkSeeded()
	meter := &rigMeter{m: m, lim: keyquota.Limits{Concurrency: noL1, RPM: 12, TPM: 1 << 30, CompletionReserve: 1024}}
	rig := newChatRig(t, []backends.Backend{up.row("spark", backends.TrustFull, 1)}, 64, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	return rig.router
}

// conformanceEmbeddings is the fixture of test_embeddings.py: the
// embeddings route over a fake engine of native dimension 4096.
func conformanceEmbeddings(t *testing.T) http.Handler {
	t.Helper()
	e := newEmbedEngine(t, 4096)
	return newEmbedRig(t, []backends.Backend{e.row("conf", backends.ProtocolOpenAI, 1)}, 33, 1).router
}

// conformanceRerank is the fixture of test_rerank.py: POST /v1/rerank
// (Jina) and POST /v2/rerank (Cohere v2) over a fake cross-encoder that
// answers the logits of cfRerankDocs (conformance_rerank_test.go).
func conformanceRerank(t *testing.T) http.Handler {
	t.Helper()
	return newCfRerankRig(t).router
}

// conformanceConcurrency is the L1 fixture of test_concurrency_retry.py
// (W06-11): the chat route over the real meter with C = 1. The engine holds
// a request whose message is "hold" until POST /release (at most 30 s), so
// the consumer's one slot stays taken until the test lets it go. GET /held
// answers 204 once that request reached the engine — it holds the slot from
// then on, so the test probes only after it (a probe racing the holder could
// take the one slot first and leave the holder refused) — and 504 after 10 s.
func conformanceConcurrency(t *testing.T) http.Handler {
	t.Helper()
	release, held := make(chan struct{}), make(chan struct{})
	var once, heldOnce sync.Once
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte(`"hold"`)) {
			heldOnce.Do(func() { close(held) })
			select {
			case <-release:
			case <-time.After(30 * time.Second):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answerSGLang)
	}))
	t.Cleanup(engine.Close)
	m := keyquota.New(time.Now)
	m.MarkSeeded()
	meter := &rigMeter{m: m, lim: keyquota.Limits{RPM: 1000, TPM: 1 << 30, CompletionReserve: 1024, Concurrency: 1}}
	up := &upstream{srv: engine} // for its row
	rig := newChatRig(t, []backends.Backend{up.row("spark", backends.TrustFull, 1)}, 64, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	mux := http.NewServeMux()
	mux.Handle("/v1/", rig.router)
	mux.HandleFunc("GET /held", func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-held:
			w.WriteHeader(http.StatusNoContent)
		case <-time.After(10 * time.Second):
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	})
	mux.HandleFunc("POST /release", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(release) })
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// TestConformancePythonSDK: every case parses in the Python SDK with the
// mapped exception class and code, x-should-retry stops its retries, and
// every body is a valid ErrorResponse.
func TestConformancePythonSDK(t *testing.T) {
	out, err := runPySDK(t, oaiWriters())
	t.Log("\n" + out)
	if err != nil {
		t.Fatalf("Python SDK half failed: %v", err)
	}
}

// TestConformancePythonSDKRejectsHouseFormat: the red side, kept — the same
// pytest run against the pre-W02-02 house forms must fail every case.
func TestConformancePythonSDKRejectsHouseFormat(t *testing.T) {
	out, err := runPySDK(t, houseWriters())
	t.Log("\n" + out)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("pytest against the house forms: %v, want exit status 1 (tests failed)", err)
	}
	want := fmt.Sprintf("%d failed", len(sdkCases))
	if !strings.Contains(out, want) {
		t.Errorf("pytest summary lacks %q — a house form passed the Python harness", want)
	}
}

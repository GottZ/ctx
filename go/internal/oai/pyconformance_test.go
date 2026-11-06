//go:build pyconformance

package oai_test

// Conformance harness, Python half (design/02 §7): the official OpenAI Python
// SDK against the same fake the Go half uses. Local, per wave — CI has no
// Python job (masterplan §5c):
//
//	go test -tags=pyconformance -count=1 -v ./internal/oai/
//
// Needs uv on PATH (it fetches openai==3.42.1, jsonschema==4.26.0 and
// pytest==9.2.0 into its own cache). Without uv the test SKIPS and says why; it never passes
// silently (W21).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// The Python packages the harness pins: openai per design/02 §7; jsonschema
// and pytest at the versions measured when the harness was built (W02-02),
// so a later run resolves the same validator and runner.
const (
	pythonSDKVersion  = "3.42.1"
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
// CTX_METER_BASE_URL for test_meter_retry.py, fresh per run.
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
	moduleRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, uv, "run", "--no-project",
		"--with", "openai=="+pythonSDKVersion, "--with", "jsonschema=="+jsonschemaVersion, "--with", "pytest=="+pytestVersion,
		"pytest", "-p", "no:cacheprovider", "-v", "conformance/oai")
	cmd.Dir = moduleRoot
	cmd.Env = append(childEnv(), "OPENAI_BASE_URL="+srv.URL+"/", "CTX_MODELS_BASE_URL="+models.URL+"/v1/",
		"CTX_CHAT_BASE_URL="+chat.URL+"/", "CTX_METER_BASE_URL="+meter.URL+"/", "PYTHONDONTWRITEBYTECODE=1")
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
	meter := &rigMeter{m: m, lim: keyquota.Limits{RPM: 12, TPM: 1 << 30, CompletionReserve: 1024}}
	rig := newChatRig(t, []backends.Backend{up.row("spark", backends.TrustFull, 1)}, 64, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	return rig.router
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

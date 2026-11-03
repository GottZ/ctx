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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
func runPySDK(t *testing.T, ws writerSet) (string, error) {
	t.Helper()
	uv, err := exec.LookPath("uv")
	if err != nil {
		t.Skipf("SKIP, not PASS: uv not on PATH (%v) — the Python SDK half cannot run here", err)
	}
	srv := newFakeServer(t, ws)
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
	cmd.Env = append(os.Environ(), "OPENAI_BASE_URL="+srv.URL+"/", "PYTHONDONTWRITEBYTECODE=1")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err = cmd.Run()
	return out.String(), err
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

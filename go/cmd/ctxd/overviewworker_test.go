package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunOverviewWorker_BrokenOptionsJSON is the E-A negative gate on the
// process side: broken options JSON exits ≠0 with NOTHING on stdout — and the
// stderr prefix proves the decode failed BEFORE config/pool ever ran (the
// structural "no DB mutation" guarantee: no connection exists at that point).
func TestRunOverviewWorker_BrokenOptionsJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runOverviewWorker(strings.NewReader(`{"Resolution": broken`), &stdout, &stderr)
	if code == 0 {
		t.Fatal("broken options JSON exited 0")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout must stay empty on failure (the parent would try to decode it): %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "overview-worker: options:") {
		t.Fatalf("failure did not originate in the options decode (pre-DB): %q", stderr.String())
	}
}

// TestRunOverviewWorker_EmptyStdin — same gate for a spawner that dies before
// writing (EOF on stdin): exit ≠0 at the decode step, no DB touch.
func TestRunOverviewWorker_EmptyStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runOverviewWorker(strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatal("empty stdin exited 0")
	}
	if !strings.Contains(stderr.String(), "overview-worker: options:") {
		t.Fatalf("failure did not originate in the options decode (pre-DB): %q", stderr.String())
	}
}

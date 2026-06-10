package backends

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Synthetic fixture values only (RFC-2606 host, made-up key) — never real
// deployment topology or credentials in a public repo.
const (
	leakKey  = "sk-test-0123456789abcdefghijklmnopqrstuv"
	testHost = "http://backend.example:8089"
)

func testBackend() Backend {
	return Backend{
		Host:     testHost,
		APIKey:   leakKey,
		Protocol: ProtocolOpenAI,
		Model:    "test-model-27b",
		NumCtx:   98304,
		Think:    "false",
		Timeout:  420 * time.Second,
	}
}

// TestBackendSecretLogHygiene proves the APIKey survives none of the four
// serialization paths: slog (LogValue), %v/%+v/%s (String), %#v (GoString —
// fmt uses GoStringer there, not Stringer), and encoding/json (json:"-" —
// encoding/json ignores both Stringer interfaces). Red-proof procedure: remove
// LogValue/String/GoString and the json:"-" tag — every subtest fails.
func TestBackendSecretLogHygiene(t *testing.T) {
	b := testBackend()

	outputs := map[string]string{
		"fmt %v":  fmt.Sprintf("%v", b),
		"fmt %+v": fmt.Sprintf("%+v", b),
		"fmt %s":  fmt.Sprintf("%s", b), //nolint:staticcheck // the %s fmt path itself is under test
		"fmt %#v": fmt.Sprintf("%#v", b),
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("fallback engaged", "backend", b)
	outputs["slog JSON"] = buf.String()

	jsonBytes, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	outputs["json.Marshal"] = string(jsonBytes)

	// Pointer receiver path: slog resolves LogValuer on the value it gets.
	var ptrBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&ptrBuf, nil)).Info("ptr", "backend", &b)
	outputs["slog JSON (ptr)"] = ptrBuf.String()

	for path, out := range outputs {
		if strings.Contains(out, leakKey) {
			t.Errorf("%s leaks the APIKey: %s", path, out)
		}
		// Positive control: the output is not vacuously empty — non-secret
		// fields must be present, otherwise the leak assertion proves nothing.
		if !strings.Contains(out, "backend.example") {
			t.Errorf("%s lost the host (masking too broad?): %s", path, out)
		}
	}

	// Presence indicator: set key renders as "set", never raw.
	if !strings.Contains(outputs["fmt %+v"], `APIKey:"set"`) {
		t.Errorf("String() should render a set APIKey as \"set\": %s", outputs["fmt %+v"])
	}
	empty := testBackend()
	empty.APIKey = ""
	if !strings.Contains(empty.String(), `APIKey:""`) {
		t.Errorf("String() should render an empty APIKey as empty: %s", empty.String())
	}
}

func TestThinkModePtr(t *testing.T) {
	if v := ThinkMode("true").Ptr(); v == nil || !*v {
		t.Errorf("ThinkMode(true).Ptr() = %v, want &true", v)
	}
	if v := ThinkMode("false").Ptr(); v == nil || *v {
		t.Errorf("ThinkMode(false).Ptr() = %v, want &false", v)
	}
	if v := ThinkMode("").Ptr(); v != nil {
		t.Errorf("ThinkMode(\"\").Ptr() = %v, want nil (omit)", v)
	}
	if v := ThinkMode("maybe").Ptr(); v != nil {
		t.Errorf("ThinkMode(maybe).Ptr() = %v, want nil (matches legacy parseThinkMode)", v)
	}

	// Fresh allocation per call — no shared *bool between config generations.
	a, b := ThinkMode("true").Ptr(), ThinkMode("true").Ptr()
	if a == b {
		t.Error("Ptr() must allocate fresh, got the same pointer twice")
	}
}

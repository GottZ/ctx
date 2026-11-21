package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// recordedServer is a ctxd stand-in for the CLI tests of issue #408: it answers
// every request with the body answer returns and keeps each request body, so a
// test can state both what the command sent and that it sent nothing.
type recordedServer struct {
	mu     sync.Mutex
	bodies []map[string]any
}

// requests returns the decoded bodies received so far.
func (s *recordedServer) requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.bodies...)
}

// newRecordedServer starts the stand-in and points the CLI config at it
// (CTX_BASE_URL/CTX_KEY, a private HOME, a scratch working directory).
func newRecordedServer(t *testing.T, answer func(body map[string]any) string) *recordedServer {
	t.Helper()
	rs := &recordedServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		rs.mu.Lock()
		rs.bodies = append(rs.bodies, body)
		rs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer(body)))
	}))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home+"/.config")
	t.Setenv("CTX_BASE_URL", srv.URL)
	t.Setenv("CTX_KEY", "fixture-key")
	t.Chdir(t.TempDir())
	return rs
}

// runCLI executes one argv on a fresh command tree with stdin taken from the
// given text (nil: stdin is left as it is) and returns stdout, stderr and the
// command error. stdout and stderr are files, so StdoutIsTTY() is false: the
// command takes the branch a pipe sees.
func runCLI(t *testing.T, stdin *string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	outFile, ferr := os.Create(dir + "/stdout.txt")
	if ferr != nil {
		t.Fatalf("temp stdout: %v", ferr)
	}
	defer func() { _ = outFile.Close() }()
	errFile, ferr := os.Create(dir + "/stderr.txt")
	if ferr != nil {
		t.Fatalf("temp stderr: %v", ferr)
	}
	defer func() { _ = errFile.Close() }()

	oldIn, oldOut, oldErr := os.Stdin, os.Stdout, os.Stderr
	if stdin != nil {
		if werr := os.WriteFile(dir+"/stdin.txt", []byte(*stdin), 0o600); werr != nil {
			t.Fatalf("temp stdin: %v", werr)
		}
		inFile, oerr := os.Open(dir + "/stdin.txt")
		if oerr != nil {
			t.Fatalf("open stdin: %v", oerr)
		}
		defer func() { _ = inFile.Close() }()
		os.Stdin = inFile
	}
	os.Stdout, os.Stderr = outFile, errFile
	defer func() { os.Stdin, os.Stdout, os.Stderr = oldIn, oldOut, oldErr }()

	root := &cobra.Command{Use: "ctx", Short: "Your AI's save game"}
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SetOut(outFile)
	root.SetErr(errFile)
	RegisterCommands(root)
	root.SetArgs(args)
	err = root.Execute()

	o, rerr := os.ReadFile(dir + "/stdout.txt")
	if rerr != nil {
		t.Fatalf("reading stdout: %v", rerr)
	}
	e, rerr := os.ReadFile(dir + "/stderr.txt")
	if rerr != nil {
		t.Fatalf("reading stderr: %v", rerr)
	}
	return string(o), string(e), err
}

// canonicalJSON re-encodes v with sorted object keys, so two documents compare
// by content and not by the encoder's key order.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	b, _ = json.Marshal(back)
	return string(b)
}

func strPtr(s string) *string { return &s }

func jsonUnmarshalString(s string, v any) error { return json.Unmarshal([]byte(s), v) }

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/httpx"
)

// sealLogBuffer captures the process's log lines for the test's duration.
type sealLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *sealLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *sealLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestRecordingTranslateSealsBackendAnswers (issue #575): a backend whose
// error answer quotes what it was sent — a validation error echoing the
// prompt — puts nothing of it into the call's error or into a log line of
// the walk: the error is the attempt's class, status and the code "sealed".
// The walk fails over exactly as on the unsealed error: a refused connection
// (transport) and a 503 (gateway) go on to the next row, a 500 stops.
func TestRecordingTranslateSealsBackendAnswers(t *testing.T) {
	const secret = "password=Tr0ub4dor-seal-575"
	var hits [3]atomic.Int64
	echo := func(i, status int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i].Add(1)
			raw, _ := io.ReadAll(r.Body)
			var req struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			_ = json.Unmarshal(raw, &req)
			var user string
			if len(req.Messages) == 2 {
				user = req.Messages[1].Content
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rejected input: " + user}})
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()
	row := func(name, host string, priority int) backends.Backend {
		r := recordingRow(name, backends.TrustFull, backends.LocalityLocal, 32768)
		r.Host, r.Priority = host, priority
		return r
	}
	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest([]backends.Backend{
		row("gone", goneURL, 40),
		row("busy", echo(0, http.StatusServiceUnavailable), 30),
		row("fault", echo(1, http.StatusInternalServerError), 20),
		row("after", echo(2, http.StatusOK), 10),
	})

	logs := &sealLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	_, err := TranslateRecordingBlock(testPrincipalCtx(), RecordingTranslateCall{Pool: pool, Admission: testAdmission(t, dispatch.ClassInteractive)},
		RecordingTranslateInput{Address: "request/messages/1", Kind: "text", Role: "user", Text: secret + " — bitte übersetzen", Chunks: 1, Language: "de"})
	if err == nil {
		t.Fatal("a chain whose rows all fail answered")
	}
	if h := [3]int64{hits[0].Load(), hits[1].Load(), hits[2].Load()}; h != [3]int64{1, 1, 0} {
		t.Errorf("attempts per row (busy, fault, after): %v, want [1 1 0] — the walk goes past a refused connection and a 503 and stops at a 500", h)
	}
	if got := backends.Classify(err, ""); got != backends.ClassServerFault {
		t.Errorf("the call's error classifies as %s, want server_fault (the 500 that ended the walk)", got)
	}
	if strings.Contains(err.Error(), "Tr0ub4dor") || err.Error() != "server_fault 500 sealed" {
		t.Errorf("the call's error: %q, want \"server_fault 500 sealed\" — nothing of the backend's answer", err.Error())
	}
	out := logs.String()
	if strings.Contains(out, "Tr0ub4dor") || strings.Contains(out, "rejected input") {
		t.Errorf("a log line carries the backend's answer:\n%s", out)
	}
	for _, class := range []string{`"class":"transport"`, `"class":"gateway"`, `"class":"server_fault"`} {
		if !strings.Contains(out, class) {
			t.Errorf("the walk's log has no attempt of %s:\n%s", class, out)
		}
	}
}

// TestSealedErrorKeepsWhatTheWalkReads: a sealed attempt error keeps what
// the walk and the caller read of it — the class, the status, the
// Retry-After that sets the row's cooldown, a context error — and nothing of
// the backend's body.
func TestSealedErrorKeepsWhatTheWalkReads(t *testing.T) {
	orig := fmt.Errorf("llm: %w", &httpx.StatusError{Code: http.StatusTooManyRequests, Body: "slow down: password=Tr0ub4dor", RetryAfter: 240 * time.Second})
	err := sealWireError(orig, "")
	if got := retryAfterOf(err); got != 240*time.Second {
		t.Errorf("Retry-After of the sealed 429: %s, want 4m0s (the cooldown reads it)", got)
	}
	if got := backends.Classify(err, ""); got != backends.ClassRateLimit {
		t.Errorf("class: %s, want rate_limit", got)
	}
	if err.Error() != "rate_limit 429 sealed" {
		t.Errorf("text: %q", err.Error())
	}
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.Body != "" || se.Code != http.StatusTooManyRequests {
		t.Errorf("the status error the sealed one reaches: %+v, want status 429 and no body", se)
	}
	deadline := sealWireError(fmt.Errorf("llm: request failed: %w", context.DeadlineExceeded), "")
	if !errors.Is(deadline, context.DeadlineExceeded) || backends.Classify(deadline, "") != backends.ClassTimeout {
		t.Errorf("a sealed deadline: %v (%s), want a timeout that is still context.DeadlineExceeded", deadline, backends.Classify(deadline, ""))
	}
}

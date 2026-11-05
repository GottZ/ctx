package handler

// Review-2 gates of W01-7 (reports/review/W01-7-fremdreview-2.md): a valid
// ConsumerAuth without a consumer id is poisoned, not ctx' own work (FR2-1),
// and the two ERROR lines of RequestPrincipal are pinned at their level
// (FR2-2, mutants M32/M34).

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// levelLines records every record at or above ERROR.
type levelLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *levelLines) Enabled(context.Context, slog.Level) bool { return true }
func (l *levelLines) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		l.mu.Lock()
		l.lines = append(l.lines, r.Message)
		l.mu.Unlock()
	}
	return nil
}
func (l *levelLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *levelLines) WithGroup(string) slog.Handler      { return l }

func (l *levelLines) matching(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.lines {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// captureDefaultLog swaps slog's default logger for the test (not parallel).
func captureDefaultLog(t *testing.T) *levelLines {
	t.Helper()
	rec := &levelLines{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return rec
}

// FR2-1: a ConsumerAuth that is valid but names no consumer is no credential
// AuthenticateModelKey produces (consumer_id is NOT NULL); it must not read
// as ctx' own detached work — the zero principal, which B8 downgrades and
// enqueues — but as a poisoned principal both doors refuse.
func TestRequestPrincipalValidConsumerWithoutIDIsConflict(t *testing.T) {
	ca := testConsumerAuth()
	ca.ConsumerID = ""
	p := RequestPrincipal(withConsumerAuth(context.Background(), ca))
	if !p.Conflict {
		t.Fatalf("valid ConsumerAuth without consumer id: principal = %+v, want Conflict (not the zero principal)", p)
	}
}

// FR2-2: the two ERROR lines of RequestPrincipal (both credentials; an
// invalid or id-less model credential) are the forensics of a wiring defect —
// exactly one line each, at ERROR.
func TestRequestPrincipalLogsConflictsAtError(t *testing.T) {
	rec := captureDefaultLog(t)
	ar := &auth.AuthResult{ApiKeyID: "key-1", TenantID: "tenant-1", HomeScope: "scope-1"}

	_ = RequestPrincipal(withConsumerAuth(withAuthResult(context.Background(), ar), testConsumerAuth()))
	if n := rec.matching("both a knowledge and a model credential"); n != 1 {
		t.Errorf("both credentials: %d ERROR line(s) naming the conflict, want 1 (all: %q)", n, rec.lines)
	}

	invalid := testConsumerAuth()
	invalid.IsValid = false
	_ = RequestPrincipal(withConsumerAuth(context.Background(), invalid))
	if n := rec.matching("invalid model credential"); n != 1 {
		t.Errorf("invalid ConsumerAuth: %d ERROR line(s) naming it, want 1 (all: %q)", n, rec.lines)
	}

	// The knowledge and the consumer branch log nothing.
	before := len(rec.lines)
	_ = RequestPrincipal(withAuthResult(context.Background(), ar))
	_ = RequestPrincipal(withConsumerAuth(context.Background(), testConsumerAuth()))
	if got := len(rec.lines) - before; got != 0 {
		t.Errorf("knowledge/consumer branch: %d ERROR line(s), want 0", got)
	}
}

package events

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestListenerLogsItsOwnShutdownBelowError (issue #495): the listener the
// scheduler builds logs through pgxlisten's LogError. At a normal SIGTERM
// the scheduler cancels the listener's context, and pgxlisten reports the
// interrupted wait once more — that is the stop, not a fault, and deploy.sh
// counts every ERROR line after a deploy. An error while the listener runs
// stays ERROR, a context.Canceled from somewhere else (a handler's own
// context) included, and so does an error that is not the cancellation
// although the context has ended.
func TestListenerLogsItsOwnShutdownBelowError(t *testing.T) {
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	l := NewPgxlistenListener("postgres://unused", 0, &Scheduler{}, nil, nil, nil, nil, nil)
	if l.LogError == nil {
		t.Fatal("the listener has no LogError — pgxlisten would drop every error silently")
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()

	for _, c := range []struct {
		name      string
		ctx       context.Context
		err       error
		wantError bool
	}{
		{"shutdown: the interrupted wait", stopped, fmt.Errorf("waiting for notification: %w", context.Canceled), false},
		{"running: a broken connection", live, fmt.Errorf("waiting for notification: %w", io.ErrUnexpectedEOF), true},
		{"running: a handler's own context canceled", live, fmt.Errorf("handle ctx_settings_write notification: %w", context.Canceled), true},
		{"stopped, but no cancellation", stopped, fmt.Errorf("connect: %w", errors.New("connection refused")), true},
	} {
		before := len(logs.String())
		l.LogError(c.ctx, c.err)
		line := logs.String()[before:]
		if got := strings.Contains(line, "level=ERROR"); got != c.wantError {
			t.Errorf("%s: logged %q, want an ERROR line = %v", c.name, strings.TrimSpace(line), c.wantError)
		}
		if strings.TrimSpace(line) == "" {
			t.Errorf("%s: nothing logged — the error must leave a line at some level", c.name)
		}
	}
}

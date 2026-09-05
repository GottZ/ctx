package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// pool.go has no pure functions — NewPool and sleepCtx both depend on
// external resources (pgxpool connection, timer channels).
//
// sleepCtx is a thin wrapper around select{} and could be tested with
// a cancelled context, but it is not exported.

// TestLogPgNoticeSeverityIsLocaleIndependent pins the one property that
// cannot be observed on the English-locale servers the suite runs against:
// the slog LEVEL follows SeverityUnlocalized, the protocol-fixed English
// field, not Severity, which the server translates via lc_messages.
//
// A German-locale Postgres sends Severity="WARNUNG" for exactly the notices
// this handler exists to carry (the migration RAISE traffic, 092/094/115/133)
// — under a switch on the localized field they all fell through to Info, a
// silent downgrade of every warning on every non-English deployment.
//
// Mutation probe: switch on n.Severity again and the localized-warning row
// goes red (level INFO instead of WARN).
func TestLogPgNoticeSeverityIsLocaleIndependent(t *testing.T) {
	cases := []struct {
		name        string
		notice      *pgconn.Notice
		wantLevel   string
		wantSevAttr string
	}{
		{
			name:        "localized warning still warns",
			notice:      &pgconn.Notice{Severity: "WARNUNG", SeverityUnlocalized: "WARNING", Message: "verwaiste Zeile", Code: "01000"},
			wantLevel:   "WARN",
			wantSevAttr: "WARNING",
		},
		{
			name:        "localized notice still informs",
			notice:      &pgconn.Notice{Severity: "HINWEIS", SeverityUnlocalized: "NOTICE", Message: "Relation existiert bereits", Code: "42P07"},
			wantLevel:   "INFO",
			wantSevAttr: "NOTICE",
		},
		{
			name:        "english server is unchanged",
			notice:      &pgconn.Notice{Severity: "WARNING", SeverityUnlocalized: "WARNING", Message: "dangling row", Code: "01000"},
			wantLevel:   "WARN",
			wantSevAttr: "WARNING",
		},
		{
			// Pre-9.6 servers omit the unlocalized field entirely; the
			// localized one is then all there is.
			name:        "missing unlocalized field falls back",
			notice:      &pgconn.Notice{Severity: "WARNING", Message: "legacy server", Code: "01000"},
			wantLevel:   "WARN",
			wantSevAttr: "WARNING",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			logPgNotice(nil, c.notice)

			out := buf.String()
			if !strings.Contains(out, "level="+c.wantLevel) {
				t.Errorf("log = %q, want level=%s", out, c.wantLevel)
			}
			if !strings.Contains(out, "severity="+c.wantSevAttr) {
				t.Errorf("log = %q, want severity=%s (the untranslated value, so log filters are locale-independent too)", out, c.wantSevAttr)
			}
			if !strings.Contains(out, c.notice.Message) {
				t.Errorf("log = %q, want the server message %q", out, c.notice.Message)
			}
		})
	}

	// A nil notice must not panic in pgx's connection reader.
	logPgNotice(nil, nil)
}

func TestSleepCtx_CancelledContext(t *testing.T) {
	// sleepCtx is unexported but we can test it indirectly since we're
	// in the same package.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	err := sleepCtx(ctx, 10*time.Second)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestSleepCtx_ShortDuration(t *testing.T) {
	ctx := context.Background()
	start := time.Now()
	err := sleepCtx(ctx, 1*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("expected nil error for completed sleep, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("sleep took too long: %v", elapsed)
	}
}

func TestSleepCtx_ZeroDuration(t *testing.T) {
	ctx := context.Background()
	err := sleepCtx(ctx, 0)
	if err != nil {
		t.Errorf("expected nil error for zero-duration sleep, got %v", err)
	}
}

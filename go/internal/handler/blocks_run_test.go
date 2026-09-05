package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/events"
)

// auditControllerStub is the AuditController double: the two Start entry
// points record what they were handed and play back a scripted error. The
// status calls exist for the interface only — a test that reaches them would
// need a live pool.
type auditControllerStub struct {
	err    error
	family string
	dryRun bool
	limit  int
	calls  int
}

func (c *auditControllerStub) StartSensitivityAudit(dryRun bool, limit int) error {
	c.calls, c.family, c.dryRun, c.limit = c.calls+1, "audit", dryRun, limit
	return c.err
}

func (c *auditControllerStub) StartCredentialsClassify(dryRun bool, limit int) error {
	c.calls, c.family, c.dryRun, c.limit = c.calls+1, "classify", dryRun, limit
	return c.err
}

func (c *auditControllerStub) SensitivityAuditStatus() events.AuditStatus { return events.AuditStatus{} }

func (c *auditControllerStub) CredentialsClassifyStatus() events.ClassifyStatus {
	return events.ClassifyStatus{}
}

// logSink captures slog messages verbatim — the two start log lines are wire
// prose too (an operator greps them), so they are pinned like the bodies.
type logSink struct{ msgs []string }

func (l *logSink) Enabled(context.Context, slog.Level) bool { return true }

func (l *logSink) Handle(_ context.Context, r slog.Record) error {
	l.msgs = append(l.msgs, r.Level.String()+" "+r.Message)
	return nil
}

func (l *logSink) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logSink) WithGroup(string) slog.Handler      { return l }

// startResult is one recorded run of a blocks-* start action.
type startResult struct {
	code int
	body struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	logs []string
}

// runBlocksStart drives one start handler with the given payload and
// controller (nil ⇒ no scheduler wired) and returns status, body and log.
// The success path renders the family status, which needs a pool and a config
// store this unit test has not got — the render panic is recovered on
// purpose: the log line under test is written BEFORE the render, and the
// render itself is the subject of the status tests, not of this one.
func runBlocksStart(call func(*ManageHandler, http.ResponseWriter, *http.Request, manageRequest),
	ctl AuditController, data string) startResult {
	sink := &logSink{}
	old := slog.Default()
	slog.SetDefault(slog.New(sink))
	defer slog.SetDefault(old)

	h := &ManageHandler{}
	if ctl != nil {
		h.auditController = ctl
	}
	rec := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		call(h, rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil),
			manageRequest{Action: "test", Data: json.RawMessage(data)})
	}()

	res := startResult{code: rec.Code, logs: sink.msgs}
	_ = json.Unmarshal(rec.Body.Bytes(), &res.body)
	return res
}

// TestBlocksRunStartContract pins both blocks-* start actions against the body
// they share since T03-5 (design 03 §4.5). The per-family prose is DATA now
// (blocksRunSpec), so a swapped spec field — the classify busy message on the
// audit action, say — would ship a wrong answer that compiles and passes a
// smoke test. Every string below is the byte-exact wire text of the two
// pre-T03-5 handlers.
func TestBlocksRunStartContract(t *testing.T) {
	fams := []struct {
		name    string
		call    func(*ManageHandler, http.ResponseWriter, *http.Request, manageRequest)
		running error
		busyMsg string
		failMsg string
		errLog  string
		infoLog string
	}{
		{
			name:    "audit",
			call:    (*ManageHandler).handleBlocksAuditStart,
			running: events.ErrAuditRunning,
			busyMsg: "Sensitivity audit already running",
			failMsg: "Failed to start audit",
			errLog:  "ERROR manage: blocks-audit-start failed",
			infoLog: "INFO manage: sensitivity audit started",
		},
		{
			name:    "classify",
			call:    (*ManageHandler).handleBlocksClassifyStart,
			running: events.ErrClassifyRunning,
			busyMsg: "Credentials classify already running",
			failMsg: "Failed to start classify",
			errLog:  "ERROR manage: blocks-classify-start failed",
			infoLog: "INFO manage: credentials classify started",
		},
	}

	for _, f := range fams {
		t.Run(f.name, func(t *testing.T) {
			t.Run("no scheduler answers 503", func(t *testing.T) {
				got := runBlocksStart(f.call, nil, `{"dry_run":true,"limit":30}`)
				if got.code != http.StatusServiceUnavailable || got.body.Error != "Scheduler not enabled" {
					t.Fatalf("status=%d error=%q, want 503/Scheduler not enabled", got.code, got.body.Error)
				}
			})

			t.Run("unreadable payload answers 400", func(t *testing.T) {
				ctl := &auditControllerStub{}
				got := runBlocksStart(f.call, ctl, `{"dry_run":`)
				want := "Invalid data: expected {\"dry_run\":bool,\"limit\":int}"
				if got.code != http.StatusBadRequest || got.body.Error != want {
					t.Fatalf("status=%d error=%q, want 400/%q", got.code, got.body.Error, want)
				}
				if ctl.calls != 0 {
					t.Fatalf("controller called %d times on a rejected payload", ctl.calls)
				}
			})

			t.Run("negative limit answers 422", func(t *testing.T) {
				ctl := &auditControllerStub{}
				got := runBlocksStart(f.call, ctl, `{"limit":-1}`)
				if got.code != http.StatusUnprocessableEntity || got.body.Error != "limit must be >= 0" {
					t.Fatalf("status=%d error=%q, want 422/limit must be >= 0", got.code, got.body.Error)
				}
				if ctl.calls != 0 {
					t.Fatalf("controller called %d times on a rejected limit", ctl.calls)
				}
			})

			t.Run("already running answers 409 with the family text", func(t *testing.T) {
				for _, err := range []error{f.running, fmt.Errorf("wrapped: %w", f.running)} {
					got := runBlocksStart(f.call, &auditControllerStub{err: err}, `{}`)
					if got.code != http.StatusConflict || got.body.Error != f.busyMsg {
						t.Fatalf("status=%d error=%q, want 409/%q (err=%v)", got.code, got.body.Error, f.busyMsg, err)
					}
					if len(got.logs) != 0 {
						t.Fatalf("logs=%v, want none — a busy run is not a server error", got.logs)
					}
				}
			})

			t.Run("other errors answer 500 and log the action", func(t *testing.T) {
				got := runBlocksStart(f.call, &auditControllerStub{err: errors.New("boom")}, `{}`)
				if got.code != http.StatusInternalServerError || got.body.Error != f.failMsg {
					t.Fatalf("status=%d error=%q, want 500/%q", got.code, got.body.Error, f.failMsg)
				}
				if len(got.logs) != 1 || got.logs[0] != f.errLog {
					t.Fatalf("logs=%v, want [%q]", got.logs, f.errLog)
				}
			})

			t.Run("a started run logs the family noun and passes the params", func(t *testing.T) {
				ctl := &auditControllerStub{}
				got := runBlocksStart(f.call, ctl, `{"dry_run":true,"limit":7}`)
				if len(got.logs) != 1 || got.logs[0] != f.infoLog {
					t.Fatalf("logs=%v, want [%q]", got.logs, f.infoLog)
				}
				if ctl.calls != 1 || ctl.family != f.name || !ctl.dryRun || ctl.limit != 7 {
					t.Fatalf("controller=%+v, want one %s call with dry_run=true limit=7", ctl, f.name)
				}
			})

			t.Run("an absent payload starts a full live run", func(t *testing.T) {
				for _, data := range []string{``, `null`} {
					ctl := &auditControllerStub{}
					runBlocksStart(f.call, ctl, data)
					if ctl.calls != 1 || ctl.dryRun || ctl.limit != 0 {
						t.Fatalf("controller=%+v on data=%q, want one call with dry_run=false limit=0", ctl, data)
					}
				}
			})
		})
	}
}

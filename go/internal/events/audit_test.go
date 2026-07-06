package events

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditTestScheduler builds a scheduler whose classify seam is scripted per
// question (dreamCycleFunc pattern). dry-run probes never touch pool/cfg, so
// nil is fine — a verdict write against nil would panic, which is exactly
// what the dry-run contract forbids.
func auditTestScheduler(answers map[string]struct {
	val bool
	err error
}) *Scheduler {
	s := NewScheduler(nil, nil, nil, StartupConfig{})
	s.classify = func(_ context.Context, _ *pgxpool.Pool, _ *backends.Pool, _ backends.GamingState,
		question, _, _, _ string, _ llm.Admission) (bool, error) {
		a, ok := answers[question]
		if !ok {
			return false, fmt.Errorf("unexpected question: %s", question)
		}
		return a.val, a.err
	}
	return s
}

type qa = struct {
	val bool
	err error
}

func auditBlockFixture() store.AuditBlock {
	return store.AuditBlock{ID: "0190-test", Title: "t", Content: "c"}
}

// TestAuditDecisionTable pins the G41 verdict table: credentials-ja keeps
// credentials (and never asks the personal question), nein+ja goes personal,
// nein×2 goes internal, a parse failure is no verdict, and a chain failure
// aborts the RUN instead of producing a verdict. public never appears.
func TestAuditDecisionTable(t *testing.T) {
	parseErr := fmt.Errorf("%w: prose", llm.ErrClassifyParse)
	chainErr := errors.New("llm: chain exhausted for role \"classify\"")

	cases := []struct {
		name        string
		answers     map[string]qa
		wantVerdict string
		wantAbort   bool
	}{
		{"credentials ja", map[string]qa{
			llm.QuestionCredentials: {val: true},
		}, "credentials", false},
		{"nein dann personal ja", map[string]qa{
			llm.QuestionCredentials: {val: false},
			llm.QuestionPersonal:    {val: true},
		}, "personal", false},
		{"nein mal zwei", map[string]qa{
			llm.QuestionCredentials: {val: false},
			llm.QuestionPersonal:    {val: false},
		}, "internal", false},
		{"parse failure q1", map[string]qa{
			llm.QuestionCredentials: {err: parseErr},
		}, "no-verdict", false},
		{"parse failure q2", map[string]qa{
			llm.QuestionCredentials: {val: false},
			llm.QuestionPersonal:    {err: parseErr},
		}, "no-verdict", false},
		{"chain failure q1 aborts", map[string]qa{
			llm.QuestionCredentials: {err: chainErr},
		}, "", true},
		{"chain failure q2 aborts", map[string]qa{
			llm.QuestionCredentials: {val: false},
			llm.QuestionPersonal:    {err: chainErr},
		}, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := auditTestScheduler(tc.answers)
			blk := auditBlockFixture()
			sample, abort := s.auditOneBlock(context.Background(), blk, backends.GamingState{}, true)
			if abort != tc.wantAbort {
				t.Fatalf("abort = %v, want %v", abort, tc.wantAbort)
			}
			if tc.wantAbort {
				st := s.SensitivityAuditStatus()
				if !st.Aborted || st.LastError == "" {
					t.Fatalf("aborted run not recorded in status: %+v", st)
				}
				return
			}
			if sample.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q", sample.Verdict, tc.wantVerdict)
			}
			if sample.Verdict == "public" {
				t.Fatal("audit produced a public verdict — public is manual-only")
			}
		})
	}
}

// TestAuditCredentialsSkipsPersonalQuestion: a credentials-ja must not spend
// a second GPU call — and must not let a personal answer overwrite the
// dominant verdict.
func TestAuditCredentialsSkipsPersonalQuestion(t *testing.T) {
	s := auditTestScheduler(map[string]qa{
		llm.QuestionCredentials: {val: true},
		llm.QuestionPersonal:    {err: errors.New("must not be asked")},
	})
	sample, abort := s.auditOneBlock(context.Background(), auditBlockFixture(), backends.GamingState{}, true)
	if abort {
		t.Fatal("credentials-ja aborted — the personal question must be skipped, not asked")
	}
	if sample.Verdict != "credentials" {
		t.Fatalf("verdict = %q, want credentials", sample.Verdict)
	}
	if sample.Personal != nil {
		t.Fatal("personal answer recorded although the question must be skipped")
	}
}

// TestStartSensitivityAuditConcurrentGuard: a second start while one run is
// in flight is ErrAuditRunning, never a second goroutine.
func TestStartSensitivityAuditConcurrentGuard(t *testing.T) {
	s := NewScheduler(nil, nil, nil, StartupConfig{})
	s.audit.mu.Lock()
	s.audit.running = true
	s.audit.mu.Unlock()

	if err := s.StartSensitivityAudit(false, 0); !errors.Is(err, ErrAuditRunning) {
		t.Fatalf("second start = %v, want ErrAuditRunning", err)
	}
}

// TestAuditStatusCopiesSamples: the status getter hands out a copy — a
// caller mutating the slice must not reach the run state.
func TestAuditStatusCopiesSamples(t *testing.T) {
	s := NewScheduler(nil, nil, nil, StartupConfig{})
	s.audit.status.Samples = []AuditSample{{ID: "a", Verdict: "internal"}}

	st := s.SensitivityAuditStatus()
	st.Samples[0].Verdict = "mutated"

	if s.audit.status.Samples[0].Verdict != "internal" {
		t.Fatal("status getter leaked the internal samples slice")
	}
}

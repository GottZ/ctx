// Sensitivity LLM audit (G41, F3-P9): an on-demand batch job that classifies
// every home-scope block still carrying sensitivity_source='default' out of
// the E1 credentials default. Two separate yes/no questions per block over
// the classify chain (hard-local); the verdict table is fixed:
//
//	credentials=ja                → stays credentials, source='llm-audit'
//	credentials=nein, personal=ja → personal,          source='llm-audit'
//	nein × 2                      → internal,          source='llm-audit'
//	parse failure (no verdict)    → stays credentials, source='default' + retry cooldown
//	chain/backend failure         → RUN ABORTS, block untouched
//
// public is NEVER assigned — that stays a manual decision. 'manual' rows are
// untouchable by the store predicate, not by discipline. A backend failure is
// not a verdict about a block: aborting beats stamping cooldowns across the
// corpus while the chain is down.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// auditRetryCooldown keeps a no-verdict block out of the pick set long
	// enough that the next evening run retries it, not the same loop.
	auditRetryCooldown = 24 * time.Hour

	// auditBatchSize bounds one pick query; verdict writes remove rows from
	// the pick set, so the loop re-picks until it drains.
	auditBatchSize = 25

	// auditSampleDefault is the N=30 sample gate size (masterplan G41).
	auditSampleDefault = 30
)

// ErrAuditRunning rejects a second concurrent audit run.
var ErrAuditRunning = errors.New("sensitivity audit already running")

// classifyFunc is the seam between the audit loop and the LLM call —
// production value is llm.ClassifyBlockBool; the decision-table test swaps it
// (dreamCycleFunc pattern). adm is the scheduler's background admission
// binding (MW3, design/01 §4.6 N1: classify is the one ChainCall consumer
// classified background).
type classifyFunc func(ctx context.Context, db *pgxpool.Pool, bpool *backends.Pool, gaming backends.GamingState,
	question, title, content, blockID string, adm llm.Admission) (bool, error)

// AuditSample is one dry-run verdict for the N=30 gate: what the model
// answered and what the run WOULD have written.
type AuditSample struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Credentials *bool  `json:"credentials"` // nil = question not reached / no verdict
	Personal    *bool  `json:"personal"`
	Verdict     string `json:"verdict"` // credentials|personal|internal|no-verdict
}

// AuditStatus is the in-memory state of the current/last run. Pending counts
// come from the DB at status time (handler) — this struct carries only what
// the run itself knows.
type AuditStatus struct {
	Running         bool          `json:"running"`
	DryRun          bool          `json:"dry_run"`
	StartedAt       time.Time     `json:"started_at,omitzero"`
	FinishedAt      time.Time     `json:"finished_at,omitzero"`
	Processed       int           `json:"processed"`
	KeptCredentials int           `json:"kept_credentials"`
	ToPersonal      int           `json:"to_personal"`
	ToInternal      int           `json:"to_internal"`
	NoVerdict       int           `json:"no_verdict"`
	Discarded       int           `json:"discarded"` // verdict UPDATE matched 0 rows (manual race)
	Aborted         bool          `json:"aborted"`
	LastError       string        `json:"last_error,omitempty"`
	Samples         []AuditSample `json:"samples,omitempty"`
}

// auditState is the mutex-guarded run state on the scheduler.
type auditState struct {
	mu      sync.Mutex
	running bool
	status  AuditStatus
}

// StartSensitivityAudit launches the audit batch in the background. dryRun
// classifies WITHOUT writing (the sample gate; limit defaults to 30, picked
// in random order for representativeness). limit 0 on a live run = drain
// everything. Returns ErrAuditRunning when a run is in flight.
func (s *Scheduler) StartSensitivityAudit(dryRun bool, limit int) error {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	if s.audit.running {
		return ErrAuditRunning
	}
	if dryRun && limit <= 0 {
		limit = auditSampleDefault
	}
	s.audit.running = true
	s.audit.status = AuditStatus{Running: true, DryRun: dryRun, StartedAt: time.Now()}

	go s.runSensitivityAudit(dryRun, limit)
	return nil
}

// SensitivityAuditStatus returns a copy of the current/last run state.
func (s *Scheduler) SensitivityAuditStatus() AuditStatus {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	st := s.audit.status
	st.Samples = append([]AuditSample(nil), st.Samples...)
	return st
}

// runSensitivityAudit drains the pick set (or the dry-run sample) with dream
// loop manners: yield to active queries, stop on shutdown, never panic the
// process. cfg snapshot per batch (§2.3).
func (s *Scheduler) runSensitivityAudit(dryRun bool, limit int) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in sensitivity audit", "error", r, "stack", string(debug.Stack()))
		}
		s.audit.mu.Lock()
		s.audit.running = false
		s.audit.status.Running = false
		s.audit.status.FinishedAt = time.Now()
		s.audit.mu.Unlock()
	}()

	ctx := s.lifecycleCtx()
	slog.Info("scheduler: sensitivity audit started", "dry_run", dryRun, "limit", limit)

	// 06-C6 / T38: audit each tenant under its OWN config generation + its
	// entitlement-clamped home scope, iterating the authoritative tenant list.
	// Single-tenant: a 1-element loop over _global == the pre-T13 single pass.
	for _, bt := range s.backgroundTenantsFn(ctx) {
		if s.auditTenantScope(ctx, bt, dryRun, limit) {
			return // shutdown / pick error / infra abort already recorded
		}
	}

	st := s.SensitivityAuditStatus()
	slog.Info("scheduler: sensitivity audit finished",
		"dry_run", dryRun, "processed", st.Processed,
		"kept_credentials", st.KeptCredentials, "to_personal", st.ToPersonal,
		"to_internal", st.ToInternal, "no_verdict", st.NoVerdict, "discarded", st.Discarded)
}

// auditTenantScope drains the audit pick set (or the dry-run sample) for ONE
// iterated tenant under its config snapshot (06-C6), with dream-loop manners:
// yield to active queries, stop on shutdown, never panic the process. cfg
// snapshot per batch (§2.3). It returns abort=true when the WHOLE audit must
// stop — shutdown, a pick error (already recorded via auditAbort), or an
// auditOneBlock infra abort — and false when this tenant's drain finished
// normally (the caller continues to the next tenant).
//
// limit bounds blocks PER TENANT (each iterated tenant drains up to limit), not
// per whole run. At a single tenant this is identical to the pre-T13 cap; the
// cross-tenant aggregation of limit + run status is refined with the
// entitlement-correct background path T38 (04-W6).
func (s *Scheduler) auditTenantScope(ctx context.Context, bt backgroundTenant, dryRun bool, limit int) (abort bool) {
	processed := 0
	for {
		if ctx.Err() != nil {
			s.auditAbort("shutdown")
			return true
		}
		// Demand interruption: audit GPU work yields to live queries.
		if s.activeQueries.Load() > 0 {
			select {
			case <-ctx.Done():
				s.auditAbort("shutdown")
				return true
			case <-time.After(dreamYieldWait):
			}
			continue
		}

		cfg := s.cfg.SnapshotForTenant(ctx, bt.scope)
		scope := effectiveHomeScope(cfg.Scheduler.HomeScope, bt.owned)
		if scope == "" {
			scope = "private"
		}

		batch := auditBatchSize
		if dryRun {
			// One random pick serves the whole sample — the drain ends after
			// this batch (a second random pick would re-draw the same rows).
			batch = limit
		} else if limit > 0 && limit-processed < batch {
			batch = limit - processed
		}
		if batch <= 0 {
			return false
		}

		blocks, err := store.PickAuditBlocks(ctx, s.pool, scope, auditRetryCooldown, batch, dryRun)
		if err != nil {
			s.auditAbort(fmt.Sprintf("pick: %v", err))
			return true
		}
		if len(blocks) == 0 {
			return false
		}

		for _, blk := range blocks {
			if ctx.Err() != nil {
				s.auditAbort("shutdown")
				return true
			}
			sample, abort := s.auditOneBlock(ctx, blk, cfg.GamingState(), dryRun)
			if abort {
				return true
			}
			processed++
			if dryRun {
				s.audit.mu.Lock()
				s.audit.status.Samples = append(s.audit.status.Samples, sample)
				s.audit.mu.Unlock()
			}
		}
		if dryRun {
			return false // one pick per dry run — random order would re-pick the same rows
		}
	}
}

// auditOneBlock asks the two questions and applies (or records) the verdict.
// abort=true when the chain/backend failed — that is an infrastructure
// condition, not a block verdict.
func (s *Scheduler) auditOneBlock(ctx context.Context, blk store.AuditBlock, gaming backends.GamingState, dryRun bool) (AuditSample, bool) {
	sample := AuditSample{ID: blk.ID, Title: blk.Title}

	cred, err := s.classify(ctx, s.pool, s.backendPool, gaming, llm.QuestionCredentials, blk.Title, blk.Content, blk.ID, s.backgroundAdmission())
	verdict, noVerdict := backends.SensCredentials, false
	switch {
	case err != nil && !isParseFailure(err):
		s.auditAbort(fmt.Sprintf("classify chain: %v", err))
		return sample, true
	case err != nil:
		noVerdict = true
	case cred:
		sample.Credentials = &cred
	default:
		sample.Credentials = &cred
		pers, perr := s.classify(ctx, s.pool, s.backendPool, gaming, llm.QuestionPersonal, blk.Title, blk.Content, blk.ID, s.backgroundAdmission())
		switch {
		case perr != nil && !isParseFailure(perr):
			s.auditAbort(fmt.Sprintf("classify chain: %v", perr))
			return sample, true
		case perr != nil:
			noVerdict = true
		case pers:
			sample.Personal = &pers
			verdict = backends.SensPersonal
		default:
			sample.Personal = &pers
			verdict = backends.SensInternal
		}
	}

	s.audit.mu.Lock()
	s.audit.status.Processed++
	s.audit.mu.Unlock()

	if noVerdict {
		sample.Verdict = "no-verdict"
		s.auditCount(func(st *AuditStatus) { st.NoVerdict++ })
		if !dryRun {
			if err := store.MarkAuditAttempt(ctx, s.pool, blk.ID); err != nil {
				slog.Error("scheduler: audit cooldown stamp failed", "block_id", blk.ID, "error", err)
			}
		}
		return sample, false
	}

	sample.Verdict = string(verdict)
	if dryRun {
		return sample, false
	}

	applied, err := store.ApplyAuditVerdict(ctx, s.pool, blk.ID, verdict)
	if err != nil {
		s.auditAbort(fmt.Sprintf("verdict write: %v", err))
		return sample, true
	}
	if !applied {
		// Reclassified (e.g. manually) between pick and write — the source
		// predicate discarded the verdict. Correct, count it, move on.
		s.auditCount(func(st *AuditStatus) { st.Discarded++ })
		return sample, false
	}
	switch verdict {
	case backends.SensCredentials:
		s.auditCount(func(st *AuditStatus) { st.KeptCredentials++ })
	case backends.SensPersonal:
		s.auditCount(func(st *AuditStatus) { st.ToPersonal++ })
	default:
		s.auditCount(func(st *AuditStatus) { st.ToInternal++ })
	}
	return sample, false
}

func (s *Scheduler) auditCount(f func(*AuditStatus)) {
	s.audit.mu.Lock()
	f(&s.audit.status)
	s.audit.mu.Unlock()
}

func (s *Scheduler) auditAbort(reason string) {
	s.audit.mu.Lock()
	s.audit.status.Aborted = true
	s.audit.status.LastError = reason
	s.audit.mu.Unlock()
	slog.Error("scheduler: sensitivity audit aborted", "reason", reason)
}

// isParseFailure separates "the model answered garbage" (a no-verdict for
// THIS block: cooldown + continue) from "the chain/backend failed" (an
// infrastructure condition: abort the run).
func isParseFailure(err error) bool {
	return errors.Is(err, llm.ErrClassifyParse)
}

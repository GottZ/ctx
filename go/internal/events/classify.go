// Credentials pattern classify (G40, F3-P8): an on-demand batch job that
// re-audits the home-scope corpus with the deterministic sensitivity.Scan
// detector and RAISES every pattern hit to credentials (source='pattern').
//
// It is the retroactive VETO against G41: where the LLM audit downgraded a
// block out of the credentials default, a pattern hit stamps source='pattern',
// which the audit's pick set (source='default' only) can never re-touch. Pure
// CPU + DB — no GPU, no LLM — so, unlike the audit, it does not yield to live
// queries; it just stays shutdown-aware and never panics the process.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/sensitivity"
	"github.com/GottZ/ctx/internal/store"
)

const (
	// classifyBatchSize bounds one keyset pick; the cursor advances past every
	// scanned row, so the loop drains regardless of the verdict.
	classifyBatchSize = 200

	// classifySampleCap bounds the status payload: count every hit, but stop
	// growing the sample slice past this (a corpus with thousands of hits is a
	// different problem than this status surface should carry).
	classifySampleCap = 200
)

// ErrClassifyRunning rejects a second concurrent classify run.
var ErrClassifyRunning = errors.New("credentials classify already running")

// ClassifySample is one pattern hit for the dry-run gate: which block and which
// rule fired. Never carries the matched secret.
type ClassifySample struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
}

// ClassifyStatus is the in-memory state of the current/last classify run.
type ClassifyStatus struct {
	Running    bool             `json:"running"`
	DryRun     bool             `json:"dry_run"`
	StartedAt  time.Time        `json:"started_at,omitzero"`
	FinishedAt time.Time        `json:"finished_at,omitzero"`
	Scanned    int              `json:"scanned"`
	Upgraded   int              `json:"upgraded"`  // live: applied; dry-run: would-apply
	Discarded  int              `json:"discarded"` // verdict matched 0 rows (raced to credentials/manual)
	Aborted    bool             `json:"aborted"`
	LastError  string           `json:"last_error,omitempty"`
	Samples    []ClassifySample `json:"samples,omitempty"`
}

// classifyState is the mutex-guarded run state on the scheduler.
type classifyState struct {
	mu      sync.Mutex
	running bool
	status  ClassifyStatus
}

// StartCredentialsClassify launches the re-audit in the background. dryRun
// scans WITHOUT writing (the gate: see exactly what WOULD be raised, on the
// real corpus, before committing). limit 0 = scan everything; limit>0 stops
// after that many blocks scanned. Returns ErrClassifyRunning when in flight.
func (s *Scheduler) StartCredentialsClassify(dryRun bool, limit int) error {
	s.credClassify.mu.Lock()
	defer s.credClassify.mu.Unlock()
	if s.credClassify.running {
		return ErrClassifyRunning
	}
	s.credClassify.running = true
	s.credClassify.status = ClassifyStatus{Running: true, DryRun: dryRun, StartedAt: time.Now()}
	go s.runCredentialsClassify(dryRun, limit)
	return nil
}

// CredentialsClassifyStatus returns a copy of the current/last run state.
func (s *Scheduler) CredentialsClassifyStatus() ClassifyStatus {
	s.credClassify.mu.Lock()
	defer s.credClassify.mu.Unlock()
	st := s.credClassify.status
	st.Samples = append([]ClassifySample(nil), st.Samples...)
	return st
}

// runCredentialsClassify keyset-drains the candidate set, scanning each block
// and (live) raising hits. cfg snapshot once — the home scope does not change
// mid-run.
func (s *Scheduler) runCredentialsClassify(dryRun bool, limit int) {
	defer func() {
		s.credClassify.mu.Lock()
		s.credClassify.running = false
		s.credClassify.status.Running = false
		s.credClassify.status.FinishedAt = time.Now()
		s.credClassify.mu.Unlock()
	}()
	defer guardPanic("credentials classify")

	ctx := s.lifecycleCtx()

	// 06-C6 / T38: classify each tenant under its OWN config generation + its
	// entitlement-clamped home scope, iterating the authoritative tenant list.
	// Single-tenant: a 1-element loop over _global == the pre-T13 single pass.
	for _, bt := range s.backgroundTenantsFn(ctx) {
		if s.classifyTenantScope(ctx, bt, dryRun, limit) {
			return // shutdown / pick / verdict-write error already recorded
		}
	}

	st := s.CredentialsClassifyStatus()
	slog.Info("scheduler: credentials classify finished",
		"dry_run", dryRun, "scanned", st.Scanned, "upgraded", st.Upgraded, "discarded", st.Discarded)
}

// classifyTenantScope keyset-drains the candidate set for ONE iterated tenant
// under its config snapshot (06-C6), scanning each block and (live) raising
// hits. cfg snapshot once per tenant — the home scope does not change mid-run.
// Returns abort=true when the whole classify must stop — shutdown, or a pick /
// verdict-write error already recorded via classifyAbort — and false when this
// tenant's drain finished normally (the caller continues to the next tenant).
//
// limit bounds blocks PER TENANT (each iterated tenant drains up to limit), not
// per whole run. At a single tenant this is identical to the pre-T13 cap; the
// cross-tenant aggregation of limit is refined with the entitlement-correct
// background path T38 (04-W6).
func (s *Scheduler) classifyTenantScope(ctx context.Context, bt backgroundTenant, dryRun bool, limit int) (abort bool) {
	cfg := s.cfg.SnapshotForTenant(ctx, bt.scope)
	scope := effectiveHomeScope(cfg.Scheduler.HomeScope, bt.owned)
	if scope == "" {
		scope = "private"
	}
	slog.Info("scheduler: credentials classify started", "dry_run", dryRun, "limit", limit, "scope", scope)

	afterID := ""
	scanned := 0
	for {
		if ctx.Err() != nil {
			s.classifyAbort("shutdown")
			return true
		}
		batch := classifyBatchSize
		if limit > 0 && limit-scanned < batch {
			batch = limit - scanned
		}
		if batch <= 0 {
			return false
		}
		blocks, err := store.PickClassifyCandidates(ctx, s.pool, scope, afterID, batch)
		if err != nil {
			s.classifyAbort(fmt.Sprintf("pick: %v", err))
			return true
		}
		if len(blocks) == 0 {
			return false
		}
		for _, blk := range blocks {
			afterID = blk.ID
			scanned++
			m, hit := sensitivity.Scan(blk.Content)
			if !hit {
				continue
			}
			s.classifyCount(func(st *ClassifyStatus) {
				if len(st.Samples) < classifySampleCap {
					st.Samples = append(st.Samples, ClassifySample{ID: blk.ID, Title: blk.Title, Kind: m.Kind})
				}
			})
			if dryRun {
				s.classifyCount(func(st *ClassifyStatus) { st.Upgraded++ })
				continue
			}
			applied, err := store.ApplyPatternVerdict(ctx, s.pool, blk.ID, scope, m.Kind, m.Reason)
			if err != nil {
				s.classifyAbort(fmt.Sprintf("verdict write: %v", err))
				return true
			}
			if applied {
				s.classifyCount(func(st *ClassifyStatus) { st.Upgraded++ })
			} else {
				s.classifyCount(func(st *ClassifyStatus) { st.Discarded++ })
			}
		}
		// Additive across batches AND tenants — consistent with Upgraded/Discarded
		// (++). delta == len(blocks): every block in the batch increments scanned.
		s.classifyCount(func(st *ClassifyStatus) { st.Scanned += len(blocks) })
		if len(blocks) < batch {
			return false // drained
		}
	}
}

func (s *Scheduler) classifyCount(f func(*ClassifyStatus)) {
	s.credClassify.mu.Lock()
	f(&s.credClassify.status)
	s.credClassify.mu.Unlock()
}

func (s *Scheduler) classifyAbort(reason string) {
	s.credClassify.mu.Lock()
	s.credClassify.status.Aborted = true
	s.credClassify.status.LastError = reason
	s.credClassify.mu.Unlock()
	slog.Error("scheduler: credentials classify aborted", "reason", reason)
}

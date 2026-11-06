package oai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

// Sub-blocks of one attempt (design/02 §4.8, §4.11.1; masterplan K21; wave
// W02-09 — the rerank route W02-10 walks its documents through the same
// helper). A request of n units (embedding inputs, rerank documents) is
// split by the planner's weight plan of the attempt's row
// (RequestAcquirer.Blocks after First/Next, A03-W8: block i carries
// Blocks()[i] units, the clamp min(sub_batch, ExternalWeightCap) is the
// planner's, never the route's). Block one runs under the walker's lease;
// every further block asks the continuation door (Continue, A03-W17) for a
// lease of its own — one lease per upstream request, Σacquire == Σwire. The
// order of one block boundary:
//
//	usage of block i → its lease → Release → run check → Continue(request
//	context) → wire call of block i+1 under that lease
//
// Block i's lease goes back BEFORE block i+1 asks (v1census rule 7): a
// continuation waits at the head of its class for units, and units its own
// request still holds never come (A03-W8 §Übergaben, a deadlock on a full
// E, executed there). Every continuation asks on the REQUEST context, never
// on the run context of the released lease — Release cancels that one, and
// a continuation on it ends at once with context.Canceled (A03-W8 Z. 269).
// Usage is reported to each block's lease before its Release: a report after
// it is dropped (dispatch.Lease.ReportUsage).
//
// Sticky (design/02 §4.8): every block of an attempt runs on its row; the
// attempt commits as soon as block one has a result, so the walker stops
// there (llm.WalkChain: no failover after a committed attempt) — two vector
// builds never mix in one answer. A failed continuation is an admission
// outcome (the AdmissionError hull: terminal, no Classify, no health report,
// err_class admission); a failed wire call of block ≥ 2 ends the attempt
// committed with its upstream error.

// errNoBlocks is a weight plan without a block: a planner defect (A03-W8
// hands out at least one block for every request).
var errNoBlocks = errors.New("oai: the attempt's weight plan has no block")

// errNoLease is a continuation door that answered without a lease or run
// context and without an error — a contract breach, refused like the
// walker refuses one of First/Next (llm acquireFor).
var errNoLease = errors.New("oai: continuation returned no lease")

// blockFunc is the wire call of block i over the units [lo, hi) under
// lease, on its run context runCtx, with the block's wire deadline. It
// returns the block's usage. The lease is the one the call runs under (the
// X13 census: a wire call lives in a function that takes its lease); the
// block does not release it — run does.
type blockFunc func(i, lo, hi int, lease *dispatch.Lease, runCtx context.Context, timeout time.Duration) (dispatch.Usage, error)

// subBlockRun is the block walk of one request.
type subBlockRun struct {
	// ctx is the request's own context: every Continue asks on it.
	ctx context.Context
	// acq is the plan's acquirer; Continue is its continuation door.
	acq RequestAcquirer
	// role is the alias role the leases are asked for.
	role string
	// timeout is a block's wire deadline, read per continuation (what is
	// left of the request deadline shrinks block by block).
	timeout func(*backends.Backend) time.Duration
	// alive is the run check before every continuation: nil goes on, else
	// the cause the attempt ends with (the shutdown, the watcher, a refusal
	// of the route) — no acquire on an ended run.
	alive func() error
	// report hands a block's usage to its lease before the Release; nil ⇒
	// (*dispatch.Lease).ReportUsage. The dispatcher keeps an external
	// lease's usage unexported, so the per-lease charge is observable only
	// here (gate Z3).
	report func(*dispatch.Lease, dispatch.Usage)
}

// continuationAllowed is the k = 1 rule (design/02 §4.8, masterplan K21): a
// request whose weight plan has more than one block needs the planner's
// continuation door. Without it (Plan.Continuation false) only a one-block
// plan may run — the route refuses the rest with 400 unsupported_value,
// before any further wire work.
func continuationAllowed(blocks []int, continuation bool) bool {
	return continuation || len(blocks) <= 1
}

// checkBlocks validates the planner's weight plan for a request of units:
// at least one block, every block ≥ 1, the blocks summing to units. Anything
// else is a planner defect the route must not walk.
func checkBlocks(blocks []int, units int) error {
	if len(blocks) == 0 {
		return errNoBlocks
	}
	sum := 0
	for _, n := range blocks {
		if n < 1 {
			return fmt.Errorf("oai: weight plan %v has an empty block", blocks)
		}
		sum += n
	}
	if sum != units {
		return fmt.Errorf("oai: weight plan %v covers %d units, the request has %d", blocks, sum, units)
	}
	return nil
}

// run serves every block of one attempt on row b: block one on the walker's
// lease (runCtx, timeout), each further block on its own continuation lease.
// It returns the usage of block one (the walker's lease; a later block's
// usage went to its own lease), whether the attempt committed — block one
// has a result — and the error that ended the attempt.
func (r subBlockRun) run(runCtx context.Context, b *backends.Backend, lease *dispatch.Lease, timeout time.Duration,
	blocks []int, do blockFunc) (dispatch.Usage, bool, error) {
	if len(blocks) == 0 {
		return dispatch.Usage{}, false, errNoBlocks
	}
	first, err := do(0, 0, blocks[0], lease, runCtx, timeout)
	if err != nil {
		return first, false, err
	}
	if len(blocks) == 1 {
		return first, true, nil // the walker reports it and releases
	}
	report := r.report
	if report == nil {
		report = (*dispatch.Lease).ReportUsage
	}
	// Block one is done: its usage to its lease, the lease back (idempotent
	// — the walker's deferred Release becomes a no-op, its later report one
	// too), before block two asks.
	report(lease, first)
	lease.Release()
	lo := blocks[0]
	for i := 1; i < len(blocks); i++ {
		if err := r.alive(); err != nil {
			return first, true, err
		}
		t := r.timeout(b)
		l, rc, err := r.continueBlock(b, t)
		if err != nil {
			return first, true, err
		}
		hi := lo + blocks[i]
		if err := r.block(l, rc, report, i, lo, hi, t, do); err != nil {
			return first, true, err
		}
		lo = hi
	}
	return first, true, nil
}

// continueBlock asks the continuation door on the request context. Every
// failure comes back inside the acquire hull (llm.AdmissionError), so the
// walker ends the attempt terminally without a health report and the route
// hands it to the planner's writer. A lease in another class than the plan
// declared is handed back — the walker's class assert for the further
// blocks (llm.ErrWalkClass in the hull: the writer's code-bug answer).
func (r subBlockRun) continueBlock(b *backends.Backend, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	l, rc, err := r.acq.Continue(r.ctx, b, r.role, timeout)
	if err == nil && (l == nil || rc == nil) {
		err = errNoLease
	}
	if err == nil && l.Class() != r.acq.Class() {
		err = fmt.Errorf("%w: continuation lease in class %s, plan class %s", llm.ErrWalkClass, l.Class(), r.acq.Class())
	}
	if err != nil {
		if l != nil {
			l.Release() // a contract breach must not leak the units
		}
		if !llm.IsAdmissionError(err) {
			err = &llm.AdmissionError{Err: err, Backend: b.Name, Host: b.Host}
		}
		return nil, nil, err
	}
	return l, rc, nil
}

// block runs block i under its continuation lease; defer is the only
// release form (B1: panic-safe), the usage reaches the lease before it.
func (r subBlockRun) block(l *dispatch.Lease, rc context.Context, report func(*dispatch.Lease, dispatch.Usage),
	i, lo, hi int, timeout time.Duration, do blockFunc) error {
	defer l.Release()
	u, err := do(i, lo, hi, l, rc, timeout)
	if err == nil || u != (dispatch.Usage{}) {
		report(l, u)
	}
	return err
}

// Package dispatch is the single process-wide admission layer in front of
// every inference wire call (Vorhaben E, design/01 §1.2 invariant I-D1): a
// blocking Acquire per physical backend origin with an explicit priority
// class (interactive > background, FIFO per class, no barging), a demand
// herald counting interactive requests from HTTP ingress, per-principal /
// per-tenant / per-target wait caps (Deckel-Staffel, design/03 §4.5.4) and a
// reaper against leaked leases. Policy lives in data (context_backends.limits
// → DerivePolicy): an empty policy makes every Acquire a pass-through, so the
// code waves stay behavior-neutral until the deliberate data activation.
//
// Layering: dispatch is a LEAF package (stdlib only). Policy and settings
// arrive as plain values (BackendRow / Settings) — the owner side (cmd,
// handler, settings reload) adapts pool rows and config snapshots. That keeps
// the package import-cycle-free against llm/embedcache/rrf/chat/handler and
// parameter-pure like the other domain packages.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Class is the admission priority of one wire call. Two classes by decision
// E-U1: no consumer has a requirement between "a human waits synchronously"
// and "nobody waits" — a third class would need an ordering decision against
// every existing one (design/03 §4.1). Extension rule (design/03 C7): a new
// class only as its own wave including an authorization decision, and
// structurally — since A03-W1 — in three places the compiler or the table
// test enforces: its constant goes BEFORE numClasses, its row into
// classTable (the build fails while a trailing row is missing), and every
// class branch is a switch over the valid classes with a fail-closed
// default, so a forgotten branch is test-red instead of silently treating
// the new value like an existing class. A value outside the table is a code
// bug: both admission doors refuse it with ErrInvalidClass before any
// waiter or lease exists, String names it "invalid", External is false.
type Class uint8

// The admission classes (E-U1). A new class goes before numClasses.
const (
	// ClassInteractive — a human waits synchronously for the answer.
	ClassInteractive Class = iota
	// ClassBackground — dream, audit, backfill, daily synthesis (scheduler
	// paths). Background leases are the only ones the dispatcher may cancel
	// (I-D1: interactive leases are never canceled).
	ClassBackground
	// numClasses is the enum end, not a class: every value >= numClasses is
	// invalid, and classTable carries exactly numClasses rows.
	numClasses
)

// invalidClassName is String() of a value outside the class table — never
// the name of a real class (pairwise distinct dispatch_class vocabulary,
// design/03 §3.4, design/06 B-06-12).
const invalidClassName = "invalid"

// classInfo is one row of the class table. Deliberately WITHOUT a rank: the
// admission order is policy data (A-E2 no. 1, dispatch.external_order, wave
// A03-W2), not a property of the class.
type classInfo struct {
	// name is the String() form, persisted as context_llm_log.dispatch_class.
	name string
	// external marks a class of external consumers (design/01 §4.5 I-K10: a
	// consumer principal is admitted only in an external class, wave W01-7).
	// Both in-house classes are false; the external classes arrive with
	// their own waves (design/03 §4.1: XU A03-W3, XB A03-W9).
	external bool
}

// classTable is the single source of String, valid and External: one row per
// class below numClasses, keyed by the constant.
var classTable = [...]classInfo{
	ClassInteractive: {name: "interactive"},
	ClassBackground:  {name: "background"},
}

// Compile-time half of the C7 gate: len(classTable) == numClasses in both
// directions. A constant added before numClasses without its row (or a row
// keyed past the enum end) makes one of these array lengths negative, so the
// build fails. A row missing in the MIDDLE compiles as a zero row with an
// empty name — valid() treats it as invalid (refused at the doors) and the
// table test (class_table_test.go) is red.
var (
	_ [len(classTable) - int(numClasses)]struct{}
	_ [int(numClasses) - len(classTable)]struct{}
)

// valid reports whether c has a (named) row in the class table.
func (c Class) valid() bool {
	return c < numClasses && classTable[c].name != ""
}

// String implements fmt.Stringer for logs, snapshots and the persisted
// dispatch_class column: the table name, "invalid" outside the table
// (pre-A03-W1 every non-interactive value read "background").
func (c Class) String() string {
	if !c.valid() {
		return invalidClassName
	}
	return classTable[c].name
}

// External reports whether c is a class of external consumers — the
// classifier of the I-K10 gate (design/01 §4.5: a consumer principal in a
// class with External() == false is refused with ErrClassUnauthorized, wave
// W01-7). False for both in-house classes and fail-closed false outside the
// class table.
func (c Class) External() bool {
	return c.valid() && classTable[c].external
}

// GlobalScope is the shared-backend sentinel on context_backends.scope.
// Mirrors backends.GlobalScope without the import (leaf rule) — the value is
// a DDL constant anchored in migration 062. It carries the K2 limits
// authority: dispatch keys on a shared origin count ONLY from '_global' rows.
const GlobalScope = "_global"

// Target names one physical backend origin (normalized form of the
// base_url). Two spellings of the same physical target MUST collapse into
// one Target — otherwise the slots cap is silently bypassable by string
// drift (design/01 §4.3, fail-open against the protected good).
type Target struct {
	// Origin is the normalized origin (NormalizeOrigin output). Acquire
	// re-normalizes defensively, so a raw base_url also works.
	Origin string
}

// NormalizeOrigin canonicalizes a backend base_url to its physical origin:
// scheme and host lowercase, default ports made explicit (http ⇒ :80,
// https ⇒ :443), path/query/fragment dropped (including a /v1 suffix).
// "http://HOST:8089/v1" and "http://host:8089" yield the SAME origin;
// unequal targets stay distinct (design/01 §4.3, negatively probed).
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("dispatch: unparseable origin %q: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if scheme == "" || host == "" {
		return "", fmt.Errorf("dispatch: origin %q lacks scheme or host", raw)
	}
	if u.Port() == "" {
		switch scheme {
		case "http":
			host += ":80"
		case "https":
			host += ":443"
		}
	}
	return scheme + "://" + host, nil
}

// Principal identifies the caller for class authorization (I-D1/B8), the
// Deckel-Staffel and the fairness bucket. Derived from auth.AuthResult (N10)
// — since MW4 EXCLUSIVELY out of the request ctx via the boot-installed
// principal hook (design/03 §4.1.1), never as a caller parameter: detached
// scheduler contexts resolve empty, which makes them STRUCTURALLY background.
type Principal struct {
	ApiKeyID  string
	TenantID  string
	HomeScope string
	// PrincipalID is the person identity behind the key (OAuth F2, auth.go
	// AuthResult.PrincipalID / migration 095). Carried here like TenantID —
	// an identity dimension, NOT the fairness key: fairKey stays HomeScope
	// (E-F1, the tenant IS the scope namespace), so scheduling is byte-
	// unchanged by F2. It is the seam for a later person-level fairness mode
	// (policy.FairnessPrincipalRR) once a principal may hold keys across
	// tenants (E1t=multi); no consumer flips it in this wave.
	PrincipalID string
}

// fairKey is the fairness bucket of a principal (E-F1: HomeScope, single
// level — the tenant identity IS the scope namespace, 06-C5). Fallback
// ApiKeyID for a key without scope (defensive); "" only for the empty
// principal, which is structurally background and never takes part in
// interactive fairness (design/04 §4.1).
func (p Principal) fairKey() string {
	if p.HomeScope != "" {
		return p.HomeScope
	}
	return p.ApiKeyID
}

// Request is one admission demand: which physical target, which class. WHO
// asks is deliberately NOT a field (design/03 §4.1.1): the principal — class
// authorization + Deckel-Staffel dimensions — derives exclusively from the
// Acquire ctx via the boot-installed hook (SetPrincipalHook), so a stored
// AuthResult cannot be replayed as a parameter. Role is a telemetry dimension
// only, never dispatch semantics.
type Request struct {
	Target Target
	Class  Class
	// Role travels into llmlog/telemetry (design/01 §4.3).
	Role string
	// Deadline is the caller's attempt-deadline hint (N1 knows b.TimeoutFor
	// before the acquire, N4 has rerank.Timeout). Zero is allowed: leases
	// without a hint AND without a ctx deadline fall under the reaper's
	// lease_max_age fallback (the embed path is exactly this case, N3).
	// Contract note for call-site waves: compute the hint relative to the
	// expected ADMISSION (wire deadline is admission+timeout, rule V1) — a
	// hint anchored at enqueue underestimates the reap reference by the wait
	// time; lease_reap_grace is the only slack on top. Callers holding a
	// timeout DURATION therefore pass DeadlineIn, not an absolute Deadline.
	Deadline time.Time
	// DeadlineIn is the admission-anchored form of the hint (MW3, closing
	// the contract note above): the dispatcher resolves it to
	// admitted+DeadlineIn AT ADMISSION TIME — exactly the wire deadline of
	// rule V1, however long the acquire waited. The reap reference stays the
	// EARLIEST of {Deadline, ctx deadline, admitted+DeadlineIn} over the
	// provided values. Zero = not provided.
	DeadlineIn time.Duration
}

// Wrap error doctrine (K1, binding since MW1): the two cancel causes WRAP
// context.Canceled. Go ≥ 1.23 places the context CAUSE into the http error
// chain — a naked errors.New sentinel would classify as ClassServerFault and
// health-poison the healthy target (design/02 §9.1 no. 3, repro go1.26.4 in
// both directions). Consumers use EXCLUSIVELY errors.Is over the error
// chain, never string or identity comparison of context.Cause (R1).
var (
	// ErrPreempted is the cancel cause when the dispatcher revokes a
	// background lease in favor of interactive demand (preempt path, MW18).
	ErrPreempted = fmt.Errorf("dispatch: preempted by interactive demand: %w", context.Canceled)
	// ErrReaped is the cancel cause when the reaper force-releases a leaked
	// background lease past its reap reference (design/01 §4.4/B1).
	ErrReaped = fmt.Errorf("dispatch: lease reaped after deadline: %w", context.Canceled)
)

// Rejection typology (design/03 §4.5.1): all four end the chain walk
// TERMINALLY — a rejection is NOT an attempt (no Classify, no health report,
// no llmlog line, no failover spill onto openrouter). Sentinel texts stay
// server-side; the client only ever sees the generic B6 body.
var (
	// ErrQueueFull — background wait queue at background_queue_max
	// (background only; the arm retries on its own cadence).
	ErrQueueFull = errors.New("dispatch: background queue full")
	// ErrPrincipalSaturated — B9 cap per ApiKeyID × target (interactive).
	ErrPrincipalSaturated = errors.New("dispatch: principal wait cap reached")
	// ErrTenantSaturated — second cap per TenantID × target: N self-minted
	// keys of one tenant cannot multiply the principal cap (C8).
	ErrTenantSaturated = errors.New("dispatch: tenant wait cap reached")
	// ErrTargetSaturated — aggregate cap per target: an early 429 beats a
	// wait slot no plausible client patience would ever see served.
	ErrTargetSaturated = errors.New("dispatch: target wait cap reached")
)

// ErrInvalidClass is both admission doors' answer to a Request.Class outside
// the class table (design/03 §4.1 C7 precondition, wave A03-W1). It is a
// CODE BUG, not a capacity verdict: deliberately NOT part of IsRejection (no
// 429, no Retry-After) — the /v1 mapping is 503 server_error with
// x-should-retry: false (masterplan K29, wave A03-W8). Like every admission
// error it is terminal for a chain walk and not an attempt. Every refusal is
// counted (Snapshot.InvalidClass) and logged at ERROR.
var ErrInvalidClass = errors.New("dispatch: invalid admission class")

// IsRejection is the ONE check point for handler mapping (429/SSE): true for
// every Deckel-Staffel sentinel and ErrQueueFull, false for everything else
// (in particular ctx.Err() and ErrInvalidClass). It exists so no handler ever matches error
// STRINGS — string comparisons are the drift source that would collide the
// generic B6 texts with the mapping (design/03 §4.5.1).
func IsRejection(err error) bool {
	return errors.Is(err, ErrQueueFull) ||
		errors.Is(err, ErrPrincipalSaturated) ||
		errors.Is(err, ErrTenantSaturated) ||
		errors.Is(err, ErrTargetSaturated)
}

// Admitter is the consumer-facing surface: blocking acquire with ctx, no
// ticket model (design/01 §4.2 — every consumer is already a goroutine that
// waits synchronously on its wire call; caller-ctx cancel vacates the wait
// slot for free).
type Admitter interface {
	Acquire(ctx context.Context, req Request) (*Lease, context.Context, error)
}

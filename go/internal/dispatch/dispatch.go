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
// every existing one (design/03 §4.1). Extension rule: a new class only as
// its own wave including an authorization decision (design/03 C7).
type Class uint8

// The two admission classes (E-U1).
const (
	// ClassInteractive — a human waits synchronously for the answer.
	ClassInteractive Class = iota
	// ClassBackground — dream, audit, backfill, daily synthesis (scheduler
	// paths). Background leases are the only ones the dispatcher may cancel
	// (I-D1: interactive leases are never canceled).
	ClassBackground
)

// String implements fmt.Stringer for logs and snapshots.
func (c Class) String() string {
	if c == ClassInteractive {
		return "interactive"
	}
	return "background"
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

// NewTarget builds a Target from a backend base_url.
func NewTarget(baseURL string) (Target, error) {
	o, err := NormalizeOrigin(baseURL)
	if err != nil {
		return Target{}, err
	}
	return Target{Origin: o}, nil
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
// Deckel-Staffel and the fairness bucket. Filled from auth.AuthResult (N10);
// empty on scheduler arms — which makes them STRUCTURALLY background.
type Principal struct {
	ApiKeyID  string
	TenantID  string
	HomeScope string
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

// Request is one admission demand: which physical target, which class, who
// asks. Role is a telemetry dimension only, never dispatch semantics.
type Request struct {
	Target Target
	Class  Class
	// Role travels into llmlog/telemetry (design/01 §4.3).
	Role string
	// Principal carries class authorization + the Deckel-Staffel dimensions.
	Principal Principal
	// Deadline is the caller's attempt-deadline hint (N1 knows b.TimeoutFor
	// before the acquire, N4 has rerank.Timeout). Zero is allowed: leases
	// without a hint AND without a ctx deadline fall under the reaper's
	// lease_max_age fallback (the embed path is exactly this case, N3).
	// Contract note for call-site waves: compute the hint relative to the
	// expected ADMISSION (wire deadline is admission+timeout, rule V1) — a
	// hint anchored at enqueue underestimates the reap reference by the wait
	// time; lease_reap_grace is the only slack on top.
	Deadline time.Time
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

// IsRejection is the ONE check point for handler mapping (429/SSE): true for
// every Deckel-Staffel sentinel and ErrQueueFull, false for everything else
// (in particular ctx.Err()). It exists so no handler ever matches error
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

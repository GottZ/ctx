package handler

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/GottZ/ctx/internal/backends"
)

// healthNotice is what an operator hears from /health about the state it
// probes (issue #504): a line when the state changes, DEBUG for every request
// that finds it unchanged. Before, every request logged a WARN per probed role
// while no backend of the role was enabled — three on a new installation with
// Dream on, about 8,640 a day at the compose healthcheck's 30 s — and a WARN
// per role and unreachable backend, and an ERROR while the database did not
// answer. The answer of /health does not change.
//
// One value serves the process (healthLines): /health and the status
// collector (StatusCollector.buildCheap) probe through the same HealthStatus
// with the same budget, and a change of state logs once however many of
// their calls observe the same state at the same time (calls that observe
// different states, on snapshots taken before and after a reload, log each
// change they apply). The state is kept where its reason is found:
//
//   - per role, why no backend of it is probed (roleProbed): the shared pool
//     has no row of the role, or its rows are all disabled. A WARN names the
//     reason and the remedy when a role stops being served or its reason
//     changes, an INFO says when a backend of the role is enabled again;
//   - per role and backend (name and base URL), a probe that found the
//     backend unreachable (pinged): a WARN when it stops answering, an INFO
//     when it answers again. A backend the last walk of its role did not
//     reach (walked) — an earlier candidate answered, or it is no candidate
//     any more: disabled, deleted, its role or base URL changed — is
//     forgotten without a line, so what it does unobserved is no state, and
//     the next walk that finds it down logs a WARN again;
//   - the database ping (database): an ERROR when it starts failing, an INFO
//     when it answers again.
//
// A probe whose caller left (its context cancelled: the client of /health
// went away, the status collector stopped) observes nothing about the target
// and changes no state. A role that is not probed (dream while dream.enabled
// is off) is forgotten.
type healthNotice struct {
	mu          sync.Mutex
	roles       map[string]roleProbe  // role → what its last probe found
	unreachable map[probeKey]struct{} // role and backend the last probe found unreachable
	dbDown      bool
}

// healthLines is the process's healthNotice (see healthNotice).
var healthLines = &healthNotice{}

// roleProbe is what a role's probe found among the shared pool's rows.
type roleProbe uint8

const (
	roleCandidates  roleProbe = iota + 1 // an enabled row of the role is probed
	roleNoBackend                        // the shared pool has no row of the role
	roleNoneEnabled                      // rows of the role, none of them enabled
)

// reason is the role state's attribute on its DEBUG line.
func (p roleProbe) reason() string {
	if p == roleNoBackend {
		return "no_backend_of_role"
	}
	return "none_enabled"
}

// probeKey names a backend's probe for one role: the row's name and the base
// URL the probe requests.
type probeKey struct{ role, name, host string }

// roleProbed records which candidates the probe of role found (rows: the
// shared pool's rows of the role, enabled or not) and logs a change of the
// role's state.
func (n *healthNotice) roleProbed(role string, rows int, candidates []*backends.Backend) {
	probe := roleCandidates
	switch {
	case len(candidates) > 0:
	case rows == 0:
		probe = roleNoBackend
	default:
		probe = roleNoneEnabled
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	prev, seen := n.roles[role]
	if n.roles == nil {
		n.roles = make(map[string]roleProbe)
	}
	n.roles[role] = probe
	switch {
	case probe == roleCandidates:
		if seen && prev != roleCandidates {
			slog.Info("health check: a backend of this role is enabled again — /health probes it", "role", role)
		}
	case prev == probe:
		slog.Debug("health check: no enabled backend for role", "role", role, "reason", probe.reason())
	case probe == roleNoBackend:
		slog.Warn("health check: the shared pool has no backend of this role — /health reports the role as error until one "+
			"is enabled; `ctx backends seed` seeds the pool (a chat and an embed backend), `ctx backends create` adds a "+
			"backend with the role. This line repeats only after a backend of the role was enabled in between", "role", role)
	default:
		slog.Warn("health check: every backend of this role in the shared pool is disabled — /health reports the role as "+
			"error until one is enabled; `ctx backends` lists the rows. This line repeats only after a backend of the role "+
			"was enabled in between", "role", role)
	}
}

// walked forgets the unreachable backends of role that the walk of the
// role's candidates did not reach (see healthNotice).
func (n *healthNotice) walked(role string, reached []*backends.Backend) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.retain(role, reached)
}

// retain forgets the unreachable backends of role that are not among
// candidates. Called with n.mu held.
func (n *healthNotice) retain(role string, candidates []*backends.Backend) {
	for k := range n.unreachable {
		if k.role != role {
			continue
		}
		found := false
		for _, b := range candidates {
			if b.Name == k.name && b.Host == k.host {
				found = true
				break
			}
		}
		if !found {
			delete(n.unreachable, k)
		}
	}
}

// pinged records the result of the probe of backend b for role (err nil:
// reachable) and logs a change of state.
func (n *healthNotice) pinged(ctx context.Context, role string, b *backends.Backend, err error) {
	key := probeKey{role: role, name: b.Name, host: b.Host}
	n.mu.Lock()
	defer n.mu.Unlock()
	_, down := n.unreachable[key]
	switch {
	case err == nil:
		if down {
			delete(n.unreachable, key)
			slog.Info("health check: backend answers again", "backend", b.Name, "role", role)
		}
	case callerLeft(ctx, err):
		slog.Debug("health check: backend ping abandoned, its caller left", "backend", b.Name, "role", role, "error", err)
	case down:
		slog.Debug("health check: backend ping failed", "backend", b.Name, "role", role, "error", err)
	default:
		if n.unreachable == nil {
			n.unreachable = make(map[probeKey]struct{})
		}
		n.unreachable[key] = struct{}{}
		slog.Warn("health check: backend ping failed", "backend", b.Name, "role", role, "error", err)
	}
}

// forgetRole drops the state of a role that is not probed.
func (n *healthNotice) forgetRole(role string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.roles, role)
	n.retain(role, nil)
}

// database records the result of the database ping (err nil: it answered)
// and logs a change of state. A failure that is its caller's leaving keeps
// the line it always had (logRequestError: INFO with client_gone when the
// client of /health gave up) and changes nothing.
func (n *healthNotice) database(ctx context.Context, err error) {
	const msg = "health check: database ping failed"
	if err != nil && callerLeft(ctx, err) {
		logRequestError(ctx, msg, "error", err)
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	switch {
	case err == nil:
		if n.dbDown {
			n.dbDown = false
			slog.Info("health check: database answers again")
		}
	case n.dbDown:
		slog.Debug(msg, "error", err)
	default:
		n.dbDown = true
		logRequestError(ctx, msg, "error", err)
	}
}

// callerLeft reports whether err is the cancellation of ctx itself — the
// caller of the probe went away — rather than an answer of its target. A
// probe that ran out of its own budget (context.DeadlineExceeded) found a
// target that did not answer in time: that is the target's state.
func callerLeft(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && errors.Is(ctx.Err(), context.Canceled)
}

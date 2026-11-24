package handler

import (
	"context"
	"maps"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5/pgxpool"
)

// healthRound is one probe round of the process (issue #519): the database
// ping and the walk of every probed role over one snapshot of the backend
// pool, on a context of its own that ends with healthProbeBudget. The calls of
// /health and of the status collector that arrive while a round over the same
// inputs runs wait for it instead of probing themselves, and a caller that
// leaves stops waiting without stopping the round. What a probe observes thus
// no longer depends on how long its caller waits:
//
//   - a backend that hangs is found down when the round's budget ends, also
//     when every caller left before it (a load balancer whose probe gives up
//     sooner) — where a probe that ended with its caller observed nothing, and
//     the backend stayed at DEBUG;
//   - a backend that answers within the budget is found up although every
//     caller left first — a rule that declared a backend down once no probe
//     completed for a while would report it down;
//   - a storm of requests holds one round, so one probe per base URL and one
//     database ping at a time, not one per request; the requests that wait for
//     it are not marked as waiting for a database connection of their own.
//
// The round records what it finds in its healthNotice in the order the
// rounds began (healthNotice.current): a round over a snapshot taken before a
// reload that ends after a round over the new snapshot applied its findings
// is dropped, so a reload in the middle of concurrent requests logs each
// change once.
type healthRound struct {
	key  healthRoundKey
	seq  uint64
	done chan struct{}
	// services is written once, before done is closed, and read only after.
	services map[string]string
}

// healthRoundKey names the inputs of a round: the database pool, the backend
// snapshot (its first element and length — a snapshot is immutable and a
// reload publishes a new slice) and whether the dream role is probed. A call
// whose inputs differ from the round in flight starts a round of its own.
type healthRoundKey struct {
	db    *pgxpool.Pool
	first *backends.Backend
	n     int
	dream bool
}

// healthRoles are the probed roles under their historical service keys
// ("chat" = the synthesis-serving class).
func healthRoles(dreamEnabled bool) []struct{ service, role string } {
	roles := []struct{ service, role string }{
		{"embed", backends.RoleEmbed},
		{"chat", backends.RoleSynthesis},
	}
	if dreamEnabled {
		roles = append(roles, struct{ service, role string }{"dream", backends.RoleDream})
	}
	return roles
}

// observe returns the services of the round over (db, the current snapshot
// of bp, dreamEnabled): the round in flight over the same inputs, or a new
// one. It returns when the round has ended, or with every service "error"
// when ctx ends first — the caller left; the round goes on and records what
// it finds. A caller whose context has already ended starts no round.
func (n *healthNotice) observe(ctx context.Context, db *pgxpool.Pool, bp *backends.Pool, dreamEnabled bool) map[string]string {
	if ctx.Err() != nil {
		return unansweredServices(dreamEnabled)
	}
	r := n.round(db, bp, dreamEnabled)
	select {
	case <-r.done:
		return maps.Clone(r.services)
	case <-ctx.Done():
		return unansweredServices(dreamEnabled)
	}
}

// unansweredServices is the services map of a caller that left before its
// round ended: every service "error", as a cancelled probe reported before.
func unansweredServices(dreamEnabled bool) map[string]string {
	roles := healthRoles(dreamEnabled)
	out := make(map[string]string, len(roles)+1)
	out["database"] = "error"
	for _, r := range roles {
		out[r.service] = "error"
	}
	return out
}

// round returns the round in flight over (db, bp's current snapshot,
// dreamEnabled) or starts one. The snapshot is read under n.mu, where the
// round gets its sequence number, so the order of the rounds is the order of
// their snapshots.
func (n *healthNotice) round(db *pgxpool.Pool, bp *backends.Pool, dreamEnabled bool) *healthRound {
	n.mu.Lock()
	defer n.mu.Unlock()
	var snap []backends.Backend
	if bp != nil {
		snap = bp.Snapshot()
	}
	key := healthRoundKey{db: db, n: len(snap), dream: dreamEnabled}
	if len(snap) > 0 {
		key.first = &snap[0]
	}
	if r := n.rounds[key]; r != nil {
		return r
	}
	n.started++
	r := &healthRound{key: key, seq: n.started, done: make(chan struct{})}
	if n.rounds == nil {
		n.rounds = make(map[healthRoundKey]*healthRound)
	}
	n.rounds[key] = r
	go n.run(r, db, snap, dreamEnabled, healthProbeBudget)
	return r
}

// run probes the database and every role of r concurrently (#265) — a base
// URL that several roles share is requested once (hostProbes) — records what
// it finds and publishes the services.
func (n *healthNotice) run(r *healthRound, db *pgxpool.Pool, snap []backends.Backend, dreamEnabled bool, budget time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	roles := healthRoles(dreamEnabled)
	var (
		wg      sync.WaitGroup
		dbErr   error
		reached = make([]string, len(roles)) // one slot per goroutine, read after Wait
		probes  = newHostProbes()
	)
	wg.Go(func() { dbErr = db.Ping(ctx) })
	for i, role := range roles {
		wg.Go(func() { reached[i] = n.roleReachable(ctx, r.seq, snap, role.role, probes) })
	}
	if !dreamEnabled {
		n.forgetRole(r.seq, backends.RoleDream)
	}
	wg.Wait()
	n.database(r.seq, dbErr)

	services := make(map[string]string, len(roles)+1)
	services["database"] = "ok"
	if dbErr != nil {
		services["database"] = "error"
	}
	for i, role := range roles {
		services[role.service] = reached[i]
	}
	n.mu.Lock()
	r.services = services
	if n.rounds[r.key] == r {
		delete(n.rounds, r.key)
	}
	n.mu.Unlock()
	close(r.done)
}

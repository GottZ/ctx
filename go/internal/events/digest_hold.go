package events

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/config"
)

// digestHold keeps the background digest from retrying a failure that cannot
// heal (issue #518). A failed run leaves digestPending set, and the digest
// ticker looks at it every five seconds: once the debounce after the last
// write had passed, every tick ran the whole digest again — the corpus scan
// and the map's write — into the same error, with an ERROR line each time.
//
// A failure is one that cannot heal (cannotHeal) when the database refused a
// value: the same statement over the same corpus fails the same way, the next
// tick included. Such a tenant is held: its digest does not run again until a
// block is written (the scheduler's write generation moves, NotifyWrite) or
// the tenant's configuration generation changes (a setting; digestTick looks
// for that while nothing else is pending). It is logged once when the tenant
// starts failing this way — an ERROR — and once when its digest writes the map
// again — an INFO; a held run that fails the same way again after a write logs
// at DEBUG. Every other failure (the database or a connection gone, a timeout,
// a lock, an error that is not the database's) can go differently on the next
// tick: it keeps the retry it always had.
//
// The process's own state: each ctxd instance holds and logs for itself.
type digestHold struct {
	mu   sync.Mutex
	held map[string]digestHeld // tenant scope → the state its last run failed in
}

// digestHeld is the state a tenant's digest failed in for good.
type digestHeld struct {
	gen uint64         // the scheduler's write generation when the run started
	cfg *config.Config // the tenant's configuration generation the run used
}

// cannotHeal returns the SQLSTATE of a failure that cannot heal by itself —
// the database refused a value: data exceptions (class 22, such as a byte
// sequence that is not UTF-8) and exceeded program limits (class 54, such as
// a tsvector over 1 MB, the failure of issue #518) — and "" for any other.
func cannotHeal(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 && (pgErr.Code[:2] == "22" || pgErr.Code[:2] == "54") {
		return pgErr.Code
	}
	return ""
}

// holds reports whether tenant's digest is held: its last run failed for good
// and neither a write nor its configuration generation changed since.
func (h *digestHold) holds(tenant string, gen uint64, cfg *config.Config) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.held[tenant]
	return ok && e.gen == gen && e.cfg == cfg
}

// settle records the outcome of tenant's run, started at write generation gen
// over configuration cfg, logs a change of state, and reports whether the run
// is done with: true for a success and for a failure that cannot heal, false
// for a failure the next tick retries.
func (h *digestHold) settle(tenant, homeScope string, gen uint64, cfg *config.Config, err error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, wasHeld := h.held[tenant]
	code := cannotHeal(err)
	switch {
	case err == nil:
		if wasHeld {
			delete(h.held, tenant)
			slog.Info("scheduler: digest writes the topic map again", "scope", homeScope)
		}
		return true
	case code != "":
		if h.held == nil {
			h.held = make(map[string]digestHeld)
		}
		h.held[tenant] = digestHeld{gen: gen, cfg: cfg}
		if wasHeld {
			slog.Debug("scheduler: digest failed again on a value the database cannot store", "scope", homeScope,
				"sqlstate", code, "error", err)
		} else {
			slog.Error("scheduler: digest failed on a value the database cannot store — not retried until a block is "+
				"written or a setting changes; this line repeats only after the digest wrote its map in between",
				"scope", homeScope, "sqlstate", code, "error", err)
		}
		return true
	default:
		delete(h.held, tenant)
		slog.Error("scheduler: digest error", "error", err)
		return false
	}
}

// retain forgets the held tenants that are not among tenants (a tenant
// suspended or gone since).
func (h *digestHold) retain(tenants []backgroundTenant) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for scope := range h.held {
		found := false
		for _, bt := range tenants {
			if bt.scope == scope {
				found = true
				break
			}
		}
		if !found {
			delete(h.held, scope)
		}
	}
}

// configChanged reports whether the configuration generation of a held tenant
// moved since its run failed.
func (h *digestHold) configChanged(ctx context.Context, store *config.Store) bool {
	h.mu.Lock()
	held := make(map[string]*config.Config, len(h.held))
	for scope, e := range h.held {
		held[scope] = e.cfg
	}
	h.mu.Unlock()
	for scope, cfg := range held {
		if store.SnapshotForTenant(ctx, scope) != cfg {
			return true
		}
	}
	return false
}

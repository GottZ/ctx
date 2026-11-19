package handler

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
)

// The dream_queue section of `manage stats` (issue #271). dream.QueueDepth
// counts the dream-eligible blocks of the read scopes against the clock
// (cooldown windows), so no write-maintained counter carries it, and it is a
// pass over those blocks: 155 ms at 100 000 blocks, 457 ms at 300 000. Every
// stats call paid it — the statusline among them, once per 10 s per key on a
// 500 ms budget, which it overran from about 300 000 blocks on and then lost
// its block count, while it reads nothing of the section.
//
// The section is now served from a cache per read-scope set, the way
// /api/status serves its dream section (StatusCollector, same setting
// events.queue_stats_interval): a value younger than the interval is answered
// as it is; an older one is answered as well and refreshed in the background,
// one refresh per scope set at a time. Only the first call for a scope set —
// and the first after its entry was evicted — waits for the count. The
// forecast's windows are an hour and longer, so a value up to an interval
// (plus one count) old says the same. The shape of the answer is unchanged.
//
// The key is the exact, ordered set of read scopes plus the dream-linkable
// types of the request's policy snapshot: a key's answer only ever holds the
// counts of its own scopes, as before.

// dreamQueueMaxAgeIntervals bounds how stale an answered value may be: an
// entry counted more than this many intervals ago (no call kept it fresh, or
// its refreshes failed) is counted again before the answer, as on a cold
// start.
const dreamQueueMaxAgeIntervals = 10

// dreamQueueCacheEntries bounds the cache: one entry per distinct read-scope
// set and linkable-type set, a few hundred bytes each. Past it the entry used
// longest ago goes. A fixed number, not policy: it bounds ctx's own memory.
const dreamQueueCacheEntries = 1024

type dreamQueueEntry struct {
	q       *dream.QueueStats
	at      time.Time // when q was counted
	used    time.Time // last answer served from it
	pending bool      // a background refresh is running
}

// dreamQueueCache is the per-scope-set cache of the stats dream_queue section.
type dreamQueueCache struct {
	depth queueDepthFn
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*dreamQueueEntry
}

func newDreamQueueCache(depth queueDepthFn) *dreamQueueCache {
	return &dreamQueueCache{depth: depth, now: time.Now, entries: map[string]*dreamQueueEntry{}}
}

func dreamQueueKey(scopes, linkable []string) string {
	s := slices.Clone(scopes)
	slices.Sort(s)
	l := slices.Clone(linkable)
	slices.Sort(l)
	return strings.Join(s, "\x1f") + "\x1e" + strings.Join(l, "\x1f")
}

// get answers the dream queue of scopes, at most interval old unless a count
// is under way in the background, and never older than
// dreamQueueMaxAgeIntervals intervals. interval <= 0 counts on every call.
func (c *dreamQueueCache) get(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string, interval time.Duration) (*dream.QueueStats, error) {
	if interval <= 0 {
		return c.depth(ctx, pool, scopes, linkable)
	}
	key := dreamQueueKey(scopes, linkable)
	c.mu.Lock()
	if e, ok := c.entries[key]; ok {
		now := c.now()
		if age := now.Sub(e.at); age < dreamQueueMaxAgeIntervals*interval {
			e.used = now
			if age >= interval && !e.pending {
				e.pending = true
				go c.refresh(context.WithoutCancel(ctx), pool, key, slices.Clone(scopes), slices.Clone(linkable))
			}
			q := e.q
			c.mu.Unlock()
			return q, nil
		}
	}
	c.mu.Unlock()

	q, err := c.depth(ctx, pool, scopes, linkable)
	if err != nil {
		return nil, err
	}
	c.store(key, q)
	return q, nil
}

// refresh counts one scope set again in the background and stores the result;
// a failed count keeps the old value and is logged.
func (c *dreamQueueCache) refresh(ctx context.Context, pool *pgxpool.Pool, key string, scopes, linkable []string) {
	q, err := c.depth(ctx, pool, scopes, linkable)
	if err != nil {
		slog.Warn("manage: dream queue refresh failed", "error", err)
		c.mu.Lock()
		if e, ok := c.entries[key]; ok {
			e.pending = false
		}
		c.mu.Unlock()
		return
	}
	c.store(key, q)
}

func (c *dreamQueueCache) store(key string, q *dream.QueueStats) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if e, ok := c.entries[key]; ok {
		e.q, e.at, e.pending = q, now, false
		return
	}
	if len(c.entries) >= dreamQueueCacheEntries {
		var oldest string
		var oldestUsed time.Time
		for k, e := range c.entries {
			if oldest == "" || e.used.Before(oldestUsed) {
				oldest, oldestUsed = k, e.used
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[key] = &dreamQueueEntry{q: q, at: now, used: now}
}

// statsDreamQueue is the dream_queue section of `manage stats` for the
// caller's read scopes, through the cache at events.queue_stats_interval (a
// value <= 0 means 30 s, as for the /api/status collector). A handler without
// a config or without the cache counts on every call.
func (h *ManageHandler) statsDreamQueue(ctx context.Context, readScopes []string) (*dream.QueueStats, error) {
	linkable := h.dreamLinkableTypes(ctx)
	if h.dreamQueue == nil || h.cfg == nil {
		return dream.QueueDepth(ctx, h.pool, readScopes, linkable)
	}
	interval := h.cfg.Snapshot().Events.QueueStatsInterval //nolint:forbidigo // MT 06 BLIND: the dream-queue cache cadence is a server-global process setting (global-only), as for the /api/status collector.
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return h.dreamQueue.get(ctx, h.pool, readScopes, linkable, interval)
}

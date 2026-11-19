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
// as it is; an older one is answered as well and counted again in the
// background, one count per scope set at a time. A call without a usable value
// (the first for a scope set, the first after its entry was evicted, or after
// ten intervals without one) waits for the count. That count is detached from
// the request: a caller that gives up before it ends (the statusline after its
// 500 ms) leaves the value for the next call instead of cancelling the count.
// The forecast's windows are an hour and longer, so a value up to an interval
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
	q    *dream.QueueStats // nil until the first count succeeded
	at   time.Time         // when the count behind q started
	used time.Time         // last call for this key

	// The count under way, if any: one per key, detached from the request
	// that started it, so a caller that gives up (the statusline's 500 ms
	// budget) still leaves the value for the next one. done closes when it
	// ends; err is its error.
	pending bool
	done    chan struct{}
	err     error
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

// get answers the dream queue of scopes: the cached value while it is younger
// than dreamQueueMaxAgeIntervals intervals (one older than interval starts a
// count in the background), otherwise the result of a count it starts or
// joins. The caller waits for that count until its context ends; the count
// itself runs on and stores its value.
func (c *dreamQueueCache) get(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string, interval time.Duration) (*dream.QueueStats, error) {
	key := dreamQueueKey(scopes, linkable)
	c.mu.Lock()
	now := c.now()
	e, ok := c.entries[key]
	if !ok {
		e = c.insertLocked(key)
	}
	e.used = now
	if e.q != nil && now.Sub(e.at) < dreamQueueMaxAgeIntervals*interval {
		if now.Sub(e.at) >= interval && !e.pending {
			c.startLocked(ctx, pool, e, scopes, linkable)
		}
		q := e.q
		c.mu.Unlock()
		return q, nil
	}
	if !e.pending {
		c.startLocked(ctx, pool, e, scopes, linkable)
	}
	done := e.done
	c.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	return e.q, nil
}

// startLocked starts the one count of e, detached from the request. A failed
// count keeps the old value and is logged. Caller holds c.mu.
func (c *dreamQueueCache) startLocked(ctx context.Context, pool *pgxpool.Pool, e *dreamQueueEntry, scopes, linkable []string) {
	done := make(chan struct{})
	e.pending, e.done = true, done
	started := c.now()
	go func(ctx context.Context, scopes, linkable []string) {
		q, err := c.depth(ctx, pool, scopes, linkable)
		if err != nil {
			slog.Warn("manage: dream queue count failed", "error", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if err == nil {
			e.q, e.at = q, started
		}
		e.err, e.pending = err, false
		close(done)
	}(context.WithoutCancel(ctx), slices.Clone(scopes), slices.Clone(linkable))
}

// insertLocked adds an empty entry for key, evicting the entry used longest
// ago when the cache is full. Caller holds c.mu.
func (c *dreamQueueCache) insertLocked(key string) *dreamQueueEntry {
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
	e := &dreamQueueEntry{}
	c.entries[key] = e
	return e
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

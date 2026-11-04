package handler

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// The write-rejection log of POST /api/store (issue #66).
//
// A write the handler refuses after the body parsed — every gate of
// runStageWriteGates, the parent pre-flight, the I7/S3 refusal out of
// store.UpsertBlock, the post-upsert link — used to be answered on the wire
// alone. The request logger names method, path, status and request id, not
// the key, so a key held at its write budget or an agent writing into a
// reserved category lost its blocks with nothing on the server naming who;
// and `ctx persist --hook` is silent by design (#59/#62), so the client log
// cannot either. This line is the attribution: a WARN per refusal — at most
// one per key, code and window, see THE THROTTLE below — with
//
//	api_key_id  the key's UUID (AuthResult.ApiKeyID — never its hash or plaintext)
//	code        the machine code of the rejection class (errcode.go)
//	status      the HTTP status answered
//	request_id  the id the request logger's line carries too
//	category    the write's category as sent, capped at writeRejectLogFieldMax runes
//	scope       the write's scope as REQUESTED ("" = the key's home scope), same cap
//	suppressed  refusals of this key and code the throttle swallowed since the
//	            pair's previous line
//
// and nothing of the block: no title, no content, no metadata, no tags, and not
// the rejection's prose either (the code is the verdict; some messages quote
// client values). A server fault (500 internal) is on this line too — the key
// lost a write all the same — while its cause stays on the ERROR line its call
// site writes under the same request id.
//
// THE THROTTLE. A refusal is cheap to repeat and most classes repeat by
// construction: once a key is over its budget every write of the window is a
// 429, and a refused write never books a write row (store.LogAccess runs on
// success only), so the write budget bounds no refusal. One line per request
// would let a looping client fill the log — the reason upsertFailureReject
// keeps refusals off the error log at the target scale. So the line is
// throttled per (key, code): the first refusal of a window gets its line,
// the rest of the window is counted. Per pair and not per key, because a
// per-key bound would let one cause hide another — a rate-limit storm masking
// the reserved_category refusal of a misconfigured agent of the same key for
// the whole window. The volume is bounded by keys × codes per window.
//
// Nothing is dropped while the process runs: the count reaches the log on the
// pair's next line, or — for a pair that stays quiet — on a
// writeRejectSuppressedMsg line written by the sweep. The sweep runs at most
// once per window, on the next refusal of ANY key (so a quiet pair's tail
// waits for that refusal), and forgets every pair whose window is over, so the
// state holds only the pairs of the last two windows. The counts live in
// memory: a restart drops what is pending.

const (
	// writeRejectLogWindow is the throttle window. One minute because that is
	// the window of the write rate limit itself (store.CheckRateLimitByAction
	// counts the last 60 s): a key held at its budget logs once per budget
	// window, per code. Hardcoded on purpose, not a setting — it bounds the
	// process's own log volume, not anybody's policy (the precedent of the
	// llmlog write-path throttle, llmlog.go lossWarnInterval).
	writeRejectLogWindow = time.Minute
	// writeRejectLogFieldMax caps the client-chosen strings on the line, in
	// runes. 100 is blockSizeLimit's category cap: a category that passed it is
	// logged whole, and a size_cap refusal of an oversized one cannot size the
	// line.
	writeRejectLogFieldMax = 100

	writeRejectMsg           = "store: write rejected"
	writeRejectSuppressedMsg = "store: write rejections suppressed"
)

// writeRejectPair is the throttle's unit: one key, one rejection class.
type writeRejectPair struct{ keyID, code string }

// writeRejectTally is a pair's state: when its last line was written, and how
// many refusals got none since.
type writeRejectTally struct {
	logged     time.Time
	suppressed int64
}

// writeRejectLog is the throttled rejection log of one StoreHandler. The zero
// value is ready to use; it must not be copied after first use.
type writeRejectLog struct {
	mu    sync.Mutex
	pairs map[writeRejectPair]*writeRejectTally
	swept time.Time        // last sweep; zero until the first refusal
	now   func() time.Time // nil ⇒ time.Now (test seam)
}

// record logs rej for the key — or counts it, inside the pair's window.
func (l *writeRejectLog) record(ctx context.Context, keyID, category, scope string, rej *writeReject) {
	logged, suppressed, flushed := l.admit(keyID, rej.Code)
	for _, f := range flushed {
		slog.Warn(writeRejectSuppressedMsg,
			"api_key_id", f.pair.keyID,
			"code", f.pair.code,
			"suppressed", f.suppressed,
		)
	}
	if !logged {
		return
	}
	slog.Warn(writeRejectMsg,
		"api_key_id", keyID,
		"code", rej.Code,
		"status", rej.Status,
		"request_id", RequestIDFromContext(ctx),
		"category", truncateRunes(category, writeRejectLogFieldMax),
		"scope", truncateRunes(scope, writeRejectLogFieldMax),
		"suppressed", suppressed,
	)
}

// writeRejectFlush is a quiet pair's count, handed out by the sweep.
type writeRejectFlush struct {
	pair       writeRejectPair
	suppressed int64
}

// admit is the throttle decision, under the lock; the logging happens after
// it, outside. The pair's own decision comes BEFORE the sweep, so a pair that
// recurs after its window carries its count on its own line and is never also
// flushed.
func (l *writeRejectLog) admit(keyID, code string) (logged bool, suppressed int64, flushed []writeRejectFlush) {
	now := time.Now()
	if l.now != nil {
		now = l.now()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pairs == nil {
		l.pairs = make(map[writeRejectPair]*writeRejectTally)
		l.swept = now
	}

	p := writeRejectPair{keyID: keyID, code: code}
	t := l.pairs[p]
	switch {
	case t != nil && now.Sub(t.logged) < writeRejectLogWindow:
		t.suppressed++
	case t != nil:
		logged, suppressed = true, t.suppressed
		t.logged, t.suppressed = now, 0
	default:
		logged = true
		l.pairs[p] = &writeRejectTally{logged: now}
	}

	if now.Sub(l.swept) >= writeRejectLogWindow {
		for q, tq := range l.pairs {
			if now.Sub(tq.logged) < writeRejectLogWindow {
				continue
			}
			if tq.suppressed > 0 {
				flushed = append(flushed, writeRejectFlush{pair: q, suppressed: tq.suppressed})
			}
			delete(l.pairs, q)
		}
		l.swept = now
	}
	return logged, suppressed, flushed
}

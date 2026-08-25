// read.go — the read surface. Every method here is one autocommit statement:
// no BEGIN, no snapshot, nothing that survives the call. A reader that holds a
// read mark across batches keeps the foreign writer's -wal from being
// checkpointed back, and a distillation run spans minutes of inference plus
// park phases. Batch consistency comes from the monotone id range instead.
//
// Source: https://github.com/GottZ/ctx
package hermesstate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// toolRowsTemplate is the batch read. %s carries the strategy hint.
//
// The predicate order is the one the planner is asked to honour: session first
// (equality), then the id range. compacted/role are residual filters — there is
// no index on either column in any hermes schema, and adding one would be a
// write to foreign territory.
const toolRowsTemplate = `SELECT id, COALESCE(tool_name,''), COALESCE(content,''), timestamp
  FROM messages
 WHERE %ssession_id = ?
   AND compacted    = 1
   AND role         = 'tool'
   AND id           > ?
 ORDER BY id ASC
 LIMIT ?`

// hasNewTemplate is the existence probe behind gate 4. It deliberately avoids
// max(): a maximum over a session forces a walk of every entry of that session
// in whichever index is chosen, on every tick, although in the overwhelming
// majority of ticks there is nothing new at all. LIMIT 1 stops at the first hit.
const hasNewTemplate = `SELECT id FROM messages
 WHERE %ssession_id = ? AND compacted = 1 AND id > ?
 LIMIT 1`

// maxCompactedTemplate is the B8 regression check — and ONLY that. It is the
// expensive shape the existence probe exists to avoid, so it runs once per run,
// not once per tick.
const maxCompactedTemplate = `SELECT COALESCE(max(id), 0) FROM messages
 WHERE %ssession_id = ? AND compacted = 1`

// backfillTemplate resolves the initial watermark of a source: the id of the
// N-th archived row of THIS session counted from its head. OFFSET counts ROWS,
// which is the whole point — see WatermarkFrom.
const backfillTemplate = `SELECT id FROM messages
 WHERE %ssession_id = ? AND compacted = 1
 ORDER BY id DESC
 LIMIT 1 OFFSET ?`

// newestActiveSQL asks for the newest live message of a session. No strategy
// hint here: this one is ordered by timestamp, so idx_messages_session
// (session_id, timestamp) and idx_messages_session_active are exactly the right
// indexes for it — the very indexes the id-ordered reads must avoid.
const newestActiveSQL = `SELECT timestamp FROM messages
 WHERE session_id = ? AND active = 1
 ORDER BY timestamp DESC
 LIMIT 1`

// sessionsSQL lists candidate sessions with their newest active message time.
// It carries NO row count: an unbounded count per candidate per tick is a cost
// the ledger reports after a run, not something to pay before it.
const sessionsSQL = `SELECT s.id,
       COALESCE(s.parent_session_id, ''),
       (SELECT m.timestamp FROM messages m
         WHERE m.session_id = s.id AND m.active = 1
         ORDER BY m.timestamp DESC LIMIT 1)
  FROM sessions s`

// ToolRow is one archived tool result, decoded.
type ToolRow struct {
	ID       int64
	ToolName string
	// Content is decoded: a "\x00json:" payload is folded to its text parts,
	// and no NUL byte survives. A row whose payload does not parse is dropped
	// rather than passed on.
	Content string
	TS      float64
}

// Time is the row's timestamp as a wall clock value.
func (r ToolRow) Time() time.Time {
	sec, frac := math.Modf(r.TS)
	return time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
}

// SessionInfo is one candidate session.
type SessionInfo struct {
	ID     string
	RootID string
	// NewestActive is the zero value when the session has no live rows left.
	NewestActive time.Time
}

func (s *Source) toolRowsSQL() string { return fmt.Sprintf(toolRowsTemplate, s.hint()) }
func (s *Source) hasNewSQL() string   { return fmt.Sprintf(hasNewTemplate, s.hint()) }

// MaxCompactedID returns the highest archived id of the session. It is used
// ONLY for the watermark-regression check, never as a "is there new material"
// probe — that is HasNewArchived.
func (s *Source) MaxCompactedID(ctx context.Context, sessionID string) (int64, error) {
	var id int64
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(maxCompactedTemplate, s.hint()), sessionID)
	if err := row.Scan(&id); err != nil {
		return 0, classify(err)
	}
	return id, nil
}

// GlobalMaxID is the table-wide max(rowid) — O(1) in SQLite and the cheap
// pre-gate of a tick: is there ANY row above the watermark, for anyone?
func (s *Source) GlobalMaxID(ctx context.Context) (int64, error) {
	var id int64
	row := s.db.QueryRowContext(ctx, "SELECT COALESCE(max(id), 0) FROM messages")
	if err := row.Scan(&id); err != nil {
		return 0, classify(err)
	}
	return id, nil
}

// HasNewArchived answers "is there archived material above after" with an
// existence probe that stops at the first hit — no maximum, no count.
func (s *Source) HasNewArchived(ctx context.Context, sessionID string, after int64) (bool, error) {
	var id int64
	row := s.db.QueryRowContext(ctx, s.hasNewSQL(), sessionID, after)
	switch err := row.Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, classify(err)
	}
	return true, nil
}

// WatermarkFrom derives the initial watermark of a source: the exclusive lower
// bound a first run should start at, such that exactly backfillRows archived
// rows OF THIS SESSION lie above it.
//
// backfillRows = 0 means "start at the head": the returned id is the newest
// archived row, so nothing at all is reprocessed.
//
// The naive alternative — max_compacted_id − backfillRows — is wrong, and not
// marginally so: `id` is a GLOBAL autoincrement across every session in the
// file (307 sessions sharing one id space in the live store), so subtracting a
// row count from a global id yields an id delta, which under interleaved
// sessions is a fraction of the rows asked for. The key is named "rows", so it
// has to mean rows.
//
// When the session holds fewer than backfillRows+1 archived rows there is no
// such id and the result is 0 — start from the beginning, which is exactly the
// intent of asking for more history than exists.
func (s *Source) WatermarkFrom(ctx context.Context, sessionID string, backfillRows int) (int64, error) {
	if backfillRows < 0 {
		backfillRows = 0
	}
	var id int64
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(backfillTemplate, s.hint()), sessionID, backfillRows)
	switch err := row.Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, classify(err)
	}
	return id, nil
}

// ToolRows reads archived tool rows in (after, …] id order, capped by limit. It
// returns the rows, the number of rows dropped because their encoded content
// did not parse, and an error.
func (s *Source) ToolRows(ctx context.Context, sessionID string, after int64, limit int) ([]ToolRow, int, error) {
	if limit <= 0 {
		return nil, 0, nil
	}
	rows, err := s.db.QueryContext(ctx, s.toolRowsSQL(), sessionID, after, limit)
	if err != nil {
		return nil, 0, classify(err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ToolRow, 0, limit)
	dropped := 0
	for rows.Next() {
		var (
			r   ToolRow
			raw string
		)
		if err := rows.Scan(&r.ID, &r.ToolName, &raw, &r.TS); err != nil {
			return nil, dropped, classify(err)
		}
		text, ok := decodeContent(raw)
		if !ok {
			dropped++
			continue
		}
		r.Content = text
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, dropped, classify(err)
	}
	return out, dropped, nil
}

// QuietFor reports how long the session's newest ACTIVE message has been
// standing. A session with no active rows left yields ErrNoActiveRows — there
// is nothing to measure, and inventing a duration would let a caller's idle
// gate read a fully archived session as "busy" or "idle" by accident.
//
// A timestamp in the future (hermes orders by id rather than timestamp for
// exactly this reason: a clock regression under WSL2) is clamped to zero rather
// than reported as a negative idle time.
func (s *Source) QuietFor(ctx context.Context, sessionID string, now time.Time) (time.Duration, error) {
	var ts float64
	row := s.db.QueryRowContext(ctx, newestActiveSQL, sessionID)
	switch err := row.Scan(&ts); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, ErrNoActiveRows
	case err != nil:
		return 0, classify(err)
	}
	d := now.Sub(unixFloat(ts))
	if d < 0 {
		d = 0
	}
	return d, nil
}

// Sessions lists candidate sessions: id, the root of the parent chain, and the
// newest active message time.
func (s *Source) Sessions(ctx context.Context) ([]SessionInfo, error) {
	rows, err := s.db.QueryContext(ctx, sessionsSQL)
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = rows.Close() }()

	var (
		order  []string
		parent = map[string]string{}
		newest = map[string]sql.NullFloat64{}
	)
	for rows.Next() {
		var (
			id, par string
			ts      sql.NullFloat64
		)
		if err := rows.Scan(&id, &par, &ts); err != nil {
			return nil, classify(err)
		}
		order = append(order, id)
		parent[id] = par
		newest[id] = ts
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}

	out := make([]SessionInfo, 0, len(order))
	for _, id := range order {
		si := SessionInfo{ID: id, RootID: rootOf(id, parent)}
		if ts := newest[id]; ts.Valid {
			si.NewestActive = unixFloat(ts.Float64)
		}
		out = append(out, si)
	}
	return out, nil
}

// rootOf walks the parent chain to its origin. The walk is bounded by the
// number of known sessions and remembers where it has been: a state.db is
// foreign data, and a parent cycle in it must cost a bounded loop, not a hang.
func rootOf(id string, parent map[string]string) string {
	seen := map[string]bool{id: true}
	cur := id
	for range len(parent) {
		p := parent[cur]
		if p == "" || p == cur || seen[p] {
			return cur
		}
		if _, known := parent[p]; !known {
			// The chain leaves the set of rows we read; the last known member
			// is the best-supported answer.
			return cur
		}
		seen[p] = true
		cur = p
	}
	return cur
}

// unixFloat converts hermes' REAL unix timestamp into a wall clock value.
func unixFloat(ts float64) time.Time {
	sec, frac := math.Modf(ts)
	return time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
}

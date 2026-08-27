// Package hermesadapter presents a hermes state.db, read through
// internal/hermesstate, as a distillsource.Source.
//
// It lives in its own package for one reason, and the reason is checked rather
// than asserted: importing internal/hermesstate pulls modernc.org/sqlite into
// the dependency tree, and the abstraction must not know the driver. With the
// adapter here, `go list -deps ./internal/distillsource` carries no SQLite at
// all — a property of the import graph, not of a grep over source text.
//
// internal/hermesstate is NOT modified by this package and does not know it
// exists. Everything that has to be reconciled between the two shapes is
// reconciled here:
//
//   - Error classes are LIFTED, not aliased. The two taxonomies carry the same
//     strings but not the same values; a caller must be able to switch on
//     distillsource alone.
//   - The readable unit is the state.db session row, not the root of its
//     parent chain: hermesstate reads by session_id (read.go:27, :39, :46) and
//     offers no root-scoped read, so a chain yields one unit per member. The
//     root id it does report (SessionInfo.RootID, read.go:95) has no consumer
//     that could honour it here.
//   - hermes reads only archived tool results (role='tool' AND compacted=1,
//     read.go:25-32), so every item is a tool result and every role is known
//     without reading it out of the row.
//
// Source: https://github.com/GottZ/ctx
package hermesadapter

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/hermesstate"
	"github.com/GottZ/ctx/internal/promptguard"
)

// roleTool is the role of every row this source hands out. It is asserted by
// the batch query itself (role = 'tool', read.go:29) and is therefore code, not
// a value read back out of foreign data.
const roleTool = "tool"

// adapter is the wrapper. It holds the handle and nothing else: every decision
// is either the wrapped source's or a constant of this file, so there is no
// state that could drift between two calls.
type adapter struct {
	src *hermesstate.Source
}

// The contract this package exists to satisfy, checked at compile time.
var _ distillsource.Source = (*adapter)(nil)

// New wraps an opened hermesstate handle. Ownership passes to the adapter:
// Close closes the wrapped source, because a distillsource.Source owns what it
// was built over.
func New(src *hermesstate.Source) distillsource.Source { return &adapter{src: src} }

// Label is the name the handle was opened under.
func (a *adapter) Label() string { return a.src.Label() }

// Close releases the wrapped handle.
func (a *adapter) Close() error { return a.src.Close() }

// Sessions lists the state.db sessions, newest live activity first. A session
// whose rows are all archived carries the zero time and therefore sorts last —
// it has no live activity to rank by, and dropping it here would hide material
// that is still perfectly readable.
//
// Every Ref carries Watermark 0: naming a head per candidate is a max() per
// session per tick, which is the cost hermesstate's session listing was
// written to avoid (read.go:65-73). A caller that needs the head asks Head.
func (a *adapter) Sessions(ctx context.Context) ([]distillsource.Ref, error) {
	infos, err := a.src.Sessions(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	slices.SortStableFunc(infos, func(x, y hermesstate.SessionInfo) int {
		return y.NewestActive.Compare(x.NewestActive)
	})
	out := make([]distillsource.Ref, 0, len(infos))
	for _, si := range infos {
		out = append(out, distillsource.Ref{Session: si.ID})
	}
	return out, nil
}

// HasNew is the existence probe over archived rows.
func (a *adapter) HasNew(ctx context.Context, sess string, after int64) (bool, error) {
	ok, err := a.src.HasNewArchived(ctx, sess, after)
	return ok, mapErr(err)
}

// Head is the highest archived id of the session — the regression check, never
// the "is there anything new" probe.
func (a *adapter) Head(ctx context.Context, sess string) (int64, error) {
	id, err := a.src.MaxCompactedID(ctx, sess)
	return id, mapErr(err)
}

// QuietFor reports how long the session's newest live message has been
// standing. A fully archived session yields distillsource.ErrNoActiveRows.
func (a *adapter) QuietFor(ctx context.Context, sess string, now time.Time) (time.Duration, error) {
	d, err := a.src.QuietFor(ctx, sess, now)
	return d, mapErr(err)
}

// Read returns at most maxItems archived tool results above after, each capped
// to maxRunes runes.
//
// maxItems is honoured EXACTLY here, with none of the atom overshoot the
// contract allows: this source's indivisible unit is a single archived row, so
// a row count is also an item count and the cap can always be met. The
// exception in the contract exists for sources whose atom spans several rows —
// a manifest with its parts — and it does not apply to this one.
//
// Three properties are worth naming, because each one is a decision rather
// than a translation:
//
//  1. The watermark is taken from the ROWS, not from the items. A row whose
//     payload folded to nothing (an image-only multimodal content, hermesstate
//     read_test.go:386-388) produces no item but is covered all the same, and
//     re-reading it every tick would be a permanent stall dressed up as
//     progress.
//  2. Complete is true whenever a row was read. A hermes watermark is the
//     INTEGER PRIMARY KEY of messages, so a watermark group is exactly one row
//     and can never be cut between two batches — completeness here is a
//     property of the key, not a check that could fail.
//  3. A window in which EVERY row was dropped as undecodable is the one case
//     the adapter cannot name a watermark for: ToolRows returns the drop COUNT
//     and not the highest scanned id (read.go:175), so the covered range is
//     unknown. Complete is false and the watermark does not move — guessing
//     would skip material for good, standing still costs a repeat.
func (a *adapter) Read(ctx context.Context, sess string, after int64, maxItems, maxRunes int) (distillsource.Batch, error) {
	if maxItems <= 0 || maxRunes <= 0 {
		// Nothing was read, so nothing is covered. A source never substitutes
		// a cap of its own for a caller's missing one.
		return distillsource.Batch{Watermark: after}, nil
	}
	rows, dropped, err := a.src.ToolRows(ctx, sess, after, maxItems)
	if err != nil {
		return distillsource.Batch{Watermark: after}, mapErr(err)
	}
	if len(rows) == 0 {
		return distillsource.Batch{Watermark: after, Complete: dropped == 0}, nil
	}
	b := distillsource.Batch{
		Items:     make([]distillsource.Item, 0, len(rows)),
		Watermark: rows[len(rows)-1].ID,
		Complete:  true,
	}
	for _, r := range rows {
		text, truncated := headCap(r.Content, maxRunes)
		if text == "" {
			continue
		}
		b.Items = append(b.Items, distillsource.Item{
			Text:      text,
			Truncated: truncated,
			// Code-owned attributes only: a row id and a constant. The row's
			// tool_name is foreign text and never becomes a marker value.
			Attrs: []promptguard.Attr{
				{Name: "row", Value: strconv.FormatInt(r.ID, 10)},
				{Name: "chunk", Value: "1"},
			},
			Origin: distillsource.Origin{RowID: r.ID, ChunkIndex: 1, Role: roleTool},
			// state.db carries no classification of its own. The unclassified
			// answer is the highest rank, stated explicitly rather than left to
			// the zero value (backends/trust.go:29-41).
			Sensitivity: backends.SensCredentials,
			// Foreign agent transcript, always.
			Untrusted: true,
		})
	}
	return b, nil
}

// headCap cuts s to at most maxRunes RUNES and reports whether anything was
// dropped. Runes, not bytes: a byte cut would split a multi-byte character and
// hand the model half of one, and the quote gate would then verify against text
// that no longer decodes the way the source read it.
func headCap(s string, maxRunes int) (string, bool) {
	n := 0
	for i := range s {
		if n == maxRunes {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// mapErr lifts hermesstate's error classes into the abstraction's. The wrapped
// error is kept inside for local diagnosis — it carries the driver's own text,
// which the caller logs and never persists (distillsource.go, error classes).
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, hermesstate.ErrSourceUnavailable):
		return fmt.Errorf("%w: %w", distillsource.ErrSourceUnavailable, err)
	case errors.Is(err, hermesstate.ErrSchemaUntrusted):
		return fmt.Errorf("%w: %w", distillsource.ErrSchemaUntrusted, err)
	case errors.Is(err, hermesstate.ErrNoActiveRows):
		return fmt.Errorf("%w: %w", distillsource.ErrNoActiveRows, err)
	default:
		// hermesstate classifies everything else — including a cancelled
		// context — as ErrQueryFailed (hermesstate.go:442-462), and so does
		// this default, deliberately: the two taxonomies stay aligned.
		return fmt.Errorf("%w: %w", distillsource.ErrQueryFailed, err)
	}
}

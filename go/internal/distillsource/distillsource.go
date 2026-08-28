// Package distillsource is the contract between the distillation arm and the
// stores it reads. It carries types and error classes only — no query, no
// driver, no configuration. Everything store-specific (SQLite strategies and
// plan assertions, SQL, WAL semantics, content decoding) lives behind an
// implementation and never appears here.
//
// Three properties hold for every implementation, and each one is a property
// of this contract rather than of a caller's discipline:
//
//  1. A watermark is strictly increasing within a session and addresses the
//     material as a half-open range (from, to]. A caller advances its journal
//     to Batch.Watermark and never to a value it derived itself.
//  2. A source never truncates silently. Material that does not fit the
//     caller's rune cap becomes SEVERAL items, or a head that is MARKED as
//     truncated — and the mark travels with the item, so a quote gate can
//     verify against exactly the text a model was shown.
//  3. A source never invents a classification. Sensitivity and Untrusted are
//     the SOURCE's answer about its own material; an implementation without an
//     answer reports the fail-closed one, never the convenient one.
//
// The package deliberately does not import internal/config or internal/events:
// a source is handed its values, it does not read them (F1 layering rule).
//
// Source: https://github.com/GottZ/ctx
package distillsource

import (
	"context"
	"errors"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/promptguard"
)

// Error classes. They are the strings a caller's journal records — a driver's
// own text is wrapped for local diagnosis but must never reach a persisted
// field, because the journal is readable over /api and lives for 90 days
// (migration 135_distill_run.sql:131-135).
//
// Two of the four are the journal's error taxonomy verbatim: query_failed and
// schema_untrusted are members of the dr_error_class_known CHECK
// (135_distill_run.sql:155-158). The other two are NOT error classes there and
// must be mapped by the caller, not passed through: source_unavailable is a
// skip (dr_skip_reason_known carries source_unreachable, :151) or one of
// open_failed / wal_index_unavailable, and no_active_rows is an answer about a
// gate, not a failure at all.
var (
	// ErrSourceUnavailable means the store could not be read right now:
	// absent, not permitted, or momentarily inaccessible. It is never a hard
	// failure — a source that produces nothing while it is unreachable is also
	// producing no new material to miss.
	ErrSourceUnavailable = errors.New("source_unavailable")

	// ErrSchemaUntrusted means the store's shape is not the shape the
	// implementation agreed to read.
	ErrSchemaUntrusted = errors.New("schema_untrusted")

	// ErrQueryFailed covers everything else.
	ErrQueryFailed = errors.New("query_failed")

	// ErrNoActiveRows is returned by QuietFor for a session that has no live
	// material to measure against. Inventing a duration here would let a
	// caller's idle gate read a fully archived session as "busy" or as "idle"
	// by accident, so the absence is reported instead.
	ErrNoActiveRows = errors.New("no_active_rows")
)

// Ref identifies one readable unit of a source, opaque to the arm.
//
// Session is the unit AS THE SOURCE ADDRESSES IT and is half of the journal's
// source_key: the root session id for the ctx checkpoint source, the state.db
// session row for the hermes adapter — that store has no root-scoped read, so
// a parent chain there yields one unit per member. Whatever Sessions returns
// here is a valid argument to HasNew, Head, Read and QuietFor; that is the
// invariant the field carries, not a claim about session genealogy.
type Ref struct {
	Session   string
	Watermark int64
}

// Item is ONE prompt-ready piece of foreign text. The source decides how it
// splits its material; the arm only ever sees items that already fit the rune
// cap it asked for.
type Item struct {
	// Text is decoded, normalized and ready to wrap. It is never empty: a unit
	// whose material folds to nothing produces no item at all.
	Text string

	// Truncated marks a head-capped item — material of this unit was dropped
	// by the source. A quote gate must treat Text as the whole of what the
	// model saw, and a dedup key must be built over Text, not over the unit.
	Truncated bool

	// Attrs are the marker attributes of the promptguard wrapper. They are
	// CODE-OWNED: every value here is produced by this code from a numeric id
	// or a constant, never lifted out of foreign content.
	Attrs []promptguard.Attr

	// Origin is the provenance, carried verbatim into the written block.
	Origin Origin

	// Manifest is the READ UNIT this item belongs to — the compaction whose
	// part list produced it. It is separate from Origin because Origin is the
	// CITATION anchor (the exact text a gate verified against) while this is
	// the coverage anchor: several items of several parts share one manifest,
	// and the block a caller writes cites the parts but is ABOUT the
	// compaction.
	//
	// A source whose read unit is not a manifest leaves it zero, which is the
	// honest answer rather than a synthesized one — the hermes adapter reads
	// one archived row at a time and has no such unit at all.
	Manifest Manifest

	// Sensitivity is the SOURCE's classification of this item. A source
	// without a classification reports the highest rank, because the empty
	// value is normatively rank 3 anyway (backends/trust.go:29-41) and an
	// implicit fail-closed is one refactor away from an accidental downgrade.
	Sensitivity backends.Sensitivity

	// Untrusted answers whether the source's own type carries
	// retrieval.untrusted. It is the source's answer, not the arm's guess.
	Untrusted bool
}

// Manifest names the compaction an item was read out of, in the four values a
// derived block needs for its provenance. EVERY field is FOREIGN TEXT lifted
// out of plugin-written metadata (the id excepted: it is a corpus uuid the
// reader itself selected), so a consumer must re-type it before writing it
// anywhere — the arm validates the three string fields against a character
// class and drops what fails (design/02 §4.4.3).
//
// A zero Manifest means "this source has no such unit", never "the unit was
// empty": absent is representable, and a caller must be able to tell the two
// apart before it stamps a provenance.
type Manifest struct {
	// ID is the manifest block's corpus uuid.
	ID string

	// SHA256 is the transcript digest the plugin recorded on the manifest —
	// metadata.sha256, present on every live manifest.
	SHA256 string

	// ParentID is the preceding manifest of the same root, "" for the first
	// one of a chain (live: 282 of 319 manifests carry the key at all).
	ParentID string

	// ActiveSessionID is the session that produced the compaction, which is
	// NOT the root: a root outlives its compactions and the plugin records
	// both.
	ActiveSessionID string
}

// Origin is the citation anchor. It must identify the exact text the gate
// verified against — for the checkpoint source that is (BlockID, ChunkIndex),
// NOT a message ordinal, because the overwhelming majority of the parts carry
// no ordinal at all.
type Origin struct {
	// BlockID is the ctx block this item came from; "" for non-ctx sources.
	BlockID string

	// RowID is the foreign row this item came from; 0 for ctx sources.
	RowID int64

	// ChunkIndex is 1-based within the addressed unit.
	ChunkIndex int

	// Ordinal is the last seen message ordinal, 0 when unknown. It is a
	// READABILITY field, never an identity: the identity is
	// (BlockID, ChunkIndex) or (RowID, ChunkIndex).
	Ordinal int

	// Role is "user", "assistant", "tool", or "" when unknown. The hermes
	// adapter reads only tool results, which is why the set is wider than the
	// two conversational roles of the checkpoint source.
	Role string
}

// Batch is one read of a session.
type Batch struct {
	// Items is the material, in watermark order.
	Items []Item

	// Watermark is the HIGHEST FULLY COVERED watermark of this batch — the
	// value a caller may advance its journal to once the derived material is
	// durable. It equals the caller's after when the batch read nothing, so a
	// caller may write it unconditionally.
	//
	// "Covered" is about the SOURCE ROWS, not about the items: a row that was
	// read and deliberately dropped (undecodable payload, empty text) is
	// covered and must not be read forever.
	Watermark int64

	// Complete is false when the batch must not be advanced past: the
	// watermark group was cut in the middle, or the read covered no row it
	// could name. A caller that ignores this loses material silently, which is
	// why the field is the batch's, not an error.
	Complete bool
}

// Source is the whole contract.
type Source interface {
	// Label names the source in logs, journals and the source_key. It is the
	// operator-facing name ("hermes", "ctx-checkpoint"), not a type.
	Label() string

	// Sessions lists the candidate units, newest activity first. It carries no
	// row count and no per-candidate maximum: an unbounded count per candidate
	// per tick is a cost a ledger reports after a run, not one to pay before
	// it. A Ref returned here therefore carries Watermark 0 unless the source
	// can name a head without paying for it.
	Sessions(ctx context.Context) ([]Ref, error)

	// HasNew is the existence probe: is there material above after? It stops
	// at the first hit and never computes a maximum.
	HasNew(ctx context.Context, sess string, after int64) (bool, error)

	// Head is the highest watermark of the session and exists for the
	// regression check alone — a stored watermark above the head means the
	// source was reset or replaced. It is the expensive shape HasNew exists to
	// avoid, so it runs once per run, not once per tick.
	Head(ctx context.Context, sess string) (int64, error)

	// Read returns at most maxItems items, each at most maxRunes runes, from
	// the range above after. Both caps are the caller's; a non-positive cap
	// yields an empty, incomplete batch rather than a source-chosen default.
	//
	// maxRunes is absolute. maxItems has ONE exception, and it is a property of
	// this contract rather than of any implementation: when a source's smallest
	// INDIVISIBLE unit exceeds the cap, that unit is delivered whole and the
	// cap is overshot. A source whose atom is a single row never reaches this
	// case — the hermes adapter reads one archived row at a time and honours
	// maxItems exactly. A source whose atom is a group of rows does: the ctx
	// checkpoint source reads a manifest with all of its parts, and the largest
	// manifest in the live corpus yields 558 items against a rows_per_read of
	// 400 (measured, A02-3).
	//
	// The alternative is worse than the overshoot, and that is why the contract
	// bends rather than the implementation: a batch that can never cover its
	// first atom never advances its watermark, so the caller re-reads the same
	// range every tick, pays for it every time, and progresses never. A cap
	// that produces a permanent stall is not a budget, it is a deadlock.
	//
	// Consequences a caller must plan for: a batch may exceed maxItems, so
	// buffers and per-call budgets are dimensioned from the ATOM SIZE, not from
	// the cap; and the overshoot is bounded by one atom, never by the corpus.
	// A caller that needs a hard ceiling has to bound the material itself,
	// because no source can honour one without losing the material outright.
	Read(ctx context.Context, sess string, after int64, maxItems, maxRunes int) (Batch, error)

	// QuietFor reports how long the session's newest LIVE material has been
	// standing, clamped to zero for a timestamp in the future (a clock
	// regression must not read as a negative idle time). A session with
	// nothing live yields ErrNoActiveRows.
	QuietFor(ctx context.Context, sess string, now time.Time) (time.Duration, error)

	// Close releases the source's handle. A Source owns what it was built
	// over.
	Close() error
}

package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DriftCensus is the corpus census the arm-weight sweep brackets a measurement
// run with (design 04 §4.7 (2), wave B-W5).
//
// It exists as an ADDITIVE section of the existing admin-gated stats action and
// not as a new endpoint: the four numbers per type are a stats question, the
// route, the auth chain and the scope predicate are already there, and a second
// endpoint would be a second place to get the scope filter wrong.
//
// It is not free. Four aggregates over context_blocks grouped by type is a
// full scan of the visible corpus, which at the 1M+ target is seconds, not
// milliseconds — which is why it is admin-gated and computed only when the
// caller asks for it, never as part of the ordinary stats response.
type DriftCensus struct {
	// At is the SERVER's transaction clock. The sweep's mutation window is
	// [before.At, after.At], so it has to come from the same clock updated_at
	// does; a driver-side timestamp would be off by the skew.
	At time.Time `json:"at"`
	// RetrievableBlocks counts the non-archived blocks in a retrieval-visible
	// type — the population a query can actually reach.
	RetrievableBlocks int              `json:"retrievable_blocks"`
	Types             []TypeCensus     `json:"types"`
	GoldIDs           []BlockLifecycle `json:"gold_ids"`
}

// TypeCensus is one block type's four numbers.
type TypeCensus struct {
	TypeName string `json:"type_name"`
	// Retrievable is membership in the retrieval allowlist. The sweep's
	// null-embedding rule is evaluated over retrievable types only: the store
	// holds thousands of null embeddings in excluded types as standing policy.
	Retrievable   bool       `json:"retrievable"`
	Count         int        `json:"count"`
	MaxCreatedAt  *time.Time `json:"max_created_at"`
	MaxUpdatedAt  *time.Time `json:"max_updated_at"`
	NullEmbedding int        `json:"null_embedding"`
}

// BlockLifecycle is one addressed block's create/update stamps, or absence.
// A caller asking after a set of ids gets back only the ones that EXIST in its
// read scopes; the missing ones are the finding.
type BlockLifecycle struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// maxDriftGoldIDs caps the id list a single census may be asked about, so a
// malformed request cannot turn the census into an unbounded id scan.
//
// The at-most-one-label-per-query gold set the old cap of 2000 was cut for
// ends with wave M-W5: the multi-gold slices G-SESS/G-MH/G-GLOB put the
// planned campaign at roughly 2130 ids (design 05 §4.5, §6.5), which the old
// cap refused outright. The cap is now the order of magnitude above THAT.
//
// It stays a HARD error, never a silent truncation: an id quietly dropped
// from the request would come back as an absent block, and absence is exactly
// the signal the drift protocol reads as "the gold block vanished".
const maxDriftGoldIDs = 10000

// driftGoldIDChunk is how many ids one lifecycle statement carries. The cap
// bounds the REQUEST, this bounds the single statement's parameter array — a
// full-cap census is ten bounded index probes instead of one 10000-element
// array match, which is what keeps the census usable at the 1M+ target.
//
// Chunk results are merged, then sorted by id and deduplicated. That
// reproduces the single statement this replaces exactly: `ORDER BY id` for the
// order, `= ANY(...)` set semantics for a repeated id. Sorting the TEXT form
// is the same order Postgres' uuid comparison gives — a uuid renders as
// fixed-width lowercase hex with dashes at fixed offsets, so a lexicographic
// comparison of the text is a byte comparison of the value.
const driftGoldIDChunk = 1000

// GetDriftCensus takes one census over readScopes. visibleTypes is the
// retrieval allowlist from the block-type registry snapshot — passed in rather
// than derived here, because the registry is the one authority on that list and
// store must not grow a second opinion about it.
func GetDriftCensus(ctx context.Context, pool *pgxpool.Pool, readScopes, visibleTypes, goldIDs []string) (*DriftCensus, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if len(goldIDs) > maxDriftGoldIDs {
		return nil, fmt.Errorf("store: drift census: %d ids requested, cap is %d", len(goldIDs), maxDriftGoldIDs)
	}

	c := &DriftCensus{Types: []TypeCensus{}, GoldIDs: []BlockLifecycle{}}
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&c.At); err != nil {
		return nil, fmt.Errorf("store: drift census clock: %w", err)
	}

	visible := map[string]bool{}
	for _, t := range visibleTypes {
		visible[t] = true
	}

	rows, err := pool.Query(ctx,
		`SELECT type_name,
		        count(*)::int,
		        max(created_at),
		        max(updated_at),
		        (count(*) FILTER (WHERE embedding IS NULL))::int
		   FROM context_blocks
		  WHERE scope = ANY($1::text[]) AND NOT is_archived
		  GROUP BY type_name
		  ORDER BY type_name`,
		readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: drift census types: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t TypeCensus
		if err := rows.Scan(&t.TypeName, &t.Count, &t.MaxCreatedAt, &t.MaxUpdatedAt, &t.NullEmbedding); err != nil {
			return nil, fmt.Errorf("store: drift census scan: %w", err)
		}
		t.Retrievable = visible[t.TypeName]
		if t.Retrievable {
			c.RetrievableBlocks += t.Count
		}
		c.Types = append(c.Types, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: drift census rows: %w", err)
	}

	if len(goldIDs) == 0 {
		return c, nil
	}
	for start := 0; start < len(goldIDs); start += driftGoldIDChunk {
		end := min(start+driftGoldIDChunk, len(goldIDs))
		if err := appendGoldLifecycles(ctx, pool, c, goldIDs[start:end], readScopes); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(c.GoldIDs, func(a, b BlockLifecycle) int { return strings.Compare(a.ID, b.ID) })
	c.GoldIDs = slices.CompactFunc(c.GoldIDs, func(a, b BlockLifecycle) bool { return a.ID == b.ID })
	return c, nil
}

// appendGoldLifecycles runs the lifecycle statement over ONE chunk of ids and
// appends what it found. The caller orders and deduplicates the merged result;
// the per-chunk `ORDER BY id` is kept so a single-chunk census hands the rows
// over in the same order it always did, before the merge sort even runs.
func appendGoldLifecycles(ctx context.Context, pool *pgxpool.Pool, c *DriftCensus, ids, readScopes []string) error {
	gr, err := pool.Query(ctx,
		`SELECT id::text, created_at, updated_at
		   FROM context_blocks
		  WHERE id = ANY($1::uuid[]) AND scope = ANY($2::text[]) AND NOT is_archived
		  ORDER BY id`,
		ids, readScopes)
	if err != nil {
		return fmt.Errorf("store: drift census gold ids: %w", err)
	}
	defer gr.Close()
	for gr.Next() {
		var b BlockLifecycle
		if err := gr.Scan(&b.ID, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return fmt.Errorf("store: drift census gold scan: %w", err)
		}
		c.GoldIDs = append(c.GoldIDs, b)
	}
	if err := gr.Err(); err != nil {
		return fmt.Errorf("store: drift census gold rows: %w", err)
	}
	return nil
}

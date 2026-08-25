package store

import (
	"context"
	"fmt"
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

// maxDriftGoldIDs caps the id list a single census may be asked about. The gold
// set is 650 queries with at most one label each today; the cap is the order of
// magnitude above that, so a malformed request cannot turn the census into an
// unbounded id scan.
const maxDriftGoldIDs = 2000

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
	gr, err := pool.Query(ctx,
		`SELECT id::text, created_at, updated_at
		   FROM context_blocks
		  WHERE id = ANY($1::uuid[]) AND scope = ANY($2::text[]) AND NOT is_archived
		  ORDER BY id`,
		goldIDs, readScopes)
	if err != nil {
		return nil, fmt.Errorf("store: drift census gold ids: %w", err)
	}
	defer gr.Close()
	for gr.Next() {
		var b BlockLifecycle
		if err := gr.Scan(&b.ID, &b.CreatedAt, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: drift census gold scan: %w", err)
		}
		c.GoldIDs = append(c.GoldIDs, b)
	}
	if err := gr.Err(); err != nil {
		return nil, fmt.Errorf("store: drift census gold rows: %w", err)
	}
	return c, nil
}

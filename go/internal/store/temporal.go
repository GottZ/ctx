// Package store — Temporal EAV Dimension Functions
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Manages the context_temporal table (block_id, dimension, value, source_date)
// for O(log n) temporal queries via partial B-Tree indexes.
//
// Source: https://github.com/GottZ/ctx
package store

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TemporalDimension represents a single dimension extracted from a date.
type TemporalDimension struct {
	Dimension  string    // "year", "month", "week", "weekday", "quarter"
	Value      string    // e.g. "2026", "3", "13", "1", "1"
	SourceDate time.Time // the original date this dimension was extracted from
}

// ExpandDimensions returns the 5 temporal dimensions for a given date.
// Pure function, no DB access.
func ExpandDimensions(d time.Time) []TemporalDimension {
	_, isoWeek := d.ISOWeek()

	// Go Weekday: 0=Sunday .. 6=Saturday
	// ISO weekday: 1=Monday .. 7=Sunday
	goWd := d.Weekday()
	isoWd := int(goWd)
	if isoWd == 0 {
		isoWd = 7 // Sunday: Go=0 → ISO=7
	}

	quarter := (int(d.Month()) - 1) / 3 + 1

	return []TemporalDimension{
		{Dimension: "year", Value: strconv.Itoa(d.Year()), SourceDate: d},
		{Dimension: "month", Value: strconv.Itoa(int(d.Month())), SourceDate: d},
		{Dimension: "week", Value: strconv.Itoa(isoWeek), SourceDate: d},
		{Dimension: "weekday", Value: strconv.Itoa(isoWd), SourceDate: d},
		{Dimension: "quarter", Value: strconv.Itoa(quarter), SourceDate: d},
	}
}

// BuildTemporalBatch creates the pgx.Batch for PopulateTemporal.
// Returns the batch and the expected query count.
// Exported for testing — no DB access.
func BuildTemporalBatch(blockID string, dates []time.Time, links ...[]string) (*pgx.Batch, int) {
	batch := &pgx.Batch{}

	// Delete existing dimensions for this block
	batch.Queue(`DELETE FROM context_temporal WHERE block_id = $1`, blockID)

	// Collect link targets (variadic for backward compatibility)
	var linkTargets []string
	if len(links) > 0 {
		linkTargets = links[0]
	}

	// Sentinel for blocks without dates AND without links
	if len(dates) == 0 && len(linkTargets) == 0 {
		batch.Queue(
			`INSERT INTO context_temporal (block_id, dimension, value, source_date)
			 VALUES ($1, '_none', '', '1970-01-01'::date)
			 ON CONFLICT DO NOTHING`,
			blockID,
		)
		return batch, 2
	}

	queryCount := 1 // DELETE

	// Insert temporal dimensions for each date
	for _, d := range dates {
		dims := ExpandDimensions(d)
		sourceDate := d.Format("2006-01-02")
		for _, dim := range dims {
			batch.Queue(
				`INSERT INTO context_temporal (block_id, dimension, value, source_date)
				 VALUES ($1, $2, $3, $4::date)
				 ON CONFLICT DO NOTHING`,
				blockID, dim.Dimension, dim.Value, sourceDate,
			)
			queryCount++
		}
	}

	// Insert link dimensions (graph associations)
	for _, target := range linkTargets {
		if target == "" {
			continue
		}
		batch.Queue(
			`INSERT INTO context_temporal (block_id, dimension, value, source_date)
			 VALUES ($1, 'link', $2, '1970-01-01'::date)
			 ON CONFLICT DO NOTHING`,
			blockID, target,
		)
		queryCount++
	}

	// Sentinel if only links, no dates (block still needs temporal entry for backfill)
	if len(dates) == 0 && len(linkTargets) > 0 {
		// Links are already inserted, no sentinel needed — block exists in context_temporal
	}

	return batch, queryCount
}

// PopulateTemporal replaces all temporal dimensions for a block.
// Deletes existing rows, then batch-inserts new dimensions for each date.
func PopulateTemporal(ctx context.Context, pool *pgxpool.Pool, blockID string, dates []time.Time) error {
	batch, queryCount := BuildTemporalBatch(blockID, dates)

	br := pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	for i := 0; i < queryCount; i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("store: temporal: batch query %d: %w", i, err)
		}
	}

	return nil
}

// UpdateContentDates sets the content_dates array on a block.
func UpdateContentDates(ctx context.Context, pool *pgxpool.Pool, blockID string, dates []time.Time) error {
	isoStrings := make([]string, len(dates))
	for i, d := range dates {
		isoStrings[i] = d.Format("2006-01-02")
	}

	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET content_dates = $1::date[] WHERE id = $2`,
		isoStrings, blockID,
	)
	if err != nil {
		return fmt.Errorf("store: temporal: update content_dates: %w", err)
	}
	return nil
}

// FetchContentDates retrieves content_dates for multiple blocks.
// Returns a map from block ID to parsed dates.
func FetchContentDates(ctx context.Context, pool *pgxpool.Pool, blockIDs []string) (map[string][]time.Time, error) {
	if len(blockIDs) == 0 {
		return make(map[string][]time.Time), nil
	}

	rows, err := pool.Query(ctx,
		`SELECT id, content_dates FROM context_blocks WHERE id = ANY($1)`,
		blockIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("store: temporal: fetch content_dates: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]time.Time, len(blockIDs))
	for rows.Next() {
		var id string
		var dates []time.Time
		if err := rows.Scan(&id, &dates); err != nil {
			return nil, fmt.Errorf("store: temporal: fetch content_dates scan: %w", err)
		}
		if dates != nil {
			result[id] = dates
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: temporal: fetch content_dates rows: %w", err)
	}
	return result, nil
}

// BackfillTemporal populates context_temporal for all blocks that don't have
// temporal dimensions yet. Processes in batches of 50.
// Returns the number of blocks processed.
func BackfillTemporal(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	const batchSize = 50
	processed := 0

	for {
		rows, err := pool.Query(ctx,
			`SELECT cb.id, cb.content
			 FROM context_blocks cb
			 WHERE NOT cb.is_archived
			   AND NOT EXISTS (SELECT 1 FROM context_temporal ct WHERE ct.block_id = cb.id)
			 LIMIT $1`,
			batchSize,
		)
		if err != nil {
			return processed, fmt.Errorf("store: temporal: backfill query: %w", err)
		}

		type blockRow struct {
			ID      string
			Content string
		}
		var blocks []blockRow
		for rows.Next() {
			var b blockRow
			if err := rows.Scan(&b.ID, &b.Content); err != nil {
				rows.Close()
				return processed, fmt.Errorf("store: temporal: backfill scan: %w", err)
			}
			blocks = append(blocks, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return processed, fmt.Errorf("store: temporal: backfill rows: %w", err)
		}

		if len(blocks) == 0 {
			break // No more blocks to process
		}

		for _, b := range blocks {
			dates := ExtractDates(b.Content)

			if err := UpdateContentDates(ctx, pool, b.ID, dates); err != nil {
				slog.Warn("backfill: update content_dates failed",
					"block_id", b.ID, "error", err)
				continue
			}

			// Always populate temporal — for blocks without dates this inserts
			// a sentinel (_none) so NOT EXISTS doesn't re-select them.
			if err := PopulateTemporal(ctx, pool, b.ID, dates); err != nil {
				slog.Warn("backfill: populate temporal failed",
					"block_id", b.ID, "error", err)
				continue
			}

			processed++
		}

		slog.Info("backfill: batch complete",
			"batch_size", len(blocks), "total_processed", processed)

		if len(blocks) < batchSize {
			break // Last batch was partial, we're done
		}
	}

	return processed, nil
}

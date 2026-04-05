// Package store — Temporal EAV Dimension Functions
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Manages the context_temporal table (block_id, dimension, value, source_time)
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

// TemporalDimension represents a single dimension extracted from a timestamp.
type TemporalDimension struct {
	Dimension  string    // "year", "month", "week", "weekday", "quarter", "monthday", "seasonal", "daily"
	Value      string    // e.g. "2026", "3", "13", "1", "1", "29", "88", "14"
	SourceTime time.Time // the original timestamp this dimension was extracted from
}

// ExpandDimensions returns the 8 temporal dimensions for a given timestamp.
// Pure function, no DB access.
//
// Dimensions (all cyclic except year):
//   - year:     linear, monotonic (NOT cyclic)
//   - month:    month-of-year (1-12, 12-cycle)
//   - week:     ISO week-of-year (1-53, 52-cycle)
//   - weekday:  day-of-week (ISO 1-7, 7-cycle)
//   - quarter:  quarter-of-year (1-4, 4-cycle)
//   - monthday: day-of-month (1-31, ~30-cycle)
//   - seasonal: day-of-year (1-366, 365-cycle)
//   - daily:    hour-of-day (0-23, 24-cycle)
func ExpandDimensions(t time.Time) []TemporalDimension {
	_, isoWeek := t.ISOWeek()

	// Go Weekday: 0=Sunday .. 6=Saturday
	// ISO weekday: 1=Monday .. 7=Sunday
	goWd := t.Weekday()
	isoWd := int(goWd)
	if isoWd == 0 {
		isoWd = 7 // Sunday: Go=0 → ISO=7
	}

	quarter := (int(t.Month()) - 1) / 3 + 1

	return []TemporalDimension{
		{Dimension: "year", Value: strconv.Itoa(t.Year()), SourceTime: t},
		{Dimension: "month", Value: strconv.Itoa(int(t.Month())), SourceTime: t},
		{Dimension: "week", Value: strconv.Itoa(isoWeek), SourceTime: t},
		{Dimension: "weekday", Value: strconv.Itoa(isoWd), SourceTime: t},
		{Dimension: "quarter", Value: strconv.Itoa(quarter), SourceTime: t},
		{Dimension: "monthday", Value: strconv.Itoa(t.Day()), SourceTime: t},
		{Dimension: "seasonal", Value: strconv.Itoa(t.YearDay()), SourceTime: t},
		{Dimension: "daily", Value: strconv.Itoa(t.Hour()), SourceTime: t},
	}
}

// BuildTemporalBatch creates the pgx.Batch for PopulateTemporal.
// Returns the batch and the expected query count.
// Exported for testing — no DB access.
func BuildTemporalBatch(blockID string, times []time.Time, links ...[]string) (*pgx.Batch, int) {
	batch := &pgx.Batch{}

	// Delete existing dimensions for this block
	batch.Queue(`DELETE FROM context_temporal WHERE block_id = $1`, blockID)

	// Collect link targets (variadic for backward compatibility)
	var linkTargets []string
	if len(links) > 0 {
		linkTargets = links[0]
	}

	// Sentinel for blocks without times AND without links
	sentinel := time.Unix(0, 0).UTC() // 1970-01-01 00:00:00 UTC
	if len(times) == 0 && len(linkTargets) == 0 {
		batch.Queue(
			`INSERT INTO context_temporal (block_id, dimension, value, source_time)
			 VALUES ($1, '_none', '', $2)
			 ON CONFLICT DO NOTHING`,
			blockID, sentinel,
		)
		return batch, 2
	}

	queryCount := 1 // DELETE

	// Insert temporal dimensions for each timestamp
	for _, t := range times {
		dims := ExpandDimensions(t)
		for _, dim := range dims {
			batch.Queue(
				`INSERT INTO context_temporal (block_id, dimension, value, source_time)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT DO NOTHING`,
				blockID, dim.Dimension, dim.Value, t,
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
			`INSERT INTO context_temporal (block_id, dimension, value, source_time)
			 VALUES ($1, 'link', $2, $3)
			 ON CONFLICT DO NOTHING`,
			blockID, target, sentinel,
		)
		queryCount++
	}

	// Links are already inserted above; no sentinel needed when only links, no dates.

	return batch, queryCount
}

// PopulateTemporal replaces all temporal dimensions for a block.
// Deletes existing rows, then batch-inserts new dimensions for each timestamp.
func PopulateTemporal(ctx context.Context, pool *pgxpool.Pool, blockID string, times []time.Time) error {
	batch, queryCount := BuildTemporalBatch(blockID, times)

	br := pool.SendBatch(ctx, batch)
	defer func() { _ = br.Close() }()

	for i := 0; i < queryCount; i++ {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("store: temporal: batch query %d: %w", i, err)
		}
	}

	return nil
}

// UpdateContentTimes sets the content_times array on a block.
func UpdateContentTimes(ctx context.Context, pool *pgxpool.Pool, blockID string, times []time.Time) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET content_times = $1::timestamptz[] WHERE id = $2`,
		times, blockID,
	)
	if err != nil {
		return fmt.Errorf("store: temporal: update content_times: %w", err)
	}
	return nil
}

// FetchContentTimes retrieves content_times for multiple blocks.
// Returns a map from block ID to parsed timestamps.
func FetchContentTimes(ctx context.Context, pool *pgxpool.Pool, blockIDs []string) (map[string][]time.Time, error) {
	if len(blockIDs) == 0 {
		return make(map[string][]time.Time), nil
	}

	rows, err := pool.Query(ctx,
		`SELECT id, content_times FROM context_blocks WHERE id = ANY($1)`,
		blockIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("store: temporal: fetch content_times: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]time.Time, len(blockIDs))
	for rows.Next() {
		var id string
		var times []time.Time
		if err := rows.Scan(&id, &times); err != nil {
			return nil, fmt.Errorf("store: temporal: fetch content_times scan: %w", err)
		}
		if times != nil {
			result[id] = times
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: temporal: fetch content_times rows: %w", err)
	}
	return result, nil
}

// BlockDimension is the (dimension, value) pair returned by FetchBlockDimensions.
// Keeps the type minimal (no source_time) to match rrf.TemporalDim semantics.
type BlockDimension struct {
	Dimension string
	Value     string
}

// FetchBlockDimensions retrieves EAV temporal dimensions for multiple blocks.
// Filters by the requested dimension names (e.g. ["weekday", "month"]).
// Sentinel "_none" rows are excluded automatically.
//
// Returns map[blockID][]BlockDimension for use in cyclic gravity computation.
// Blocks with no matching dimensions are simply absent from the map.
func FetchBlockDimensions(ctx context.Context, pool *pgxpool.Pool, blockIDs []string, dimensions []string) (map[string][]BlockDimension, error) {
	if len(blockIDs) == 0 || len(dimensions) == 0 {
		return make(map[string][]BlockDimension), nil
	}

	rows, err := pool.Query(ctx,
		`SELECT block_id, dimension, value
		 FROM context_temporal
		 WHERE block_id = ANY($1)
		   AND dimension = ANY($2)
		   AND dimension != '_none'`,
		blockIDs, dimensions,
	)
	if err != nil {
		return nil, fmt.Errorf("store: temporal: fetch block dimensions: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]BlockDimension, len(blockIDs))
	for rows.Next() {
		var blockID, dim, val string
		if err := rows.Scan(&blockID, &dim, &val); err != nil {
			return nil, fmt.Errorf("store: temporal: fetch block dimensions scan: %w", err)
		}
		result[blockID] = append(result[blockID], BlockDimension{Dimension: dim, Value: val})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: temporal: fetch block dimensions rows: %w", err)
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
			times := ExtractDates(b.Content)

			if err := UpdateContentTimes(ctx, pool, b.ID, times); err != nil {
				slog.Warn("backfill: update content_times failed",
					"block_id", b.ID, "error", err)
				continue
			}

			// Always populate temporal — for blocks without times this inserts
			// a sentinel (_none) so NOT EXISTS doesn't re-select them.
			if err := PopulateTemporal(ctx, pool, b.ID, times); err != nil {
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

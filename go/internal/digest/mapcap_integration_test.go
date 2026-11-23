//go:build integration

// Issue #518 integration gates: past PostgreSQL's limit on the full-text
// vectors of its row, the topic map is still written — capped, and saying so —
// and a map at the cap fits that limit for the titles that cost the most.
//
//	go test -tags=integration ./internal/digest/ -run 'TestRunDigestCapsTheMap|TestCappedMapOfCostlyTitles' -count=1 -v
package digest

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// costlyTitle is a hyphenated compound of 14 alphanumeric parts (69 runes):
// the parser emits the compound and each part, each a distinct lexeme — the
// title pattern that cost the most vector bytes per map byte of those measured
// for maxMapBytes.
func costlyTitle(r *rand.Rand) string {
	parts := make([]string, 14)
	for i := range parts {
		parts[i] = string([]byte{byte('a' + r.IntN(26)), byte('0' + r.IntN(10)), byte('a' + r.IntN(26)), byte('0' + r.IntN(10))})
	}
	return strings.Join(parts, "-")
}

// seedCostly writes n blocks with costly titles through store.UpsertBlock,
// spread over categories and scopes, four writers at a time.
func seedCostly(t *testing.T, pool *pgxpool.Pool, n int, categories, scopes []string) {
	t.Helper()
	ctx := context.Background()
	r := rand.New(rand.NewPCG(518, 518))
	type job struct{ category, title, scope string }
	jobs := make(chan job)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if _, err := store.UpsertBlock(ctx, pool, j.category, j.title, "costly fixture "+j.title,
					nil, nil, j.scope, true, store.SensitivityWrite{}, ""); err != nil {
					once.Do(func() { firstErr = err })
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		jobs <- job{categories[i%len(categories)], costlyTitle(r), scopes[i%len(scopes)]}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("seed: %v", firstErr)
	}
}

// TestRunDigestCapsTheMapPastTheVectorLimit is the request-independent RED of
// issue #518: 9,000 blocks whose uncapped map is past the 1 MB limit of a
// tsvector. Before the cap RunDigest failed with SQLSTATE 54000 ("string is
// too long for tsvector") and wrote nothing; now it writes a map under the cap
// that says it is truncated, in its text and its metadata.
func TestRunDigestCapsTheMapPastTheVectorLimit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	const n = 9000
	window := []string{"private", "work"}
	seedCostly(t, pool, n, []string{"decisions", "learnings", "reference"}, window)

	// Fixture self-check: without the cap the map would not fit its row — the
	// test would prove nothing on a corpus below the limit.
	var rows []mapRow
	q, err := pool.Query(ctx, `SELECT id, title, category, scope FROM context_blocks
	  WHERE scope = ANY($1) AND NOT is_archived ORDER BY category, title`, window)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	for q.Next() {
		var r mapRow
		if err := q.Scan(&r.id, &r.title, &r.category, &r.scope); err != nil {
			t.Fatalf("scan: %v", err)
		}
		rows = append(rows, r)
	}
	q.Close()
	uncapped, _ := renderFull("private", "2026-11-23", rows)
	var pgErr *pgconn.PgError
	if _, err := pool.Exec(ctx, `SELECT to_tsvector('german', 'topic-map-private ' || $1)`, uncapped); !errors.As(err, &pgErr) || pgErr.Code != "54000" {
		t.Fatalf("fixture: the uncapped map (%d bytes) fits a tsvector (err %v) — the corpus is below the limit", len(uncapped), err)
	}

	res, err := RunDigest(ctx, pool, reg, ModeFull, "", "private", "private", window)
	if err != nil {
		t.Fatalf("RunDigest past the vector limit: %v", err)
	}

	var (
		content, typeName string
		md                map[string]any
	)
	if err := pool.QueryRow(ctx, `SELECT content, type_name, metadata FROM context_blocks
	  WHERE category = 'index' AND title = 'topic-map-private' AND scope = 'private' AND NOT is_archived`).
		Scan(&content, &typeName, &md); err != nil {
		t.Fatalf("read map: %v", err)
	}
	if len(content) > maxMapBytes {
		t.Errorf("map is %d bytes, cap %d", len(content), maxMapBytes)
	}
	if !strings.Contains(content, fmt.Sprintf("\nTruncated: %d of %d blocks listed", res.Listed, n)) {
		t.Errorf("map does not say it is truncated:\n%.400s", content)
	}
	if !res.Truncated || res.Listed <= 0 || res.Listed >= n {
		t.Errorf("result = %+v, want truncated with 0 < listed < %d", res, n)
	}
	if md["truncated"] != true || md["listed_blocks"] != float64(res.Listed) || md["block_count"] != float64(n) {
		t.Errorf("metadata = truncated %v, listed_blocks %v, block_count %v; want true, %d, %d",
			md["truncated"], md["listed_blocks"], md["block_count"], res.Listed, n)
	}
	if typeName != "system-meta" {
		t.Errorf("type_name = %q, want system-meta", typeName)
	}
	perCat := map[string]int{}
	for _, r := range rows {
		perCat[r.category]++
	}
	for cat, k := range perCat {
		if !strings.Contains(content, fmt.Sprintf("\n%s (%d, ", cat, k)) {
			t.Errorf("category %s is not listed in part with its count %d:\n%.600s", cat, k, content)
		}
	}
}

// TestCappedMapOfCostlyTitlesIsStored: a map rendered at the cap from the
// title patterns that cost the most vector bytes is stored through
// store.UpsertBlock — the write that fails with 54000 when the cap is too
// high. RED against a cap above the limit (1 MiB fails each pattern).
func TestCappedMapOfCostlyTitlesIsStored(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	letters := func(r *rand.Rand, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + r.IntN(26))
		}
		return string(b)
	}
	patterns := map[string]func(r *rand.Rand) string{
		"compound": costlyTitle,
		"url": func(r *rand.Rand) string {
			return "https://" + letters(r, 4) + costlyTitle(r)[:9] + ".example.org/" + costlyTitle(r)[:14] + "/" + letters(r, 6) + "?q=" + letters(r, 5)
		},
		// Two-byte capitals whose lower case takes three bytes.
		"case-folding": func(r *rand.Rand) string {
			parts := make([]string, 14)
			for i := range parts {
				parts[i] = []string{"Ⱥ", "Ⱦ"}[r.IntN(2)] + letters(r, 3)
			}
			return strings.Join(parts, "-")
		},
	}
	for name, title := range patterns {
		r := rand.New(rand.NewPCG(518, uint64(len(name))))
		b := newMapBuilder("private", maxMapBytes)
		for i := 0; i < 20000; i++ {
			b.add(mapRow{id: fmt.Sprintf("%08x-0000-7000-8000-000000000000", r.Uint32()), title: title(r),
				category: fmt.Sprintf("cat-%d", i/5000), scope: "private"})
		}
		m := b.render("2026-11-23")
		if len(m.text) < maxMapBytes*95/100 || len(m.text) > maxMapBytes {
			t.Fatalf("%s: map is %d bytes, want just under the cap %d", name, len(m.text), maxMapBytes)
		}
		if _, err := store.UpsertBlock(ctx, pool, "index", "topic-map-"+name, m.text, nil,
			map[string]any{"is_meta": true}, "private", true, store.SensitivityWrite{}, ""); err != nil {
			t.Errorf("%s: a map at the cap (%d bytes) is not stored: %v", name, len(m.text), err)
		}
	}
}

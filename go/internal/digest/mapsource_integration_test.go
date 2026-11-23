//go:build integration

// Issue #525: the source query of the topic map sorts by bytes, whatever
// collation the database or its columns carry.
//
//	go test -tags=integration ./internal/digest/ -run 'TestMapSource' -count=1 -v
package digest

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestMapSourceGroupsCategoriesInByteOrder runs the production source query
// over a column whose collation orders the categories otherwise than their
// bytes do, and reads the order the categories arrive in. The test database
// of either image compares by bytes (C.UTF-8 on the CI image, and the template
// copies keep it), so the column carries the linguistic collation the
// production image's cluster has as its default (en_US.utf8), as a glibc or an
// ICU collation, whichever the image provides: the query decides its own
// order only if that order does not change.
//
// RED before issue #525: the source query sorted in the database's (here the
// column's) collation — "alpha" before "Zeta", where bytes put "Zeta" first —
// and on the production image every comparison of that sort went through
// strcoll, minutes for titles of emoji or CJK extension characters.
func TestMapSourceGroupsCategoriesInByteOrder(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var coll string
	err := pool.QueryRow(ctx, `SELECT collname FROM pg_collation
		WHERE collname IN ('en_US.utf8', 'en_US', 'en-US-x-icu') AND collencoding IN (-1, pg_char_to_encoding('UTF8'))
		ORDER BY array_position(ARRAY['en_US.utf8', 'en_US', 'en-US-x-icu']::name[], collname) LIMIT 1`).Scan(&coll)
	if err != nil {
		t.Fatalf("no linguistic collation for English on this image: %v", err)
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE context_blocks ALTER COLUMN category TYPE varchar(100) COLLATE "`+coll+`"`); err != nil {
		t.Fatalf("give category the collation %s: %v", coll, err)
	}

	// Categories whose order in English differs from their order in bytes:
	// upper case before lower case, umlauts past z, digits as text.
	cats := []string{"beta", "Zeta", "10", "Ärger", "alpha", "9", "zz"}
	for _, c := range cats {
		for _, s := range []string{"private", "work"} {
			if _, err := store.UpsertBlock(ctx, pool, c, "a block of "+c, "body", nil, nil, s, true,
				store.SensitivityWrite{}, ""); err != nil {
				t.Fatalf("seed %s: %v", c, err)
			}
		}
	}

	var linguistic []string
	rows, err := pool.Query(ctx, `SELECT DISTINCT category FROM context_blocks WHERE scope = 'private' ORDER BY category`)
	if err != nil {
		t.Fatalf("read the column's order: %v", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan: %v", err)
		}
		linguistic = append(linguistic, c)
	}
	rows.Close()
	bytesOrder := []string{"10", "9", "Zeta", "alpha", "beta", "zz", "Ärger"}
	if equalStrings(linguistic, bytesOrder) {
		t.Fatalf("the collation %s orders the categories by bytes (%v); the fixture shows nothing", coll, linguistic)
	}

	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reg.Boot(bctx, pool)
	types := reg.SnapshotForTenant(ctx, store.GlobalScope).DigestTypes()

	src, err := pool.Query(ctx, mapSourceQuery, []string{"private", "work"}, types)
	if err != nil {
		t.Fatalf("source query: %v", err)
	}
	var arrived []string
	for src.Next() {
		var id, title, category, scope string
		if err := src.Scan(&id, &title, &category, &scope); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if len(arrived) == 0 || arrived[len(arrived)-1] != category {
			arrived = append(arrived, category)
		}
	}
	if err := src.Err(); err != nil {
		t.Fatalf("source rows: %v", err)
	}
	src.Close()
	if !equalStrings(arrived, bytesOrder) {
		t.Fatalf("the source query returned the categories in the order %v (the column's collation %s orders %v), want byte order %v",
			arrived, coll, linguistic, bytesOrder)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

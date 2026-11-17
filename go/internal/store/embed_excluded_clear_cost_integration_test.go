//go:build integration

// Issue #213, second review, F1: the cost of a tenant's queue entry in both
// directions of the reconcile, marking and clearing, against the marked
// blocks of the same name in another tenant.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcludedTenantEntryCostBothDirections' -count=1 -v
package store_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// embedExcludedEntryBound is the bound of the cost pin below on the buffers
// of all statements of a tenant's entry: a few pages per statement on the
// ranges of the entry's own scope. A clear that reaches the scope's marked
// blocks through another index (the scope's whole range, or the name's
// marked blocks in every tenant on the definition of migration 175) reads
// thousands.
const embedExcludedEntryBound = 200

var indexNames = regexp.MustCompile(`"Index Name": "[^"]+"`)

// TestEmbedExcludedTenantEntryCostBothDirections: tenant B uses the name fc-x
// as its own excluded type and holds 10 000, then 100 000 marked blocks of
// it. Tenant A holds 20 000 blocks of a visible type with a vector and three
// blocks of fc-x, and writes its own row of the name: `excluded` (A's scopes
// answer true, the reconcile marks A's blocks), then `full-pass` (they answer
// false, the reconcile clears them). The buffers of every statement of A's
// entry, read again on their custom plans, stay flat in both directions and
// within embedExcludedEntryBound: A's entry reads the ranges of A's marked
// and pending blocks of the name and none of B's blocks. B's rows are not
// rewritten.
func TestEmbedExcludedTenantEntryCostBothDirections(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	traced, tracer := r3Traced(t, dsn)
	ctx := context.Background()
	reconcile(t, pool)
	ownerTenant(t, pool, "fc-a", "fc-a")
	ownerTenant(t, pool, "fc-b", "fc-b")
	createType(t, pool, "fc-x", "fc-b", "excluded")
	ownerSeed(t, pool, "fc-a", "fc-x", 3)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at, embedding, embed_model)
		 SELECT 'fc', 'fc a ' || i, 'x', 'knowledge', 'fc-a', 'public', now(), array_fill(0.1::real, ARRAY[1024])::vector, 'test'
		   FROM generate_series(1, 20000) i`); err != nil {
		t.Fatal(err)
	}
	reconcile(t, pool)
	entryCost := func(label string) int64 {
		t.Helper()
		var grp string
		if err := pool.QueryRow(ctx, `SELECT string_agg(grp, ',') FROM context_embed_excluded_queue WHERE name = 'fc-x'`).Scan(&grp); err != nil {
			t.Fatalf("%s: A's entry: %v", label, err)
		}
		if want := "t:"; !strings.HasPrefix(grp, want) || strings.Contains(grp, ",") {
			t.Fatalf("%s: queued groups %q, want A's group alone", label, grp)
		}
		// The statements are read again after the pass, with the arguments
		// they ran with: the rows they changed have left their range, so the
		// replay reads the range each statement scans and none of A's three
		// rows it wrote. That range is what this pin is about: the writes of
		// A's three rows cost the same in every state, and what grows is the
		// other tenant's marked blocks a statement walks on its way. On
		// 7adaa9b0 the replayed clear read 2 775 -> 15 341 buffers, and the
		// mutants that take the scope out of the clear's index access or
		// leave the index of 175 in place are red here.
		tracer.take()
		if _, err := store.ReconcileEmbedExcluded(ctx, traced); err != nil {
			t.Fatalf("%s: reconcile: %v", label, err)
		}
		var sum int64
		for _, st := range tracer.take() {
			if !strings.Contains(st.sql, "context_blocks") {
				continue
			}
			c := testdb.ExplainCustom(t, pool, st.sql, st.args...)
			sum += c.Buffers
			head := strings.Join(strings.Fields(st.sql), " ")
			if len(head) > 60 {
				head = head[:60]
			}
			t.Logf("   %-60s %5d buffers on %v", head, c.Buffers, indexNames.FindAllString(c.Plan, -1))
		}
		t.Logf("%s: %d buffers over the entry's statements", label, sum)
		return sum
	}
	var row *store.BlockTypeRow
	var marks, clears []int64
	from := 0
	for _, n := range []int{10_000, 100_000} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 SELECT 'fc', 'fc ' || i, 'x', 'fc-x', 'fc-b', 'public', now() FROM generate_series($1::int + 1, $2::int) i`, from, n); err != nil {
			t.Fatal(err)
		}
		from = n
		if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
			t.Fatal(err)
		}
		reconcile(t, pool)
		if got := markedCount(t, pool, "fc-b", "fc-x"); got != n {
			t.Fatalf("fixture: %d/%d of B's fc-x marked", got, n)
		}
		stB := scopeState(t, pool, "fc-b")

		if row == nil {
			row = createType(t, pool, "fc-x", "fc-a", "excluded")
		} else {
			updateType(t, pool, row.ID, "fc-a", "excluded")
		}
		marks = append(marks, entryCost(fmt.Sprintf("mark: A's fc-x excluded, B holds %d marked", n)))
		if got := markedCount(t, pool, "fc-a", "fc-x"); got != 3 {
			t.Errorf("%d/3 of A's fc-x marked after A's excluded row", got)
		}

		updateType(t, pool, row.ID, "fc-a", "full-pass")
		clears = append(clears, entryCost(fmt.Sprintf("clear: A's fc-x full-pass, B holds %d marked", n)))
		if got := markedCount(t, pool, "fc-a", "fc-x"); got != 0 {
			t.Errorf("%d/3 of A's fc-x still marked after A's full-pass row", got)
		}
		if scopeState(t, pool, "fc-b") != stB {
			t.Errorf("A's entries rewrote B's rows")
		}
	}
	for _, d := range []struct {
		dir  string
		sums []int64
	}{{"mark", marks}, {"clear", clears}} {
		if d.sums[1] > d.sums[0]+d.sums[0]/2+50 {
			t.Errorf("%s: A's entry reads %d buffers with 10 000 marked blocks of B and %d with 100 000: it walks B's rows",
				d.dir, d.sums[0], d.sums[1])
		}
		for _, sum := range d.sums {
			if sum > embedExcludedEntryBound {
				t.Errorf("%s: A's entry reads %d buffers, above %d: it does not read its scope's range of the name",
					d.dir, sum, embedExcludedEntryBound)
			}
		}
	}
}

//go:build integration

// The provenance gate (S3 / I7, design D-01 §4.3.1) for an update by id that
// changes the block's identity — its category, its title or its scope.
//
// The gate's contract: the (category, title, scope) of a derivative is not
// client-writable, and that includes the identity of an ARCHIVED derivative.
// The unique index behind the identity is partial on live rows, so an archived
// derivative leaves its identity free in that index; UpsertBlock guards a
// fresh create of it by probing for an archived claim (ProvenanceClaimed,
// foreignProvenanceClaim). store.UpdateBlock did not: a rename or a scope move
// of an ordinary block onto the identity of an archived derivative went
// through, and as long as that block stood there every writer met a live row
// without provenance — a client upsert wrote into it, another arm's badged
// upsert took it over (and the owner's revival was then refused), and the
// owner's revival replaced the moved block's content.
//
// The ownership half needs no new rule here: UpdateBlock has no badged caller,
// so every identity change through it is a client write and is refused, and the
// owner arm keeps its way back through the fresh upsert.
//
//	go test -tags=integration ./internal/store/ -run 'TestUpdateIdentityArchivedDerivativeClaim' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// u386Badge is the derived layer's server-path sensitivity intent.
var u386Badge = store.SensitivityWrite{Value: backends.SensInternal, Derived: true}

// u386Derivative writes a derivative of arm through the badged upsert and, with
// archive, archives it the way the revival tests do (no Go path archives a
// derivative; §4.7.5 will).
func u386Derivative(t *testing.T, pool *pgxpool.Pool, category, title, scope, arm string, archive bool) *store.Block {
	t.Helper()
	ctx := context.Background()
	d, err := store.UpsertBlock(ctx, pool, category, title, "derivative of "+arm, nil,
		dsProvenanceOf(arm, derived.StratumDerived), scope, true, u386Badge, derived.TypeCatalog)
	if err != nil {
		t.Fatalf("derivative %s/%s@%s: %v", category, title, scope, err)
	}
	if archive {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, d.ID); err != nil {
			t.Fatalf("archive derivative: %v", err)
		}
	}
	return d
}

// u386Client writes an ordinary block through the unbadged upsert.
func u386Client(t *testing.T, pool *pgxpool.Pool, category, title, scope string) *store.Block {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, category, title, "client text of "+title, nil, nil,
		scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("client block %s/%s@%s: %v", category, title, scope, err)
	}
	return b
}

// u386Row is a block's identity and content as the table holds it.
type u386Row struct {
	category, title, scope, content string
	archived                        bool
}

func u386Read(t *testing.T, pool *pgxpool.Pool, id string) u386Row {
	t.Helper()
	var r u386Row
	if err := pool.QueryRow(context.Background(),
		`SELECT category, title, scope, content, is_archived FROM context_blocks WHERE id = $1::uuid`, id,
	).Scan(&r.category, &r.title, &r.scope, &r.content, &r.archived); err != nil {
		t.Fatalf("read block %s: %v", id, err)
	}
	return r
}

func TestUpdateIdentityArchivedDerivativeClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const category, scope, other = "learnings", "u386", "u386-other"
	scopes := []string{scope, other}
	strptr := func(s string) *string { return &s }

	// refused asserts the refusal and that the block kept its identity.
	refused := func(t *testing.T, err error, id string, want u386Row) {
		t.Helper()
		if !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("update = %v, want ErrProvenanceProtected", err)
		}
		if got := u386Read(t, pool, id); got != want {
			t.Errorf("refused update changed the block: %+v, want %+v", got, want)
		}
	}

	t.Run("title rename onto an archived derivative's identity is refused", func(t *testing.T) {
		u386Derivative(t, pool, category, "u386 title", scope, "arm-a", true)
		c := u386Client(t, pool, category, "u386 title (client)", scope)
		before := u386Read(t, pool, c.ID)
		_, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr("u386 title")}, scopes)
		refused(t, err, c.ID, before)
	})

	t.Run("category change onto an archived derivative's identity is refused", func(t *testing.T) {
		u386Derivative(t, pool, "u386-cat", "u386 category", scope, "arm-a", true)
		c := u386Client(t, pool, category, "u386 category", scope)
		before := u386Read(t, pool, c.ID)
		_, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Category: strptr("u386-cat")}, scopes)
		refused(t, err, c.ID, before)
	})

	t.Run("scope move onto an archived derivative's identity is refused", func(t *testing.T) {
		u386Derivative(t, pool, category, "u386 scope", other, "arm-a", true)
		c := u386Client(t, pool, category, "u386 scope", scope)
		before := u386Read(t, pool, c.ID)
		_, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Scope: strptr(other)}, scopes)
		refused(t, err, c.ID, before)
	})

	t.Run("a content change riding on the rename is refused with it", func(t *testing.T) {
		u386Derivative(t, pool, category, "u386 ride", scope, "arm-a", true)
		c := u386Client(t, pool, category, "u386 ride (client)", scope)
		before := u386Read(t, pool, c.ID)
		_, _, err := store.UpdateBlock(ctx, pool, c.ID,
			store.UpdateBlockData{Title: strptr("u386 ride"), Content: strptr("new text")}, scopes)
		refused(t, err, c.ID, before)
	})

	// After the refused move the identity is still the archived derivative's:
	// each later writer meets the archived claim, not a live ordinary block.
	t.Run("after the refused move a client upsert of the identity is refused", func(t *testing.T) {
		const title = "u386 then client"
		u386Derivative(t, pool, category, title, scope, "arm-a", true)
		c := u386Client(t, pool, category, title+" (client)", scope)
		_, _, errU := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr(title)}, scopes)
		_, err := store.UpsertBlock(ctx, pool, category, title, "client upsert", nil, nil, scope, true, store.SensitivityWrite{}, "")
		if !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("client upsert after the move (move err=%v) = %v, want ErrProvenanceProtected", errU, err)
		}
	})

	t.Run("after the refused move another arm's badged upsert is refused", func(t *testing.T) {
		const title = "u386 then arm-b"
		u386Derivative(t, pool, category, title, scope, "arm-a", true)
		c := u386Client(t, pool, category, title+" (client)", scope)
		_, _, errU := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr(title)}, scopes)
		_, err := store.UpsertBlock(ctx, pool, category, title, "arm-b text", nil,
			dsProvenanceOf("arm-b", derived.StratumDerived), scope, true, u386Badge, derived.TypeInsight)
		if !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("arm-b upsert after the move (move err=%v) = %v, want ErrProvenanceProtected", errU, err)
		}
		if got := u386Read(t, pool, c.ID); got.content != "client text of "+title+" (client)" {
			t.Errorf("the client's block was rewritten: %+v", got)
		}
	})

	t.Run("after the refused move the owner revives its derivative as a new block", func(t *testing.T) {
		// The allowed write: the arm that owns the archived derivative.
		const title = "u386 then owner"
		d := u386Derivative(t, pool, category, title, scope, "arm-a", true)
		c := u386Client(t, pool, category, title+" (client)", scope)
		_, _, errU := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr(title)}, scopes)
		r, err := store.UpsertBlock(ctx, pool, category, title, "revived by arm-a", nil,
			dsProvenanceOf("arm-a", derived.StratumDerived), scope, true, u386Badge, derived.TypeCatalog)
		if err != nil {
			t.Fatalf("owner revival (move err=%v) = %v, want it admitted", errU, err)
		}
		if r.ID == c.ID || r.ID == d.ID {
			t.Errorf("the revival landed on block %s (client %s, archived derivative %s), want a new block", r.ID, c.ID, d.ID)
		}
		if got := u386Read(t, pool, c.ID); got.content != "client text of "+title+" (client)" || got.title != title+" (client)" {
			t.Errorf("the client's block after the revival: %+v, want it untouched", got)
		}
	})

	// The issue verb renames too: its lookup binds the type, not the category,
	// so the title of an issue is an identity change like UpdateBlock's.
	t.Run("an issue title change onto an archived derivative's identity is refused", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		iss, err := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: scope, Title: "u386 issue", Content: "issue body"}, scopes)
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		prefix := strings.TrimSuffix(iss.Title, "u386 issue")
		u386Derivative(t, pool, iss.Category, prefix+"u386 issue target", scope, "arm-a", true)
		before := u386Read(t, pool, iss.ID)

		update := func(title string) error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := store.UpdateIssueBlock(ctx, tx, iss.ID, store.IssueUpdate{Title: &title}, nil, scopes); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
		refused(t, update("u386 issue target"), iss.ID, before)
		// Control: a free title lands.
		if err := update("u386 issue free"); err != nil {
			t.Fatalf("free issue rename = %v", err)
		}
		if got := u386Read(t, pool, iss.ID); got.title != prefix+"u386 issue free" {
			t.Errorf("free issue rename not applied: %+v", got)
		}
	})

	// The pins: what the gate does not refuse.
	t.Run("a rename onto a free identity lands", func(t *testing.T) {
		c := u386Client(t, pool, category, "u386 free old", scope)
		b, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr("u386 free new")}, scopes)
		if err != nil || b == nil || b.Title != "u386 free new" {
			t.Fatalf("free rename = %v / %+v", err, b)
		}
	})

	t.Run("a rename onto the identity of an archived ordinary block lands", func(t *testing.T) {
		old := u386Client(t, pool, category, "u386 archived plain", scope)
		if _, err := store.DeleteBlock(ctx, pool, old.ID, scopes); err != nil {
			t.Fatalf("archive plain block: %v", err)
		}
		c := u386Client(t, pool, category, "u386 archived plain (client)", scope)
		b, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr("u386 archived plain")}, scopes)
		if err != nil || b == nil || b.Title != "u386 archived plain" {
			t.Fatalf("rename onto an archived ordinary identity = %v / %+v", err, b)
		}
	})

	t.Run("a rename onto a title an archived derivative holds in another scope lands", func(t *testing.T) {
		u386Derivative(t, pool, category, "u386 elsewhere", other, "arm-a", true)
		c := u386Client(t, pool, category, "u386 elsewhere (client)", scope)
		b, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr("u386 elsewhere")}, scopes)
		if err != nil || b == nil || b.Title != "u386 elsewhere" || b.Scope != scope {
			t.Fatalf("rename onto a title claimed only in another scope = %v / %+v", err, b)
		}
	})

	t.Run("an update that keeps the identity of a block beside an archived derivative lands", func(t *testing.T) {
		// A block that already stands on such an identity (written before this
		// gate, or by a path outside the Go code) stays editable: the gate is
		// about moving onto the identity, and an update that sends the identity
		// it already has moves nothing.
		const title = "u386 legacy"
		u386Derivative(t, pool, category, title, scope, "arm-a", true)
		c := u386Client(t, pool, category, title+" (client)", scope)
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET title = $2 WHERE id = $1::uuid`, c.ID, title); err != nil {
			t.Fatalf("set up the legacy state: %v", err)
		}
		b, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{
			Category: strptr(category), Title: strptr(title), Scope: strptr(scope), Content: strptr("edited"),
		}, scopes)
		if err != nil || b == nil || b.Content != "edited" {
			t.Fatalf("update keeping the identity = %v / %+v", err, b)
		}
		b, _, err = store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Content: strptr("edited again")}, scopes)
		if err != nil || b == nil || b.Content != "edited again" {
			t.Fatalf("content-only update = %v / %+v", err, b)
		}
	})

	t.Run("a rename onto a live block's identity stays the identity collision", func(t *testing.T) {
		// #227 is unchanged, also where an archived derivative holds the same
		// identity next to the live holder.
		const title = "u386 live holder"
		u386Derivative(t, pool, category, title, scope, "arm-a", true)
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ($1, $2, 'live holder', $3)`,
			category, title, scope); err != nil {
			t.Fatalf("live holder: %v", err)
		}
		c := u386Client(t, pool, category, title+" (client)", scope)
		_, _, err := store.UpdateBlock(ctx, pool, c.ID, store.UpdateBlockData{Title: strptr(title)}, scopes)
		var taken *store.BlockIdentityTakenError
		if !errors.As(err, &taken) {
			t.Fatalf("update = %v, want *BlockIdentityTakenError", err)
		}
	})
}

//go:build integration

// Issue #284, point 4: the confirm of a supersedes guess whose target another
// block already settles (here a declaration) is refused like a second
// declaration — over manage dream-link-resolve, 409 supersedes_conflict with
// the head of the chain, and the guess stays unpinned.
//
//	go test -tags=integration ./internal/handler/ -run TestDreamLinkResolveConfirmOfASecondSuperseder -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDreamLinkResolveConfirmOfASecondSuperseder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	set := reg.Snapshot()
	mk := func(title string) string {
		b, err := store.UpsertBlock(ctx, pool, "p4h", title, title+" content", nil, nil, "private", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		return b.ID
	}
	tgt := mk("p4h note")
	declared, err := store.UpsertBlockSupersedes(ctx, pool, "p4h", "p4h note v2", "p4h note v2 content", nil, nil, "private", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "p4h", Types: set})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	guesser := mk("p4h note v3")
	if _, err := dream.WriteLinks(ctx, pool, set, guesser, "private", 1.0,
		[]dream.Link{{TargetID: tgt, Relationship: "supersedes", Confidence: 0.9}}); err != nil {
		t.Fatalf("guess: %v", err)
	}

	ar := &auth.AuthResult{IsValid: true, HomeScope: "private", ReadScopes: []string{"private"}, TenantID: store.DefaultTenantID}
	status, resp := callManage(t, NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil), ar, "dream-link-resolve", "",
		map[string]any{"source_id": guesser, "target_id": tgt, "relationship": "supersedes", "resolution": "confirm"})
	msg, _ := resp["error"].(string)
	if status != http.StatusConflict || resp["code"] != "supersedes_conflict" || !strings.Contains(msg, declared.ID) {
		t.Errorf("confirm of a second superseder = %d %v, want 409 supersedes_conflict naming the head %s", status, resp, declared.ID)
	}
	var pinned bool
	if err := pool.QueryRow(ctx,
		`SELECT pinned FROM context_dream_links WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`, guesser, tgt).Scan(&pinned); err != nil {
		t.Fatalf("read guess: %v", err)
	}
	if pinned {
		t.Errorf("the refused guess is pinned")
	}
}

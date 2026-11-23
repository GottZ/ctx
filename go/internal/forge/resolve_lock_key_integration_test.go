//go:build integration

// The push lock is keyed on the canonical block id (issue #509): a caller of
// the resolver that spells the id in upper case takes the lock the push
// holds with the id the database renders, whether or not a boundary
// lower-cased it first.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAResolutionWithTheBlockIDInUpperCaseMeetsThePushLock' -count=1 -v
package forge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestAResolutionWithTheBlockIDInUpperCaseMeetsThePushLock(t *testing.T) {
	ctx := context.Background()
	e := i509Diverged(t, "rlk", true)
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if ok, err := store.TryPushLock(ctx, conn, e.id(t)); err != nil || !ok {
		t.Fatalf("push lock: %v %v", ok, err)
	}
	r, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, strings.ToUpper(e.id(t)), ResolveLocal, "", store.AuditStamp{})
	if !errors.Is(err, ErrPushInFlight) {
		t.Errorf("a resolution with the id in upper case while a push holds the lock: %+v %v, want ErrPushInFlight", r, err)
	}
	if _, flagged := e.mapping(t); !flagged {
		t.Error("the refused resolution cleared the flag")
	}
}

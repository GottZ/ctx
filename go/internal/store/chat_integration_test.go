//go:build integration

// Integration tests for the F6-C2/G35 chat session store against a real PG18
// testcontainer (migration 056). These pin the SQL invariants that carry the
// security + integrity guarantees of design 06 §3.1:
//   - GetSession is gated by read_scopes ⊆ caller.ReadScopes (cross-tenant AND
//     narrower-read-scope both collapse to ErrSessionNotFound — removing the
//     `<@` predicate makes ScopeIsolation + ReadScopesSubset go red)
//   - ListSessions / DeleteSession are scope-owned (home_scope), NOT read-scope
//     filtered — a foreign tenant's chats are neither listed nor deletable
//   - ClaimTurn is a non-blocking CAS: second claim → false (409), expired
//     busy_until self-heals
//   - AppendMessage assigns a gapless seq and raises a MONOTONE HWM in one TX
//   - DELETE cascades to messages; the retention janitor honours ttl
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestChatStore -count=1 -v
package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func chatCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func sessionIDs(sessions []store.ChatSession) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

func TestChatStore(t *testing.T) {
	pool := testdb.SetupTestDB(t)

	t.Run("ScopeIsolationAndReadScopesSubset", func(t *testing.T) {
		ctx := chatCtx(t)
		// Session owned by iso-priv, snapshot read_scopes = [iso-priv, iso-hth]
		// (a private key that may also read the hth tenant).
		s, err := store.CreateSession(ctx, pool, "iso-priv", []string{"iso-priv", "iso-hth"}, "", "Subset target")
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		// Superset caller → found.
		got, err := store.GetSession(ctx, pool, s.ID, []string{"iso-priv", "iso-hth", "iso-work"})
		if err != nil || got.ID != s.ID {
			t.Fatalf("superset GetSession = (%v, %v); want session %s", got, err, s.ID)
		}
		// Exact match (order-independent) → found.
		if _, err := store.GetSession(ctx, pool, s.ID, []string{"iso-hth", "iso-priv"}); err != nil {
			t.Fatalf("exact GetSession err = %v; want nil", err)
		}
		// Narrower read scope (missing iso-hth) → 404: the shadow-corpus gate.
		if _, err := store.GetSession(ctx, pool, s.ID, []string{"iso-priv"}); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("subset-miss GetSession err = %v; want ErrSessionNotFound", err)
		}
		// Foreign tenant → 404 (indistinguishable from non-existent).
		if _, err := store.GetSession(ctx, pool, s.ID, []string{"iso-work"}); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("cross-tenant GetSession err = %v; want ErrSessionNotFound", err)
		}
		// Truly non-existent → same 404.
		if _, err := store.GetSession(ctx, pool, "00000000-0000-0000-0000-000000000000", []string{"iso-priv", "iso-hth"}); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("non-existent GetSession err = %v; want ErrSessionNotFound", err)
		}
	})

	t.Run("ListAndDeleteAreScopeOwned", func(t *testing.T) {
		ctx := chatCtx(t)
		a, err := store.CreateSession(ctx, pool, "own-a", []string{"own-a"}, "", "Owned by A")
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		b, err := store.CreateSession(ctx, pool, "own-b", []string{"own-b"}, "", "Owned by B")
		if err != nil {
			t.Fatalf("create b: %v", err)
		}

		// List of scope A shows a, never the foreign-tenant b.
		listA, err := store.ListSessions(ctx, pool, "own-a", 100)
		if err != nil {
			t.Fatalf("list A: %v", err)
		}
		idsA := sessionIDs(listA)
		if !slices.Contains(idsA, a.ID) || slices.Contains(idsA, b.ID) {
			t.Fatalf("ListSessions(own-a) = %v; want contains %s, not %s", idsA, a.ID, b.ID)
		}

		// Delete b from the WRONG scope → 404, b survives.
		if err := store.DeleteSession(ctx, pool, b.ID, "own-a"); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("cross-scope delete err = %v; want ErrSessionNotFound", err)
		}
		if _, err := store.GetSession(ctx, pool, b.ID, []string{"own-b"}); err != nil {
			t.Fatalf("b should survive cross-scope delete: %v", err)
		}
		// Delete b from its own scope → ok, then gone.
		if err := store.DeleteSession(ctx, pool, b.ID, "own-b"); err != nil {
			t.Fatalf("owned delete err = %v; want nil", err)
		}
		if _, err := store.GetSession(ctx, pool, b.ID, []string{"own-b"}); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("b after delete err = %v; want ErrSessionNotFound", err)
		}
	})

	t.Run("BusyClaimAndSelfHeal", func(t *testing.T) {
		ctx := chatCtx(t)
		s, err := store.CreateSession(ctx, pool, "busy", []string{"busy"}, "", "Busy")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// First claim → true.
		if ok, err := store.ClaimTurn(ctx, pool, s.ID, 15*time.Minute); err != nil || !ok {
			t.Fatalf("first ClaimTurn = (%v, %v); want (true, nil)", ok, err)
		}
		// Second claim → false (409), non-blocking, no error.
		if ok, err := store.ClaimTurn(ctx, pool, s.ID, 15*time.Minute); err != nil || ok {
			t.Fatalf("busy ClaimTurn = (%v, %v); want (false, nil)", ok, err)
		}
		// Expire busy_until → next claim heals a crashed turn.
		if _, err := pool.Exec(ctx, `UPDATE context_chat_sessions SET busy_until = now() - interval '1 minute' WHERE id = $1`, s.ID); err != nil {
			t.Fatalf("expire busy_until: %v", err)
		}
		if ok, err := store.ClaimTurn(ctx, pool, s.ID, 15*time.Minute); err != nil || !ok {
			t.Fatalf("expired-claim ClaimTurn = (%v, %v); want (true, nil)", ok, err)
		}
		// Release → claim succeeds again.
		if err := store.ReleaseTurn(ctx, pool, s.ID); err != nil {
			t.Fatalf("release: %v", err)
		}
		if ok, err := store.ClaimTurn(ctx, pool, s.ID, 15*time.Minute); err != nil || !ok {
			t.Fatalf("post-release ClaimTurn = (%v, %v); want (true, nil)", ok, err)
		}
	})

	t.Run("AppendSeqAndMonotoneHWM", func(t *testing.T) {
		ctx := chatCtx(t)
		s, err := store.CreateSession(ctx, pool, "hwm", []string{"hwm"}, "", "HWM")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if s.MaxSensitivity != backends.SensPublic {
			t.Fatalf("fresh session HWM = %q; want public", s.MaxSensitivity)
		}

		// user(personal) → seq 1, HWM rises to personal.
		m1, hwm1, err := store.AppendMessage(ctx, pool, s.ID, store.NewMessage{Role: "user", Content: "find the db password", Sensitivity: backends.SensPersonal})
		if err != nil || m1.Seq != 1 || hwm1 != backends.SensPersonal {
			t.Fatalf("append1 = (seq %d, %v, %v); want (1, personal, nil)", m1.Seq, hwm1, err)
		}
		// tool(credentials) → seq 2, HWM rises to credentials.
		m2, hwm2, err := store.AppendMessage(ctx, pool, s.ID, store.NewMessage{
			Role: "tool", Content: "PGPASSWORD=…", Sensitivity: backends.SensCredentials,
			ToolName: "ctx_get", ToolCallID: "call_1",
		})
		if err != nil || m2.Seq != 2 || hwm2 != backends.SensCredentials {
			t.Fatalf("append2 = (seq %d, %v, %v); want (2, credentials, nil)", m2.Seq, hwm2, err)
		}
		// assistant(public) → seq 3, HWM STAYS credentials (monotone, never lowers).
		m3, hwm3, err := store.AppendMessage(ctx, pool, s.ID, store.NewMessage{Role: "assistant", Content: "Here is the answer.", Sensitivity: backends.SensPublic})
		if err != nil || m3.Seq != 3 || hwm3 != backends.SensCredentials {
			t.Fatalf("append3 = (seq %d, %v, %v); want (3, credentials, nil)", m3.Seq, hwm3, err)
		}

		// Session row reflects the persisted HWM.
		got, err := store.GetSession(ctx, pool, s.ID, []string{"hwm"})
		if err != nil || got.MaxSensitivity != backends.SensCredentials {
			t.Fatalf("GetSession HWM = (%v, %v); want credentials", got.MaxSensitivity, err)
		}

		// All three messages, ordered by seq.
		msgs, err := store.ListMessages(ctx, pool, s.ID, 0, 0)
		if err != nil || len(msgs) != 3 {
			t.Fatalf("ListMessages = (%d msgs, %v); want 3", len(msgs), err)
		}
		for i, m := range msgs {
			if m.Seq != i+1 {
				t.Fatalf("msgs[%d].Seq = %d; want %d", i, m.Seq, i+1)
			}
		}
		if msgs[1].ToolName != "ctx_get" || msgs[1].ToolCallID != "call_1" {
			t.Fatalf("tool message fields = (%q, %q); want (ctx_get, call_1)", msgs[1].ToolName, msgs[1].ToolCallID)
		}
		// afterSeq paginates.
		tail, err := store.ListMessages(ctx, pool, s.ID, 1, 0)
		if err != nil || len(tail) != 2 || tail[0].Seq != 2 {
			t.Fatalf("ListMessages(afterSeq=1) = (%d msgs, first seq %d, %v); want (2, 2, nil)", len(tail), firstSeq(tail), err)
		}
	})

	t.Run("FailClosedSensitivityNormalization", func(t *testing.T) {
		ctx := chatCtx(t)
		s, err := store.CreateSession(ctx, pool, "failclosed", []string{"failclosed"}, "", "FC")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// An unset/invalid sensitivity must store + fold as credentials (CHECK
		// would also reject ""), never a silent public message.
		_, hwm, err := store.AppendMessage(ctx, pool, s.ID, store.NewMessage{Role: "tool", Content: "x", Sensitivity: ""})
		if err != nil || hwm != backends.SensCredentials {
			t.Fatalf("empty-sensitivity append = (%v, %v); want (credentials, nil)", hwm, err)
		}
		msgs, _ := store.ListMessages(ctx, pool, s.ID, 0, 0)
		if len(msgs) != 1 || msgs[0].Sensitivity != backends.SensCredentials {
			t.Fatalf("stored sensitivity = %v; want credentials", msgs)
		}
	})

	t.Run("AutoTitleThenCascadeDelete", func(t *testing.T) {
		ctx := chatCtx(t)
		s, err := store.CreateSession(ctx, pool, "casc", []string{"casc"}, "", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if s.Title != "New chat" {
			t.Fatalf("default title = %q; want 'New chat'", s.Title)
		}
		// Auto-title applies while still default.
		if err := store.SetTitleIfDefault(ctx, pool, s.ID, store.DeriveTitle("  What is the   capital of France?  ")); err != nil {
			t.Fatalf("set title: %v", err)
		}
		got, _ := store.GetSession(ctx, pool, s.ID, []string{"casc"})
		if got.Title != "What is the capital of France?" {
			t.Fatalf("auto-title = %q; want 'What is the capital of France?'", got.Title)
		}
		// A second auto-title is a no-op (never clobbers a non-default title).
		if err := store.SetTitleIfDefault(ctx, pool, s.ID, "should not apply"); err != nil {
			t.Fatalf("second set title: %v", err)
		}
		got2, _ := store.GetSession(ctx, pool, s.ID, []string{"casc"})
		if got2.Title != "What is the capital of France?" {
			t.Fatalf("title clobbered: %q", got2.Title)
		}

		// A message, then DELETE cascades.
		if _, _, err := store.AppendMessage(ctx, pool, s.ID, store.NewMessage{Role: "user", Content: "x", Sensitivity: backends.SensInternal}); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := store.DeleteSession(ctx, pool, s.ID, "casc"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_chat_messages WHERE session_id = $1`, s.ID).Scan(&n); err != nil {
			t.Fatalf("count messages: %v", err)
		}
		if n != 0 {
			t.Fatalf("messages after cascade delete = %d; want 0", n)
		}
	})

	t.Run("RetentionJanitor", func(t *testing.T) {
		ctx := chatCtx(t)
		s, err := store.CreateSession(ctx, pool, "ret", []string{"ret"}, "", "old")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// Off by default: ttl <= 0 deletes nothing.
		if n, err := store.DeleteExpiredSessions(ctx, pool, 0); err != nil || n != 0 {
			t.Fatalf("DeleteExpiredSessions(0) = (%d, %v); want (0, nil)", n, err)
		}
		// Age the session past the ttl.
		if _, err := pool.Exec(ctx, `UPDATE context_chat_sessions SET updated_at = now() - interval '10 days' WHERE id = $1`, s.ID); err != nil {
			t.Fatalf("age session: %v", err)
		}
		n, err := store.DeleteExpiredSessions(ctx, pool, 24*time.Hour)
		if err != nil || n < 1 {
			t.Fatalf("DeleteExpiredSessions(24h) = (%d, %v); want (>=1, nil)", n, err)
		}
		if _, err := store.GetSession(ctx, pool, s.ID, []string{"ret"}); !errors.Is(err, store.ErrSessionNotFound) {
			t.Fatalf("session after retention err = %v; want ErrSessionNotFound", err)
		}
	})
}

func firstSeq(msgs []store.ChatMessage) int {
	if len(msgs) == 0 {
		return -1
	}
	return msgs[0].Seq
}

//go:build integration

// Store gates of W02-03 (internal/store/model_aliases.go over migration 162):
// create / update / delete / get / list under the scope predicate, every
// write stamped through stampAudit, the confirm flag of a sensitivity
// downgrade as an option, and ErrModelAliasImmutable derived from the 23514
// of the identity trigger (constraint model_aliases_identity) — not from a
// message text, and not from any other 23514. The store-level identity gate
// turns red with CTX_M162_OVERLAY=no-identity (the update then goes through).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelAliasStore' ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// maWrite runs fn in one transaction and commits it when fn returns nil.
func maWrite(ctx context.Context, t *testing.T, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

func maCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_model_aliases WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count aliases: %v", err)
	}
	return n
}

func TestModelAliasStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := maSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "ma-store-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	tenantA, scopeA := mcTenant(ctx, t, pool, "ma-store-a", "active")
	_, scopeB := mcTenant(ctx, t, pool, "ma-store-b", "active")
	w := store.ModelAliasWrite{Stamp: store.APIStamp(admin.ID, "ma-store-req")}

	create := func(t *testing.T, a store.ModelAlias, scopes []string, w store.ModelAliasWrite) (store.ModelAlias, error) {
		t.Helper()
		var out store.ModelAlias
		err := maWrite(ctx, t, pool, func(tx pgx.Tx) error {
			var err error
			out, err = store.CreateModelAlias(ctx, tx, a, scopes, w)
			return err
		})
		return out, err
	}
	update := func(t *testing.T, a store.ModelAlias, scopes []string, w store.ModelAliasWrite) (store.ModelAlias, bool, error) {
		t.Helper()
		var (
			out   store.ModelAlias
			found bool
		)
		err := maWrite(ctx, t, pool, func(tx pgx.Tx) error {
			var err error
			out, found, err = store.UpdateModelAlias(ctx, tx, a, scopes, w)
			return err
		})
		return out, found, err
	}
	del := func(t *testing.T, id string, scopes []string) (store.ModelAlias, bool, error) {
		t.Helper()
		var (
			out   store.ModelAlias
			found bool
		)
		err := maWrite(ctx, t, pool, func(tx pgx.Tx) error {
			var err error
			out, found, err = store.DeleteModelAlias(ctx, tx, id, scopes, w)
			return err
		})
		return out, found, err
	}

	global, err := create(t, store.ModelAlias{
		Scope: store.GlobalScope, Name: "st-chat", Kind: "chat", Role: "chat", Sensitivity: "credentials",
		Policy: json.RawMessage(`{"max_n": 2}`), Description: "operator chat", Enabled: true,
	}, nil, w)
	if err != nil {
		t.Fatalf("create _global alias: %v", err)
	}

	t.Run("create_returns_the_row_and_stamps_the_audit", func(t *testing.T) {
		if global.ID == "" || global.Name != "st-chat" || global.Scope != store.GlobalScope || global.Kind != "chat" ||
			global.Sensitivity != "credentials" || string(global.Policy) != `{"max_n": 2}` || !global.Enabled ||
			global.CreatedAt.IsZero() || !global.UpdatedAt.Equal(global.CreatedAt) {
			t.Fatalf("created row %+v", global)
		}
		rows := kaRows(ctx, t, pool, "model_alias", global.ID)
		if len(rows) != 1 || rows[0].Action != "create" || rows[0].APIKeyID == nil || *rows[0].APIKeyID != admin.ID ||
			rows[0].Meta["via"] != "api" || rows[0].Meta["request_id"] != "ma-store-req" {
			t.Errorf("audit of the create: %+v, want one create row via api with the admin actor", rows)
		}
	})
	t.Run("nil_policy_is_the_empty_object", func(t *testing.T) {
		a, err := create(t, store.ModelAlias{Scope: scopeA, Name: "st-empty", Kind: "embedding", Role: "embed", Sensitivity: "credentials", Enabled: true}, nil, w)
		if err != nil || string(a.Policy) != "{}" {
			t.Fatalf("create without policy: %+v, %v — want policy {}", a, err)
		}
	})
	t.Run("create_outside_the_scopes_is_refused", func(t *testing.T) {
		_, err := create(t, store.ModelAlias{Scope: scopeB, Name: "st-foreign", Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true},
			[]string{scopeA}, w)
		if !errors.Is(err, store.ErrModelAliasScope) {
			t.Fatalf("create in %s with scopes [%s]: err %v, want ErrModelAliasScope", scopeB, scopeA, err)
		}
		if n := maCount(ctx, t, pool, `name = 'st-foreign'`); n != 0 {
			t.Errorf("refused create left %d row(s)", n)
		}
		if _, err := create(t, store.ModelAlias{Scope: store.GlobalScope, Name: "st-foreign", Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true},
			[]string{scopeA}, w); !errors.Is(err, store.ErrModelAliasScope) {
			t.Errorf("create in _global with scopes [%s]: err %v, want ErrModelAliasScope", scopeA, err)
		}
	})
	t.Run("duplicate_is_ErrModelAliasExists", func(t *testing.T) {
		_, err := create(t, store.ModelAlias{Scope: store.GlobalScope, Name: "st-chat", Kind: "rerank", Role: "rerank", Sensitivity: "credentials", Enabled: true}, nil, w)
		if !errors.Is(err, store.ErrModelAliasExists) {
			t.Fatalf("second _global/st-chat: err %v, want ErrModelAliasExists", err)
		}
	})

	shadow, err := create(t, store.ModelAlias{Scope: scopeA, Name: "st-chat", Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true}, []string{scopeA}, w)
	if err != nil {
		t.Fatalf("create tenant alias inside its scopes: %v", err)
	}
	if rows := kaRows(ctx, t, pool, "model_alias", shadow.ID); len(rows) != 1 || rows[0].Meta["tenant_id"] != tenantA {
		t.Errorf("tenant alias audit: %+v, want metadata.tenant_id %s", rows, tenantA)
	}

	t.Run("get_under_the_scope_predicate", func(t *testing.T) {
		got, found, err := store.GetModelAlias(ctx, pool, shadow.ID, nil)
		if err != nil || !found || got.ID != shadow.ID || got.Scope != scopeA {
			t.Fatalf("get as server-admin: %+v %v %v", got, found, err)
		}
		if _, found, err := store.GetModelAlias(ctx, pool, shadow.ID, []string{scopeB}); err != nil || found {
			t.Errorf("get of A's alias with scopes [%s]: found %v err %v, want not found", scopeB, found, err)
		}
		if _, found, err := store.GetModelAlias(ctx, pool, "not-a-uuid", nil); err != nil || found {
			t.Errorf("get of a malformed id: found %v err %v, want not found without error", found, err)
		}
	})
	t.Run("list_under_the_scope_predicate", func(t *testing.T) {
		all, err := store.ListModelAliases(ctx, pool, nil)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var names []string
		for _, a := range all {
			names = append(names, a.Scope+"/"+a.Name)
		}
		want := []string{"_global/st-chat", scopeA + "/st-chat", scopeA + "/st-empty"}
		if len(names) != len(want) || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
			t.Errorf("list as server-admin: %v, want %v (ordered by scope, name)", names, want)
		}
		onlyA, err := store.ListModelAliases(ctx, pool, []string{scopeA})
		if err != nil || len(onlyA) != 2 || onlyA[0].Scope != scopeA || onlyA[1].Scope != scopeA {
			t.Errorf("list with scopes [%s]: %+v %v, want A's two aliases", scopeA, onlyA, err)
		}
		if none, err := store.ListModelAliases(ctx, pool, []string{scopeB}); err != nil || len(none) != 0 {
			t.Errorf("list with scopes [%s]: %+v %v, want none", scopeB, none, err)
		}
	})
	t.Run("update_writes_the_mutable_columns", func(t *testing.T) {
		in := shadow
		in.Role, in.Sensitivity, in.Policy, in.Description, in.Enabled = "chat-b", "personal", json.RawMessage(`{"max_n": 3}`), "tenant chat", false
		got, found, err := update(t, in, []string{scopeA}, w)
		if err != nil || !found {
			t.Fatalf("update: found %v err %v", found, err)
		}
		if got.Role != "chat-b" || got.Sensitivity != "personal" || string(got.Policy) != `{"max_n": 3}` || got.Description != "tenant chat" ||
			got.Enabled || !got.UpdatedAt.After(shadow.UpdatedAt) || !got.CreatedAt.Equal(shadow.CreatedAt) {
			t.Errorf("updated row %+v (before %+v)", got, shadow)
		}
		rows := kaRows(ctx, t, pool, "model_alias", shadow.ID)
		if got := kaActions(rows); got != "create,update" {
			t.Fatalf("audit actions %s, want create,update", got)
		}
		if _, has := rows[len(rows)-1].Meta["confirm_sensitivity_downgrade"]; has {
			t.Errorf("an update without the option carries confirm_sensitivity_downgrade: %v", rows[len(rows)-1].Meta)
		}
	})
	t.Run("confirm_option_reaches_the_audit_row", func(t *testing.T) {
		in, _, err := store.GetModelAlias(ctx, pool, global.ID, nil)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		in.Sensitivity = "public"
		if _, found, err := update(t, in, nil, store.ModelAliasWrite{Stamp: w.Stamp, ConfirmSensitivityDowngrade: true}); err != nil || !found {
			t.Fatalf("downgrade with confirm: found %v err %v", found, err)
		}
		rows := kaRows(ctx, t, pool, "model_alias", global.ID)
		if len(rows) == 0 {
			t.Fatal("no audit row")
		}
		if last := rows[len(rows)-1]; last.Meta["confirm_sensitivity_downgrade"] != true || last.New["sensitivity"] != "public" {
			t.Errorf("last audit row %+v, want sensitivity public with confirm_sensitivity_downgrade true", last)
		}
	})
	t.Run("update_outside_the_scopes_finds_nothing", func(t *testing.T) {
		in, _, _ := store.GetModelAlias(ctx, pool, shadow.ID, nil)
		in.Description = "foreign write"
		if _, found, err := update(t, in, []string{scopeB}, w); err != nil || found {
			t.Fatalf("update of A's alias with scopes [%s]: found %v err %v, want not found", scopeB, found, err)
		}
		if n := maCount(ctx, t, pool, `id = $1::uuid AND description = 'foreign write'`, shadow.ID); n != 0 {
			t.Errorf("foreign update changed the row")
		}
	})
	// The identity probes use an alias of their own: under the no-identity
	// overlay their updates go through and must not move the rows the other
	// subtests read.
	pinned, err := create(t, store.ModelAlias{Scope: scopeA, Name: "st-pin", Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true}, nil, w)
	if err != nil {
		t.Fatalf("create the pinned alias: %v", err)
	}
	for _, c := range []struct {
		field string
		mut   func(a *store.ModelAlias)
	}{
		{"name", func(a *store.ModelAlias) { a.Name = "st-chat-renamed" }},
		{"kind", func(a *store.ModelAlias) { a.Kind = "embedding" }},
		{"scope", func(a *store.ModelAlias) { a.Scope = scopeB }},
	} {
		t.Run("update_of_"+c.field+"_is_ErrModelAliasImmutable", func(t *testing.T) {
			in, _, _ := store.GetModelAlias(ctx, pool, pinned.ID, nil)
			c.mut(&in)
			_, _, err := update(t, in, nil, w)
			var imm *store.ModelAliasImmutableError
			if !errors.Is(err, store.ErrModelAliasImmutable) || !errors.As(err, &imm) || imm.Field != c.field {
				t.Fatalf("update changing %s: err %v, want ErrModelAliasImmutable naming %s", c.field, err, c.field)
			}
		})
	}
	// Fremd-Review F-1: a policy that is no JSON raises 22P02 too — it must
	// surface as an error, not read as "no such alias" over an aborted
	// transaction.
	t.Run("malformed_policy_is_an_error_not_not_found", func(t *testing.T) {
		in, _, _ := store.GetModelAlias(ctx, pool, pinned.ID, nil)
		in.Policy = json.RawMessage(`{bad`)
		got, found, err := update(t, in, nil, w)
		if err == nil || found || mcSQLState(err) != "22P02" {
			t.Fatalf("update with a malformed policy: %+v found %v err %v, want the 22P02 as an error", got, found, err)
		}
		if _, found, err := update(t, store.ModelAlias{ID: "not-a-uuid", Scope: scopeA, Name: "x", Kind: "chat", Role: "chat",
			Sensitivity: "credentials", Policy: json.RawMessage(`{bad`)}, nil, w); err != nil || found {
			t.Errorf("update of a malformed id: found %v err %v, want not found without error (the id decides first)", found, err)
		}
	})
	// Fremd-Review F-3/F-4: the confirm flag belongs to the one write that
	// asked for it — not to a later write of the same transaction, and not to
	// a later transaction on the same connection.
	t.Run("confirm_flag_is_per_write", func(t *testing.T) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		step := func(sens string, confirm bool) {
			t.Helper()
			in, _, _ := store.GetModelAlias(ctx, pool, pinned.ID, nil)
			in.Sensitivity = sens
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, _, err := store.UpdateModelAlias(ctx, tx, in, nil, store.ModelAliasWrite{Stamp: w.Stamp, ConfirmSensitivityDowngrade: confirm}); err != nil {
				t.Fatalf("update %s: %v", sens, err)
			}
			// Same transaction, second write without the option.
			in.Sensitivity = "internal"
			if sens == "internal" {
				in.Sensitivity = "personal"
			}
			if _, _, err := store.UpdateModelAlias(ctx, tx, in, nil, w); err != nil {
				t.Fatalf("second update: %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		step("public", true)
		step("credentials", false)
		rows := kaRows(ctx, t, pool, "model_alias", pinned.ID)
		if len(rows) < 4 {
			t.Fatalf("%d audit row(s), want at least the 4 of the two transactions", len(rows))
		}
		var flags []any
		for _, r := range rows[len(rows)-4:] {
			flags = append(flags, r.Meta["confirm_sensitivity_downgrade"])
		}
		if flags[0] != true || flags[1] != nil || flags[2] != nil || flags[3] != nil {
			t.Errorf("confirm flags of [confirmed write, same-tx write, next-tx write, same-tx write] = %v, want [true <nil> <nil> <nil>]", flags)
		}
	})
	t.Run("other_check_violation_is_not_immutable", func(t *testing.T) {
		in, _, _ := store.GetModelAlias(ctx, pool, pinned.ID, nil)
		in.Role = "Not A Role"
		_, _, err := update(t, in, nil, w)
		if err == nil || errors.Is(err, store.ErrModelAliasImmutable) || mcSQLState(err) != "23514" {
			t.Fatalf("update with an invalid role: err %v, want the role CHECK's 23514, not ErrModelAliasImmutable", err)
		}
	})
	t.Run("delete_under_the_scope_predicate", func(t *testing.T) {
		if _, found, err := del(t, shadow.ID, []string{scopeB}); err != nil || found {
			t.Fatalf("delete of A's alias with scopes [%s]: found %v err %v, want not found", scopeB, found, err)
		}
		got, found, err := del(t, shadow.ID, []string{scopeA})
		if err != nil || !found || got.ID != shadow.ID || got.Name != "st-chat" {
			t.Fatalf("delete inside the scopes: %+v %v %v", got, found, err)
		}
		if _, found, err := del(t, shadow.ID, nil); err != nil || found {
			t.Errorf("second delete: found %v err %v, want not found", found, err)
		}
		rows := kaRows(ctx, t, pool, "model_alias", shadow.ID)
		if got := kaActions(rows); got != "create,update,delete" {
			t.Fatalf("audit actions %s, want create,update,delete", got)
		}
		if last := rows[len(rows)-1]; last.APIKeyID == nil || *last.APIKeyID != admin.ID || last.Meta["tenant_id"] != tenantA {
			t.Errorf("delete row %+v, want the admin actor and metadata.tenant_id %s", last, tenantA)
		}
	})
	t.Run("malformed_actor_fails_before_the_write", func(t *testing.T) {
		_, err := create(t, store.ModelAlias{Scope: store.GlobalScope, Name: "st-bad-actor", Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true},
			nil, store.ModelAliasWrite{Stamp: store.APIStamp("not-a-uuid", "")})
		if err == nil || mcSQLState(err) != "22P02" {
			t.Fatalf("create with a malformed actor: err %v, want 22P02 from the stamp", err)
		}
		if n := maCount(ctx, t, pool, `name = 'st-bad-actor'`); n != 0 {
			t.Errorf("write went through without its stamp")
		}
	})
}

//go:build integration

// N-26 integration probes: the MCP `store` tool carries an explicit `type`.
//
// Ist-Stand before this wave: `storeInput` had no `type` field at all, so an
// MCP writer could only ever get the type the TITLE classifier guessed
// (`type_source='auto'`), while REST /api/store has taken an explicit `type`
// since WF T10. A `type` sent by a client was dropped silently at unmarshal
// time — neither honoured nor refused.
//
// The probes drive the arms through a JSON unmarshal into `storeInput` rather
// than a struct literal on purpose: that is exactly the step the MCP SDK
// performs on a client's tool arguments, and it is the only shape in which
// the pre-wave "silently dropped" behaviour is observable at all (a struct
// literal cannot express a field that does not exist).
//
// Subtests:
//
//	a ExplicitTypeIsManual   — type=reference ⇒ type_name/type_source manual. RED: knowledge/auto.
//	b UnknownTypeRejected    — unregistered name ⇒ rejected, nothing written. RED: stored.
//	c StagedTypeIsHashBound  — flagged key stages WITH the type, confirm executes it. RED: type lost.
//	d NoTypeHashUnchanged    — no type ⇒ the pre-wave payload_hash, byte for byte. GREEN before+after.
//	e AutoClassifierYields   — an explicit type survives the classify hook. RED: title wins.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPStoreExplicitType -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// etNoTypeHash pins the canonical payload hash of the (d) fixture as it was
// BEFORE this wave — measured on the pre-change binary, not recomputed from
// the post-change code. `type` is `omitempty`, so an absent type must not move
// a single byte of an existing staged card: a client mid-flight between the
// two versions confirms the same hash it was handed.
const etNoTypeHash = "5269f07b515958fa5efa8d81dd3718e26783aaf2f48e535d91937d8a314a1850"

func TestMCPStoreExplicitType(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()

	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}}

	mkKey := func(name string, flagged bool) context.Context {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, name, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		if flagged {
			if _, err := pool.Exec(ctx,
				`UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
				t.Fatalf("opt in %s: %v", name, err)
			}
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v (valid=%v)", name, err, ar != nil && ar.IsValid)
		}
		return context.WithValue(ctx, authResultKey, ar)
	}
	// wireInput decodes tool arguments the way the MCP SDK does — the only
	// way to express "the client sent a type" against both code versions.
	wireInput := func(args string) storeInput {
		t.Helper()
		var in storeInput
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			t.Fatalf("decode tool arguments: %v", err)
		}
		return in
	}
	callStore := func(keyCtx context.Context, args string) (text string, isErr bool) {
		t.Helper()
		r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, wireInput(args))
		if err != nil {
			t.Fatalf("mcp store: protocol error %v", err)
		}
		return resultText(t, r), r.IsError
	}
	typeOf := func(title string) (name, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE title = $1`, title).
			Scan(&name, &source); err != nil {
			t.Fatalf("read type of %q: %v", title, err)
		}
		return
	}
	blockCount := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count blocks %q: %v", title, err)
		}
		return n
	}
	staged := func(title string) (cw store.CanonicalWrite, hash string) {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT payload, payload_hash FROM context_pending_writes
			  WHERE payload->>'title' = $1 ORDER BY created_at DESC LIMIT 1`, title).
			Scan(&raw, &hash); err != nil {
			t.Fatalf("read staged payload %q: %v", title, err)
		}
		if err := json.Unmarshal(raw, &cw); err != nil {
			t.Fatalf("decode staged payload: %v", err)
		}
		return
	}

	t.Run("a_ExplicitTypeIsManual", func(t *testing.T) {
		// 'reference' is a registered non-default type whose classify rules
		// carry no title patterns — asserted here, so the probe can never
		// pass because the CLASSIFIER happened to pick the same name.
		const title = "w023 explicit type on the direct arm"
		if name, matched := set.Classify(title, nil); matched {
			t.Fatalf("fixture: title classifies as %q — pick a title the classifier ignores", name)
		}
		keyCtx := mkKey("et-a-direct", false)

		if text, isErr := callStore(keyCtx, `{"category":"test",
			"title":"w023 explicit type on the direct arm",
			"content":"an MCP writer names the type instead of hinting at it",
			"type":"reference"}`); isErr {
			t.Fatalf("store with an explicit type was rejected: %s", text)
		}
		if name, source := typeOf(title); name != "reference" || source != "manual" {
			t.Errorf("stored type = (%q, %q), want (reference, manual)", name, source)
		}
	})

	t.Run("b_UnknownTypeRejected", func(t *testing.T) {
		// Fail-closed, not silently ignored: the same validateTypeNameAgainstSet
		// class REST answers 422 with.
		const title = "w023 unregistered type name"
		keyCtx := mkKey("et-b-unknown", false)

		text, isErr := callStore(keyCtx, `{"category":"test",
			"title":"w023 unregistered type name",
			"content":"the registry has never heard of this type",
			"type":"nicht-existent"}`)
		if !isErr {
			t.Fatalf("unknown type accepted, want the registry gate to reject it")
		}
		if !strings.Contains(text, "unknown block type") {
			t.Errorf("rejection = %q, want the validateTypeNameAgainstSet class", text)
		}
		if got := blockCount(title); got != 0 {
			t.Errorf("rejected call wrote %d block(s), want 0", got)
		}
	})

	t.Run("c_StagedTypeIsHashBound", func(t *testing.T) {
		// The staged card must carry the type: a confirm selects by hash and
		// can alter nothing, so a type dropped at stage time is unrecoverable.
		const title = "w023 explicit type through the confirm dance"
		keyCtx := mkKey("et-c-staged", true)

		text, isErr := callStore(keyCtx, `{"category":"test",
			"title":"w023 explicit type through the confirm dance",
			"content":"staged with a type, confirmed with the same type",
			"type":"reference"}`)
		if !isErr || !strings.Contains(text, "STAGED — NOT saved yet") {
			t.Fatalf("flagged key did not stage: %s", text)
		}
		cw, hash := staged(title)
		if cw.Type != "reference" {
			t.Fatalf("staged payload type = %q, want reference (hash-bound)", cw.Type)
		}
		if got := blockCount(title); got != 0 {
			t.Fatalf("staging wrote %d block(s), want 0", got)
		}

		r, _, err := mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: protocol error %v", err)
		}
		if r.IsError {
			t.Fatalf("confirm rejected: %s", resultText(t, r))
		}
		if name, source := typeOf(title); name != "reference" || source != "manual" {
			t.Errorf("confirmed type = (%q, %q), want (reference, manual)", name, source)
		}
	})

	t.Run("d_NoTypeHashUnchanged", func(t *testing.T) {
		// Regression anchor: an absent type is `omitempty`, so the canonical
		// bytes — and therefore the hash a client already holds — are the
		// pre-wave ones. Fixture kept fully deterministic (no tags, no
		// metadata, benign content, settings default sensitivity).
		const title = "w023 regression anchor without a type"
		keyCtx := mkKey("et-d-anchor", true)

		text, isErr := callStore(keyCtx, `{"category":"test",
			"title":"w023 regression anchor without a type",
			"content":"no type field at all"}`)
		if !isErr || !strings.Contains(text, "STAGED — NOT saved yet") {
			t.Fatalf("flagged key did not stage: %s", text)
		}
		cw, hash := staged(title)
		if cw.Type != "" {
			t.Errorf("staged payload type = %q, want empty for a type-less store", cw.Type)
		}
		if hash != etNoTypeHash {
			t.Errorf("payload_hash = %q, want the pre-wave pin %q — the canonical bytes moved", hash, etNoTypeHash)
		}
	})

	t.Run("e_AutoClassifierYields", func(t *testing.T) {
		// The classify hook runs after every MCP upsert. Its UPDATE is guarded
		// by `type_source = 'auto'` (store/classify.go), pinned at unit level
		// by blocktype.TestClassify_ManualUntouched — this probe carries the
		// guarantee through the MCP write path: a title the classifier WOULD
		// claim, written with an explicit type, keeps the explicit type.
		const title = "w023 audit of the retrieval lane"
		if name, matched := set.Classify(title, nil); !matched || name == "reference" {
			t.Fatalf("fixture: title classifies as (%q, %v) — need a title the classifier claims for another type", name, matched)
		}
		keyCtx := mkKey("et-e-classify", false)

		if text, isErr := callStore(keyCtx, `{"category":"test",
			"title":"w023 audit of the retrieval lane",
			"content":"the title would classify audit-trail, the writer says otherwise",
			"type":"reference"}`); isErr {
			t.Fatalf("store rejected: %s", text)
		}
		if name, source := typeOf(title); name != "reference" || source != "manual" {
			t.Errorf("stored type = (%q, %q), want (reference, manual) — the classifier overrode a manual assertion", name, source)
		}
	})
}

//go:build integration

// Wave W01-2a, Nachbesserung after the adversarial review (2 blocker, 3 major,
// 2 minor). Each subtest reproduces one reviewer probe (R1–R7) against a real
// PG18 testcontainer; each was run RED against the reviewed commit 65c3ce5f
// first — reports/bau/w01-2a.md § 12 quotes the failures verbatim.
//
//	n_S3_IssueUpdate   blocker #1 — manage issue-update is the SIXTH surface and
//	                   walks past S3 entirely (UpdateIssueBlock never sees
//	                   store.UpdateBlock). RED: 200, content + provenance
//	                   overwritten.
//	o_S2_ChatStage     blocker #2 — the chat StageUpdate twin has no S2. RED: a
//	                   card into "catalog" is staged.
//	p_Card_Category    blocker #2b — the card showed the OLD category on a move,
//	                   so the human confirms blind. RED: card says "test".
//	q_Confirm_Claims   major #4 — executeConfirm re-validates scope and TOCTOU
//	                   but not S1/S2. RED: a card staged before the wave lands
//	                   after it (block in a reserved category / type=insight).
//	r_ArchiveBypass    major #5 — manage delete frees the identity, the re-upsert
//	                   walks in. RED: both calls 200.
//	s_ProvenanceClaim  major #3 — a client may set metadata.provenance and thereby
//	                   lock the digest/rootmap upsert of its scope out for good.
//	                   RED: the plant returns 200, the digest-shaped upsert dies.
//	t_ClaimOrder       minor #6 — same payload, two verdicts (403 vs 422).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestDerivedWriteLockR2 -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDerivedWriteLockR2(t *testing.T) {
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

	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfg}

	mkKey := func(label string) (context.Context, *auth.AuthResult) {
		t.Helper()
		_, plain, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", label, err)
		}
		return context.WithValue(ctx, authResultKey, ar), ar
	}
	restStore := func(keyCtx context.Context, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/store body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	restIngest := func(keyCtx context.Context, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewIngestHandler(pool).HandleIngest(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/ingest body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	restManage := func(keyCtx context.Context, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewManageHandler(pool, cfg, nil, nil, nil, nil, nil, reg).HandleManage(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/manage body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	mcpStore := func(keyCtx context.Context, in storeInput) *mcp.CallToolResult {
		t.Helper()
		r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("mcp store %q: protocol error %v", in.Title, err)
		}
		return r
	}
	mcpCode := func(r *mcp.CallToolResult) string {
		t.Helper()
		if r == nil || r.StructuredContent == nil {
			return ""
		}
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatalf("marshal StructuredContent: %v", err)
		}
		var env struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode StructuredContent %s: %v", raw, err)
		}
		return env.Code
	}
	restCode := func(resp map[string]any) string {
		c, _ := resp["code"].(string)
		return c
	}
	stateOf := func(id string) (category, content string, archived bool, meta map[string]any) {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT category, content, is_archived, metadata FROM context_blocks WHERE id = $1`, id).
			Scan(&category, &content, &archived, &raw); err != nil {
			t.Fatalf("read state of %s: %v", id, err)
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("decode metadata of %s: %v", id, err)
		}
		return
	}
	blockCount := func(category, title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE category = $1 AND title = $2 AND NOT is_archived`,
			category, title).Scan(&n); err != nil {
			t.Fatalf("count %q/%q: %v", category, title, err)
		}
		return n
	}
	// seedProvenance writes a derivative the way the ARM will — straight through
	// the store, past every handler gate.
	seedProvenance := func(category, title string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, category, title, "derived body", nil,
			map[string]any{derived.MetadataKey: map[string]any{
				"v": derived.ContractVersion, "stratum": int(derived.StratumDerived),
			}}, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed provenance block %q: %v", title, err)
		}
		return b.ID
	}

	// --- blocker #1: manage issue-update (the sixth surface) ---------------

	t.Run("n_S3_IssueUpdate", func(t *testing.T) {
		const title = "w012a r2 issue-update target"
		id := seedProvenance("learnings", title)
		_, contentBefore, _, _ := stateOf(id)
		keyCtx, _ := mkKey("wl2-n-issue")

		status, resp := restManage(keyCtx, map[string]any{
			"action": "issue-update", "id": id,
			"data": map[string]any{
				"content":  "ATTACKER text via manage issue-update",
				"metadata": map[string]any{derived.MetadataKey: "mine now"},
			},
		})
		if status == http.StatusOK {
			t.Errorf("issue-update accepted on a derivative: status 200, body %v", resp)
		}
		_, contentAfter, _, metaAfter := stateOf(id)
		if contentAfter != contentBefore {
			t.Errorf("content overwritten: %q", contentAfter)
		}
		if _, ok := metaAfter[derived.MetadataKey].(map[string]any); !ok {
			t.Errorf("provenance object replaced: %#v", metaAfter[derived.MetadataKey])
		}

		// The store primitive itself, which BOTH client entries share
		// (manage issue-update and PATCH /api/project/{id}/issues/{block_id}):
		// it must refuse a non-issue block by TYPE, the way GetIssue already
		// does — that closes the whole "issue verb writes arbitrary blocks"
		// class, not only the derived half.
		content := "direct store call"
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Content: &content}, set, []string{"private"})
		if !errors.Is(err, store.ErrIssueNotFound) {
			t.Errorf("UpdateIssueBlock on a non-issue block: err = %v, want ErrIssueNotFound", err)
		}
	})

	// --- blocker #2: the chat stage twin -----------------------------------

	t.Run("o_S2_ChatStage", func(t *testing.T) {
		const title = "w012a r2 chat stage target"
		keyCtx, _ := mkKey("wl2-o-chat")
		if status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title, "content": "an ordinary block, first",
		}); status != http.StatusOK {
			t.Fatalf("seed write: status %d (body %v)", status, resp)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("read seed id: %v", err)
		}

		runner := &chatStageRunner{pool: pool, cfg: cfg, blocktypes: reg}
		cat := "catalog"
		staged, reject, err := runner.StageUpdate(keyCtx, id, &cat, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("chat StageUpdate: infrastructural error %v", err)
		}
		if reject == "" {
			t.Errorf("chat staged an update into a reserved category (card %v)", staged)
		}
		if staged != nil {
			t.Errorf("a refused chat update produced a card: %+v", staged)
		}
		var cards int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_pending_writes WHERE payload->>'id' = $1`, id).Scan(&cards); err != nil {
			t.Fatalf("count staged cards: %v", err)
		}
		if cards != 0 {
			t.Errorf("%d staged card(s) for a refused chat update, want 0", cards)
		}
	})

	t.Run("p_Card_Category", func(t *testing.T) {
		// The ConfirmCard IS this harness's gating mechanism (chat_stage.go
		// file header). On a category move it showed block.Category — the OLD
		// one — so the human approved a move they could not see.
		const title = "w012a r2 card category target"
		keyCtx, _ := mkKey("wl2-p-card")
		if status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title, "content": "an ordinary block, first",
		}); status != http.StatusOK {
			t.Fatalf("seed write: status %d (body %v)", status, resp)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("read seed id: %v", err)
		}

		runner := &chatStageRunner{pool: pool, cfg: cfg, blocktypes: reg}
		cat := "reference"
		staged, reject, err := runner.StageUpdate(keyCtx, id, &cat, nil, nil, nil, nil)
		if err != nil || reject != "" {
			t.Fatalf("legal category move refused: reject=%q err=%v", reject, err)
		}
		if staged.Category != "reference" {
			t.Errorf("card shows category %q, want the TARGET category %q — the human approves what the card says",
				staged.Category, "reference")
		}
	})

	// --- major #4: the confirm core re-validates the claim gates ------------

	t.Run("q_Confirm_Claims", func(t *testing.T) {
		// Cards written straight through store.StagePendingWrite — the shape a
		// client already holds when the wave deploys. TTL default is 600 s and
		// writes.confirm_ttl=0 means "never expires", so the window is not
		// hypothetical.
		keyCtx, ar := mkKey("wl2-q-confirm")
		stage := func(cw store.CanonicalWrite) string {
			t.Helper()
			hash, canonical, err := cw.PayloadHash()
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if _, err := store.StagePendingWrite(ctx, pool, ar.ApiKeyID, cw.Scope, cw.Op, "mcp", canonical, hash, time.Hour); err != nil {
				t.Fatalf("stage: %v", err)
			}
			return hash
		}
		consumed := func(hash string) bool {
			t.Helper()
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*)::int FROM context_pending_writes WHERE payload_hash = $1 AND consumed_at IS NULL`,
				hash).Scan(&n); err != nil {
				t.Fatalf("read stage row: %v", err)
			}
			return n == 0
		}

		t.Run("reserved_category", func(t *testing.T) {
			hash := stage(store.CanonicalWrite{
				Op: "store", Scope: "private", Category: "catalog",
				Title: "w012a r2 pre-wave card category", Content: "staged before the wave",
			})
			out := executeConfirm(keyCtx, pool, reg, ar, hash)
			if out.Kind == confirmOK {
				t.Errorf("confirm executed a pre-wave card into a reserved category")
			}
			if got := blockCount("catalog", "w012a r2 pre-wave card category"); got != 0 {
				t.Errorf("%d block(s) in the reserved category, want 0", got)
			}
			if consumed(hash) {
				t.Errorf("the stage token was burned by a claim rejection — a reject before the consume must leave it intact")
			}
		})

		t.Run("reserved_type", func(t *testing.T) {
			hash := stage(store.CanonicalWrite{
				Op: "store", Scope: "private", Category: "test",
				Title: "w012a r2 pre-wave card type", Content: "staged before the wave",
				Type: derived.TypeInsight,
			})
			out := executeConfirm(keyCtx, pool, reg, ar, hash)
			if out.Kind == confirmOK {
				t.Errorf("confirm executed a pre-wave card claiming a derived type")
			}
			if got := blockCount("test", "w012a r2 pre-wave card type"); got != 0 {
				t.Errorf("%d block(s) written for a refused claim, want 0", got)
			}
			if consumed(hash) {
				t.Errorf("the stage token was burned by a claim rejection")
			}
		})
	})

	// --- major #5: archive + re-upsert -------------------------------------

	t.Run("r_ArchiveBypass", func(t *testing.T) {
		const title = "w012a r2 archive bypass target"
		id := seedProvenance("learnings", title)
		keyCtx, _ := mkKey("wl2-r-archive")

		status, resp := restManage(keyCtx, map[string]any{"action": "delete", "id": id})
		if status == http.StatusOK {
			if ok, _ := resp["success"].(bool); ok {
				t.Errorf("manage delete archived a derivative: %v", resp)
			}
		}
		if _, _, archived, _ := stateOf(id); archived {
			t.Errorf("the derivative is archived — the first step alone is data loss at the derivative")
		}

		// Second step, probed independently of the first: even if the identity
		// were free, the re-upsert must not walk in.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true WHERE id = $1`, id); err != nil {
			t.Fatalf("force-archive for the second half of the probe: %v", err)
		}
		status, resp = restStore(keyCtx, map[string]any{
			"category": "learnings", "title": title,
			"content": "attacker block on the freed identity",
		})
		if status != http.StatusForbidden {
			t.Errorf("re-upsert on the archived derivative's identity: status = %d, want 403 (body %v)", status, resp)
		}
		if got := blockCount("learnings", title); got != 0 {
			t.Errorf("%d live block(s) on the derivative's identity, want 0", got)
		}
	})

	// --- major #3: provenance is not a client-writable key ------------------

	t.Run("s_ProvenanceClaim", func(t *testing.T) {
		keyCtx, _ := mkKey("wl2-s-plant")
		plant := map[string]any{derived.MetadataKey: map[string]any{"v": 1, "stratum": 1}}

		t.Run("rest_store", func(t *testing.T) {
			status, resp := restStore(keyCtx, map[string]any{
				"category": "index", "title": "topic-map-private",
				"content": "client-planted", "metadata": plant,
			})
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %v)", status, resp)
			}
			if got := restCode(resp); got != "reserved_metadata" {
				t.Errorf("code = %q, want reserved_metadata (body %v)", got, resp)
			}
			if got := blockCount("index", "topic-map-private"); got != 0 {
				t.Errorf("the plant landed (%d block(s))", got)
			}
		})

		t.Run("mcp_store", func(t *testing.T) {
			r := mcpStore(keyCtx, storeInput{
				Category: "index", Title: "topic-map-private-mcp",
				Content: "client-planted", Metadata: plant,
			})
			if !r.IsError {
				t.Fatalf("MCP accepted a client-set provenance: %s", resultText(t, r))
			}
			if got := mcpCode(r); got != "reserved_metadata" {
				t.Errorf("code = %q, want reserved_metadata", got)
			}
		})

		t.Run("ingest", func(t *testing.T) {
			status, resp := restIngest(keyCtx, map[string]any{
				"source": "w012a-r2", "chunks": []map[string]any{{
					"category": "index", "title": "topic-map-private-ingest",
					"content": "client-planted", "metadata": plant,
				}},
			})
			if status != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body %v)", status, resp)
			}
			if got := blockCount("index", "topic-map-private-ingest"); got != 0 {
				t.Errorf("the plant landed (%d block(s))", got)
			}
		})

		t.Run("manage_update", func(t *testing.T) {
			const title = "w012a r2 plant via update"
			if status, resp := restStore(keyCtx, map[string]any{
				"category": "test", "title": title, "content": "an ordinary block, first",
			}); status != http.StatusOK {
				t.Fatalf("seed write: status %d (body %v)", status, resp)
			}
			var id string
			if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
				t.Fatalf("read seed id: %v", err)
			}
			status, resp := restManage(keyCtx, map[string]any{
				"action": "update", "id": id, "data": map[string]any{"metadata": plant},
			})
			if status != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (body %v)", status, resp)
			}
			if _, _, _, meta := stateOf(id); meta[derived.MetadataKey] != nil {
				t.Errorf("the plant landed on the block: %#v", meta[derived.MetadataKey])
			}
		})

		// The point of the whole finding: with the plant refused, the system
		// writer of that identity keeps working. This is the exact call
		// digest/digest.go:146 makes.
		t.Run("digest_upsert_survives", func(t *testing.T) {
			_, err := store.UpsertBlock(ctx, pool, "index", "topic-map-private",
				"the digest's own topic map", []string{"index", "topic-map"},
				map[string]any{"source": "context-digest", "is_meta": true},
				"private", true, store.SensitivityWrite{}, "")
			if err != nil {
				t.Fatalf("the digest-shaped upsert died: %v — a client key can shut the aggregate arms of its scope down", err)
			}
		})
	})

	// --- RR-#1 (round 3): the issue domain's metadata half -------------------

	t.Run("v_S3b_IssueDomain", func(t *testing.T) {
		// Round 2 restricted UpdateIssueBlock by TYPE and closed the S3 half.
		// The METADATA half stayed open on this domain: issue-create and
		// issue-comment-create hand client metadata straight into the INSERT and
		// issue-update merges it with `metadata = metadata || $n::jsonb`, none of
		// them through claimReject. Together with the round-2 archive exclusion
		// that produced a class that did not exist before: a client-created block
		// that NO client verb can remove (the issue domain has no delete, and both
		// generic archive verbs now answer 403).
		keyCtx, _ := mkKey("wl2-v-issue")
		plant := map[string]any{derived.MetadataKey: map[string]any{"v": 1, "stratum": 1}}
		issueTitle := func(id string) string {
			t.Helper()
			var title string
			if err := pool.QueryRow(ctx, `SELECT title FROM context_blocks WHERE id = $1`, id).Scan(&title); err != nil {
				t.Fatalf("read issue title: %v", err)
			}
			return title
		}
		// Scoped to the ISSUE DOMAIN on purpose: the derivatives the other
		// subtests seed through the server path legitimately carry the key, and
		// a table-wide count would make this probe fail for their existence
		// rather than for a planted issue row.
		plantedIssueRows := func() int {
			t.Helper()
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*)::int FROM context_blocks
				  WHERE metadata ? $1 AND type_name = ANY(ARRAY['issue','comment'])`,
				derived.MetadataKey).Scan(&n); err != nil {
				t.Fatalf("count planted issue rows: %v", err)
			}
			return n
		}

		t.Run("issue_create", func(t *testing.T) {
			status, resp := restManage(keyCtx, map[string]any{
				"action": "issue-create",
				"data":   map[string]any{"title": "w012a r3 planted issue", "content": "body", "metadata": plant},
			})
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %v)", status, resp)
			}
			if got := restCode(resp); got != "reserved_metadata" {
				t.Errorf("code = %q, want reserved_metadata (body %v)", got, resp)
			}
			if n := plantedIssueRows(); n != 0 {
				t.Errorf("%d issue/comment row(s) carry the provenance key after a refused issue-create, want 0", n)
			}
		})

		// A legitimate issue, which the rest of this subtest builds on.
		var legitID string
		t.Run("legit_issue_create_unchanged", func(t *testing.T) {
			status, resp := restManage(keyCtx, map[string]any{
				"action": "issue-create",
				"data": map[string]any{"title": "w012a r3 legit issue", "content": "body",
					"metadata": map[string]any{"labels": []string{"bug"}}},
			})
			if status != http.StatusOK {
				t.Fatalf("a legitimate issue-create was refused: status %d (body %v)", status, resp)
			}
			issue, _ := resp["issue"].(map[string]any)
			legitID, _ = issue["id"].(string)
			if legitID == "" {
				t.Fatalf("no issue id in the response: %v", resp)
			}
		})

		t.Run("issue_update", func(t *testing.T) {
			if legitID == "" {
				t.Skip("no legit issue")
			}
			titleBefore := issueTitle(legitID)
			status, resp := restManage(keyCtx, map[string]any{
				"action": "issue-update", "id": legitID,
				"data": map[string]any{"metadata": plant},
			})
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %v)", status, resp)
			}
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT metadata FROM context_blocks WHERE id = $1`, legitID).Scan(&raw); err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			var meta map[string]any
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatalf("decode metadata: %v", err)
			}
			if meta[derived.MetadataKey] != nil {
				t.Errorf("the key was merged onto the issue: %#v", meta[derived.MetadataKey])
			}
			if issueTitle(legitID) != titleBefore {
				t.Errorf("the refused update changed the title")
			}
		})

		t.Run("issue_comment_create", func(t *testing.T) {
			if legitID == "" {
				t.Skip("no legit issue")
			}
			status, resp := restManage(keyCtx, map[string]any{
				"action": "issue-comment-create",
				"data": map[string]any{"parent_id": legitID, "author": "probe",
					"content": "planted comment", "metadata": plant},
			})
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %v)", status, resp)
			}
			if n := plantedIssueRows(); n != 0 {
				t.Errorf("%d issue/comment row(s) carry the key after a refused comment-create, want 0", n)
			}
		})

		t.Run("mcp_issue_create", func(t *testing.T) {
			r, _, err := mcpIssueCreateHandler(mcpCfg)(keyCtx, nil, issueCreateInput{
				Title: "w012a r3 planted issue via mcp", Content: "body", Metadata: plant,
			})
			if err != nil {
				t.Fatalf("mcp issue_create: protocol error %v", err)
			}
			if !r.IsError {
				t.Fatalf("MCP issue_create accepted a client-set provenance: %s", resultText(t, r))
			}
		})

		// The consequence the reviewer chained: a client block carrying the key
		// is removable by no client verb. With the plant refused, a legitimate
		// issue stays fully manageable — no collateral from the round-2 archive
		// exclusion.
		t.Run("legit_issue_stays_deletable", func(t *testing.T) {
			if legitID == "" {
				t.Skip("no legit issue")
			}
			status, resp := restManage(keyCtx, map[string]any{"action": "delete", "id": legitID})
			if status != http.StatusOK {
				t.Fatalf("manage delete on a legitimate issue: status %d (body %v)", status, resp)
			}
			if ok, _ := resp["success"].(bool); !ok {
				t.Errorf("manage delete refused a legitimate issue: %v", resp)
			}
		})
	})

	// --- minor #7: the ingest S3 answer is the contract's 403 ---------------

	t.Run("u_Ingest_S3_Is403", func(t *testing.T) {
		// Before the Nachbesserung the store refused the chunk and ingest
		// rendered it as `200 … status:"failed"`, while docs/api.md promised
		// 403. §4.3.1 is the contract line and it says 403 — so the CODE moved,
		// not the doc: the batch is swept ahead of every write, exactly as S2
		// already was, and refused whole.
		const title = "w012a r2 ingest against a derivative"
		id := seedProvenance("learnings", title)
		_, contentBefore, _, _ := stateOf(id)
		keyCtx, _ := mkKey("wl2-u-ingest")

		status, resp := restIngest(keyCtx, map[string]any{
			"source": "w012a-r2", "chunks": []map[string]any{{
				"category": "learnings", "title": title, "content": "attacker text via ingest",
			}},
		})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", status, resp)
		}
		if got := restCode(resp); got != "provenance_protected" {
			t.Errorf("code = %q, want provenance_protected (body %v)", got, resp)
		}
		if _, contentAfter, _, _ := stateOf(id); contentAfter != contentBefore {
			t.Errorf("ingest overwrote the derivative: %q", contentAfter)
		}
	})

	// --- minor #6: one order, one verdict ----------------------------------

	t.Run("t_ClaimOrder", func(t *testing.T) {
		keyCtx, _ := mkKey("wl2-t-order")
		const title = "w012a r2 claim order target"
		if status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title, "content": "an ordinary block, first",
		}); status != http.StatusOK {
			t.Fatalf("seed write: status %d (body %v)", status, resp)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("read seed id: %v", err)
		}

		_, storeResp := restStore(keyCtx, map[string]any{
			"category": "catalog", "title": "w012a r2 both violations",
			"content": "both at once", "type": derived.TypeInsight,
		})
		_, updResp := restManage(keyCtx, map[string]any{
			"action": "update", "id": id,
			"data": map[string]any{"category": "catalog", "type": derived.TypeInsight},
		})
		a, b := restCode(storeResp), restCode(updResp)
		if a != b {
			t.Errorf("identical violation pair answers %q on /api/store and %q on manage update — "+
				"claimReject's doc comment calls that order load-bearing", a, b)
		}
	})

	_ = pgx.ErrNoRows
}

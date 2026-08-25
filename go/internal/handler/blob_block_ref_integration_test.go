//go:build integration

// W02-10 gates 1 and 4-9 at the surface layer: both blob write surfaces carry
// the blob-to-block edge, blob_link is the O(1) second phase, and a reference
// the caller cannot see is refused identically to one that does not exist.
//
// Ist-Stand before this wave: context_block_id reached no surface at all.
// /api/blob/store ignored the field (an unknown JSON key), the MCP tool had no
// such field, blob_link did not exist, and every blob the Go backend wrote
// carried NULL — so each claim below was red, most of them behaviourally
// (the REST arm) and the rest by absence of the tool.
//
// What the probes pin:
//
//	a StoreWritesTheEdge  — REST and MCP blob_store both write the column, and
//	                        the population of linked blobs grows by exactly one
//	                        per write (the count probe replacing the live 42→43
//	                        check, which is the lead's to run)
//	b LinkSetsTheEdge     — blob_link writes the edge and touches nothing else:
//	                        file_size/checksum/updated_at are identical before
//	                        and after (it is an UPDATE, not a re-upsert)
//	c BlockRefNotFound    — A1: a block in an unreadable scope answers
//	                        BYTE-IDENTICALLY to a UUID that exists nowhere, on
//	                        all three write paths, and nothing is written
//	d LinkScope           — A2: a blob outside writableBlockScopes is "not
//	                        found", identical to an absent one
//	e StageAndConfirm     — A3/N-28: a confirm_writes key STAGES blob_link
//	                        (uncoded IsError + payload_hash, no write) and the
//	                        confirm executes it; the same for a blob_store that
//	                        carries an edge
//	f Budget              — one blob_link books exactly one blob-write row
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestBlobBlockRef -count=1 -v
package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w210AbsentUUID is well-formed and belongs to nothing. Every "not found"
// verdict is compared against the one this id produces.
const w210AbsentUUID = "00000000-0000-0000-0000-0000000000ff"

// w210Answer is the wire form of a blob_store / blob_link answer.
type w210Answer struct {
	ID             string `json:"id"`
	Scope          string `json:"scope"`
	FileSize       int64  `json:"file_size"`
	Checksum       string `json:"checksum"`
	ContextBlockID string `json:"context_block_id"`
}

func TestBlobBlockRef(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	mkKey := func(name, home string, allowed []string, confirmWrites bool) (string, context.Context, *auth.AuthResult) {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, name, home, allowed, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		if confirmWrites {
			if _, err := pool.Exec(ctx,
				`UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
				t.Fatalf("opt %s into confirm_writes: %v", name, err)
			}
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", name, err)
		}
		return row.ID, context.WithValue(ctx, authResultKey, ar), ar
	}

	cfgStore := staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{BlobStageMaxBytes: 1 << 20, BlobScanMaxBytes: 1 << 20},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Cfg: cfgStore}

	seedBlock := func(title, scope string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "reference", title, "manifest "+title,
			nil, nil, scope, true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed block %q in %q: %v", title, scope, err)
		}
		return b.ID
	}
	edgeOf := func(blobID string) (string, bool) {
		t.Helper()
		var ref *string
		if err := pool.QueryRow(ctx,
			`SELECT context_block_id FROM context_blobs WHERE id = $1`, blobID).Scan(&ref); err != nil {
			t.Fatalf("read edge of %s: %v", blobID, err)
		}
		if ref == nil {
			return "", false
		}
		return *ref, true
	}
	linkedCount := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE context_block_id IS NOT NULL`).Scan(&n); err != nil {
			t.Fatalf("count linked blobs: %v", err)
		}
		return n
	}
	blobWriteRows := func(keyID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = $2`, keyID, store.ActionBlobWrite).Scan(&n); err != nil {
			t.Fatalf("count blob-write rows: %v", err)
		}
		return n
	}
	callStore := func(keyCtx context.Context, in blobStoreInput) (string, string) {
		t.Helper()
		r, _, err := mcpBlobStoreHandler(mcpCfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_store %q: protocol error %v", in.Title, err)
		}
		return resultText(t, r), ecMCPCode(t, r)
	}
	callLink := func(keyCtx context.Context, in blobLinkInput) (string, string) {
		t.Helper()
		r, _, err := mcpBlobLinkHandler(mcpCfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_link %q: protocol error %v", in.ID, err)
		}
		return resultText(t, r), ecMCPCode(t, r)
	}
	decode := func(text string) w210Answer {
		t.Helper()
		var out w210Answer
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("decode tool answer %q: %v", text, err)
		}
		return out
	}

	t.Run("a_StoreWritesTheEdge", func(t *testing.T) {
		_, keyCtx, ar := mkKey("w210-store", "w210a", nil, false)
		blockID := seedBlock("w210-manifest-store", "w210a")
		before := linkedCount()

		// MCP arm.
		text, code := callStore(keyCtx, blobStoreInput{
			Category: "reference", Title: "w210-mcp-edge", Filename: "p.ndjson",
			MimeType: "application/x-ndjson", Text: "one line\n",
			ContextBlockID: blockID,
		})
		if code != "" {
			t.Fatalf("blob_store with a block ref was refused (%q): %s", code, text)
		}
		got := decode(text)
		if got.ContextBlockID != blockID {
			t.Errorf("blob_store answer carries context_block_id %q, want %s (W02-10 A5)", got.ContextBlockID, blockID)
		}
		if edge, ok := edgeOf(got.ID); !ok || edge != blockID {
			t.Errorf("stored edge = %q (set %v), want %s", edge, ok, blockID)
		}

		// REST arm — the same field on the JSON body of /api/blob/store.
		status, resp := postBlobStore(t, NewBlobHandler(pool, cfgStore), ar, map[string]any{
			"file":             base64.StdEncoding.EncodeToString([]byte("rest payload")),
			"filename":         "p.bin",
			"category":         "reference",
			"title":            "w210-rest-edge",
			"mime_type":        "application/octet-stream",
			"context_block_id": blockID,
		})
		if status != http.StatusOK {
			t.Fatalf("REST blob-store with a block ref: status %d (body %v)", status, resp)
		}
		blob, _ := resp["blob"].(map[string]any)
		if blob == nil || blob["context_block_id"] != blockID {
			t.Fatalf("REST answer carries context_block_id %v, want %s (body %v)", blob["context_block_id"], blockID, resp)
		}
		if edge, ok := edgeOf(blob["id"].(string)); !ok || edge != blockID {
			t.Errorf("REST-stored edge = %q (set %v), want %s", edge, ok, blockID)
		}

		// A6: the container-local stand-in for the live 42→43 probe. Two writes
		// carrying an edge, two more linked blobs — the population the design
		// counts, counted where this agent may count it.
		if after := linkedCount(); after != before+2 {
			t.Errorf("blobs with a block ref: %d before, %d after two edge-carrying writes, want %d",
				before, after, before+2)
		}
	})

	t.Run("b_LinkSetsTheEdge", func(t *testing.T) {
		_, keyCtx, _ := mkKey("w210-link", "w210b", nil, false)
		blockID := seedBlock("w210-manifest-link", "w210b")

		text, code := callStore(keyCtx, blobStoreInput{
			Category: "reference", Title: "w210-phase1", Filename: "p.ndjson",
			MimeType: "application/x-ndjson", Text: "payload of phase one\n",
		})
		if code != "" {
			t.Fatalf("phase 1 refused (%q): %s", code, text)
		}
		phase1 := decode(text)
		if phase1.ContextBlockID != "" {
			t.Fatalf("phase 1 already carries an edge (%q) — the answer must omit the key when there is none", phase1.ContextBlockID)
		}
		if _, ok := edgeOf(phase1.ID); ok {
			t.Fatal("phase 1 wrote an edge without being asked for one")
		}
		var beforeUpdated time.Time
		if err := pool.QueryRow(ctx,
			`SELECT updated_at FROM context_blobs WHERE id = $1`, phase1.ID).Scan(&beforeUpdated); err != nil {
			t.Fatalf("read updated_at: %v", err)
		}

		text, code = callLink(keyCtx, blobLinkInput{ID: phase1.ID, ContextBlockID: blockID})
		if code != "" {
			t.Fatalf("blob_link refused (%q): %s", code, text)
		}
		linked := decode(text)
		if linked.ContextBlockID != blockID {
			t.Errorf("blob_link answer carries context_block_id %q, want %s", linked.ContextBlockID, blockID)
		}
		if edge, ok := edgeOf(phase1.ID); !ok || edge != blockID {
			t.Errorf("edge after blob_link = %q (set %v), want %s", edge, ok, blockID)
		}

		// Phase 2 is a LINK. The payload identity must be untouched — otherwise
		// the two-phase write costs a second payload round trip, which is the
		// only reason it is two-phased at all.
		if linked.FileSize != phase1.FileSize || linked.Checksum != phase1.Checksum {
			t.Errorf("blob_link moved the payload identity: size %d -> %d, checksum %q -> %q",
				phase1.FileSize, linked.FileSize, phase1.Checksum, linked.Checksum)
		}
		var afterUpdated time.Time
		if err := pool.QueryRow(ctx,
			`SELECT updated_at FROM context_blobs WHERE id = $1`, phase1.ID).Scan(&afterUpdated); err != nil {
			t.Fatalf("read updated_at after link: %v", err)
		}
		if !afterUpdated.Equal(beforeUpdated) {
			t.Errorf("updated_at moved %s -> %s across blob_link — the link must not read as a payload rewrite",
				beforeUpdated, afterUpdated)
		}
	})

	t.Run("c_BlockRefNotFound", func(t *testing.T) {
		// A1. The key cannot read scope w210z, so the block that lives there
		// must be as invisible as a UUID that belongs to nothing — on every
		// path that writes the edge.
		_, keyCtx, ar := mkKey("w210-ref", "w210c", nil, false)
		foreignBlock := seedBlock("w210-manifest-foreign", "w210z")
		if contains(ar.ReadScopes, "w210z") {
			t.Fatalf("probe setup broken: the key reads w210z (%v)", ar.ReadScopes)
		}

		mcpVerdict := func(blockID, title string) (string, string) {
			return callStore(keyCtx, blobStoreInput{
				Category: "reference", Title: title, Filename: "p.bin",
				MimeType: "application/octet-stream", Text: "x",
				ContextBlockID: blockID,
			})
		}
		foreignText, foreignCode := mcpVerdict(foreignBlock, "w210-ref-foreign")
		absentText, absentCode := mcpVerdict(w210AbsentUUID, "w210-ref-absent")
		if foreignCode != "constraint" || absentCode != "constraint" {
			t.Errorf("block-ref rejection codes = %q / %q, want constraint twice (texts %q / %q)",
				foreignCode, absentCode, foreignText, absentText)
		}
		if foreignText != absentText {
			t.Errorf("MCP blob_store answers a DIFFERENT verdict for a foreign block (%q) than for an absent one (%q) — that is an existence oracle",
				foreignText, absentText)
		}
		if foreignText != blobBlockRefNotFoundMsg {
			t.Errorf("block-ref verdict = %q, want %q", foreignText, blobBlockRefNotFoundMsg)
		}
		for _, title := range []string{"w210-ref-foreign", "w210-ref-absent"} {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*)::int FROM context_blobs WHERE title = $1`, title).Scan(&n); err != nil {
				t.Fatalf("count %q: %v", title, err)
			}
			if n != 0 {
				t.Errorf("a refused block ref still stored the blob %q (%d rows)", title, n)
			}
		}

		// REST arm: same verdict, same code, same status.
		h := NewBlobHandler(pool, cfgStore)
		restVerdict := func(blockID, title string) (int, map[string]any) {
			return postBlobStore(t, h, ar, map[string]any{
				"file":             base64.StdEncoding.EncodeToString([]byte("x")),
				"filename":         "p.bin",
				"category":         "reference",
				"title":            title,
				"mime_type":        "application/octet-stream",
				"context_block_id": blockID,
			})
		}
		fStatus, fResp := restVerdict(foreignBlock, "w210-rest-foreign")
		aStatus, aResp := restVerdict(w210AbsentUUID, "w210-rest-absent")
		if fStatus != http.StatusUnprocessableEntity || aStatus != fStatus {
			t.Errorf("REST block-ref statuses = %d / %d, want 422 twice", fStatus, aStatus)
		}
		if fResp["error"] != aResp["error"] || ecRESTCode(fResp) != ecRESTCode(aResp) {
			t.Errorf("REST answers differ between a foreign block (%v) and an absent one (%v) — existence oracle", fResp, aResp)
		}
		if fResp["error"] != blobBlockRefNotFoundMsg || ecRESTCode(fResp) != "constraint" {
			t.Errorf("REST block-ref verdict = %v / %q, want %q / constraint", fResp["error"], ecRESTCode(fResp), blobBlockRefNotFoundMsg)
		}

		// blob_link arm: the gate sits in front of the UPDATE, so the edge is
		// refused before any row is touched.
		text, code := callStore(keyCtx, blobStoreInput{
			Category: "reference", Title: "w210-ref-target", Filename: "p.bin",
			MimeType: "application/octet-stream", Text: "x",
		})
		if code != "" {
			t.Fatalf("seed blob for the link probe refused (%q): %s", code, text)
		}
		target := decode(text)
		linkForeign, lfCode := callLink(keyCtx, blobLinkInput{ID: target.ID, ContextBlockID: foreignBlock})
		linkAbsent, laCode := callLink(keyCtx, blobLinkInput{ID: target.ID, ContextBlockID: w210AbsentUUID})
		if linkForeign != linkAbsent || lfCode != laCode {
			t.Errorf("blob_link answers differ for a foreign block (%q/%q) and an absent one (%q/%q) — existence oracle",
				linkForeign, lfCode, linkAbsent, laCode)
		}
		if linkForeign != blobBlockRefNotFoundMsg || lfCode != "constraint" {
			t.Errorf("blob_link block-ref verdict = %q / %q, want %q / constraint", linkForeign, lfCode, blobBlockRefNotFoundMsg)
		}
		if edge, ok := edgeOf(target.ID); ok {
			t.Errorf("a refused blob_link wrote the edge anyway (%q)", edge)
		}
	})

	t.Run("d_LinkScope", func(t *testing.T) {
		// A2: a link is a WRITE to the blob row, so the blob has to lie in the
		// key's writable scopes. Outside them it is "not found" — the same
		// answer an id that belongs to nothing gets.
		_, keyCtx, _ := mkKey("w210-link-scope", "w210d", nil, false)
		blockID := seedBlock("w210-manifest-scope", "w210d")
		foreignBlob, err := store.UpsertBlob(ctx, pool, "reference", "w210-foreign-blob", "f.bin",
			"application/octet-stream", "w210q", []byte("foreign"), nil, nil, "")
		if err != nil {
			t.Fatalf("seed foreign blob: %v", err)
		}

		foreignText, foreignCode := callLink(keyCtx, blobLinkInput{ID: foreignBlob.ID, ContextBlockID: blockID})
		absentText, absentCode := callLink(keyCtx, blobLinkInput{ID: w210AbsentUUID, ContextBlockID: blockID})
		if foreignText != absentText || foreignCode != absentCode {
			t.Errorf("blob_link on a foreign blob (%q/%q) differs from an absent one (%q/%q) — existence oracle",
				foreignText, foreignCode, absentText, absentCode)
		}
		if foreignText != blobLinkNotFoundMsg || foreignCode != "constraint" {
			t.Errorf("blob_link scope verdict = %q / %q, want %q / constraint", foreignText, foreignCode, blobLinkNotFoundMsg)
		}
		if edge, ok := edgeOf(foreignBlob.ID); ok {
			t.Errorf("the foreign blob was linked anyway (%q)", edge)
		}
	})

	t.Run("e_StageAndConfirm", func(t *testing.T) {
		// A3 / N-28: a confirm_writes key never writes directly, on any surface
		// it can reach — blob_link included.
		keyID, keyCtx, _ := mkKey("w210-stage", "w210e", nil, true)
		blockID := seedBlock("w210-manifest-stage", "w210e")

		// Phase 1 through the same key: staged, then confirmed, so the blob it
		// links exists at all.
		text, code := callStore(keyCtx, blobStoreInput{
			Category: "reference", Title: "w210-staged-payload", Filename: "p.ndjson",
			MimeType: "application/x-ndjson", Text: "staged payload\n",
		})
		if code != "" {
			t.Fatalf("staged blob_store carries code %q — a staged answer is deliberately uncoded: %s", code, text)
		}
		m := hashRe.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("staged blob_store answer carries no payload_hash: %q", text)
		}
		confirmed, _, err := mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
		if err != nil {
			t.Fatalf("confirm phase 1: protocol error %v", err)
		}
		if confirmed.IsError {
			t.Fatalf("confirm of the staged payload failed: %s", resultText(t, confirmed))
		}
		var blobID string
		if err := pool.QueryRow(ctx,
			`SELECT id FROM context_blobs WHERE title = 'w210-staged-payload'`).Scan(&blobID); err != nil {
			t.Fatalf("read confirmed blob id: %v", err)
		}

		// Phase 2 is staged, not executed.
		text, code = callLink(keyCtx, blobLinkInput{ID: blobID, ContextBlockID: blockID})
		if code != "" {
			t.Errorf("staged blob_link carries code %q — the gates passed, so the answer must stay uncoded (D3-C3)", code)
		}
		if !strings.Contains(text, "STAGED — NOT saved yet") {
			t.Fatalf("blob_link for a confirm_writes key answered %q, want a STAGED card", text)
		}
		if !strings.Contains(text, blockID) {
			t.Errorf("the staged card does not name the block it would link (%q)", text)
		}
		if edge, ok := edgeOf(blobID); ok {
			t.Fatalf("a staged blob_link already wrote the edge (%q) — that is the bypass confirm_writes exists to close", edge)
		}
		linkHash := hashRe.FindStringSubmatch(text)
		if linkHash == nil {
			t.Fatalf("staged blob_link answer carries no payload_hash: %q", text)
		}
		var stagedOp, stagedScope string
		if err := pool.QueryRow(ctx,
			`SELECT op, scope FROM context_pending_writes WHERE payload_hash = $1`, linkHash[1]).Scan(&stagedOp, &stagedScope); err != nil {
			t.Fatalf("read staged blob_link row: %v", err)
		}
		if stagedOp != store.OpBlobLink || stagedScope != "w210e" {
			t.Errorf("staged row = op %q / scope %q, want %q / w210e (the BLOB's own scope, which the confirm re-validates)",
				stagedOp, stagedScope, store.OpBlobLink)
		}

		confirmed, _, err = mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: linkHash[1]})
		if err != nil {
			t.Fatalf("confirm blob_link: protocol error %v", err)
		}
		ctext := resultText(t, confirmed)
		if confirmed.IsError {
			t.Fatalf("confirm of the staged blob_link failed: %q", ctext)
		}
		if !strings.Contains(ctext, blockID) {
			t.Errorf("the confirm answer does not name the edge it wrote: %q", ctext)
		}
		if edge, ok := edgeOf(blobID); !ok || edge != blockID {
			t.Errorf("edge after confirming the link = %q (set %v), want %s", edge, ok, blockID)
		}

		// A staged blob_store that CARRIES an edge confirms with the edge — the
		// hash binds it, so the confirm cannot execute a different reference.
		text, code = callStore(keyCtx, blobStoreInput{
			Category: "reference", Title: "w210-staged-with-edge", Filename: "p.ndjson",
			MimeType: "application/x-ndjson", Text: "staged with edge\n",
			ContextBlockID: blockID,
		})
		if code != "" {
			t.Fatalf("staged blob_store with an edge was refused (%q): %s", code, text)
		}
		m = hashRe.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("staged answer carries no payload_hash: %q", text)
		}
		confirmed, _, err = mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
		if err != nil {
			t.Fatalf("confirm staged blob_store with an edge: protocol error %v", err)
		}
		if confirmed.IsError {
			t.Fatalf("confirm failed: %s", resultText(t, confirmed))
		}
		var withEdge string
		if err := pool.QueryRow(ctx,
			`SELECT id FROM context_blobs WHERE title = 'w210-staged-with-edge'`).Scan(&withEdge); err != nil {
			t.Fatalf("read confirmed blob: %v", err)
		}
		if edge, ok := edgeOf(withEdge); !ok || edge != blockID {
			t.Errorf("confirmed blob_store edge = %q (set %v), want %s — the staged payload carries the reference", edge, ok, blockID)
		}

		// The budget is charged at INTENT: three stages, three rows, and the
		// confirms book nothing on top.
		if got := blobWriteRows(keyID); got != 3 {
			t.Errorf("stage+confirm of two stores and one link booked %d blob-write row(s), want 3 (one per stage, none per confirm)", got)
		}
	})

	t.Run("f_Budget", func(t *testing.T) {
		// A link is a WRITE and is counted as one (design/02 sec. 6.4 budgets
		// both slots of a checkpoint: the payload and the link).
		keyID, keyCtx, _ := mkKey("w210-budget", "w210f", nil, false)
		blockID := seedBlock("w210-manifest-budget", "w210f")

		text, code := callStore(keyCtx, blobStoreInput{
			Category: "reference", Title: "w210-budget-payload", Filename: "p.bin",
			MimeType: "application/octet-stream", Text: "x",
		})
		if code != "" {
			t.Fatalf("seed blob refused (%q): %s", code, text)
		}
		target := decode(text)
		if got := blobWriteRows(keyID); got != 1 {
			t.Fatalf("the payload write booked %d row(s), want 1", got)
		}

		if _, code := callLink(keyCtx, blobLinkInput{ID: target.ID, ContextBlockID: blockID}); code != "" {
			t.Fatalf("blob_link refused (%q)", code)
		}
		if got := blobWriteRows(keyID); got != 2 {
			t.Errorf("after blob_link the key has booked %d blob-write row(s), want 2 — a link costs exactly one slot", got)
		}

		// A refused link costs budget too: probing ids must not be free
		// (meterBlobWrite runs ahead of the block-ref gate).
		if _, code := callLink(keyCtx, blobLinkInput{ID: target.ID, ContextBlockID: w210AbsentUUID}); code != "constraint" {
			t.Errorf("a link to an absent block answered %q, want constraint", code)
		}
		if got := blobWriteRows(keyID); got != 3 {
			t.Errorf("a refused link booked nothing (%d rows, want 3) — id probing would be free", got)
		}

		// The audit row of the successful link names the blob it paid for. The
		// attribution is fire-and-forget, so this waits for it rather than
		// asserting on the first read.
		waitForAttribution(t, pool, keyID, target.ID)
	})
}

// waitForAttribution polls for the async blob attribution of the booked audit
// row (executeBlobWrite and the blob_link handler both attribute in a
// goroutine — losing it costs the audit trail a reference, never a budget
// entry, so the probe waits instead of racing).
func waitForAttribution(t *testing.T, pool *pgxpool.Pool, keyID, blobID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = $2 AND blob_id = $3::uuid`,
			keyID, store.ActionBlobWrite, blobID).Scan(&n); err != nil {
			t.Fatalf("count attributed rows: %v", err)
		}
		if n >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("only %d audit row(s) reference blob %s, want the payload write AND the link", n, blobID)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

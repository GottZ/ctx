//go:build integration

// W02-8 gates 2-5: the MCP blob tools run the REST blob write path, stage for
// confirm_writes keys, and read ranges instead of whole payloads.
//
// Ist-Stand before this wave: there was no blob tool at all, so every claim
// below was vacuously red. What the probes actually pin is that the new
// surface did not get its OWN copies of the four mechanisms:
//
//	a HomeScopeOnly       — an MCP blob write WITHOUT a scope field lands in
//	                        ar.HomeScope even when the key holds further write
//	                        scopes, while the REST route keeps its
//	                        explicit-scope gate. (W02-8 wrote this as "the tool
//	                        has no scope field at all", decision D4; E-M4 gave
//	                        it an optional one, and the probe kept its value as
//	                        the DEFAULT pin — the explicit-scope arm lives in
//	                        mcp_write_scope_integration_test.go)
//	b StagingParity       — a confirm_writes key gets a payload_hash, not an id;
//	                        confirm executes the server-held write; a payload
//	                        over pool.blob_stage_max_bytes is REFUSED by name
//	                        rather than silently written direct (N-28)
//	c WriteBudget         — the tool books and reads store.ActionBlobWrite, incl.
//	                        the pool.blob_rate_limit_write → query.rate_limit_write
//	                        fallback VALUE semantics
//	d RangeRead           — offset/length cut the stored (uncompressed) bytes;
//	                        a length over the maximum is refused, never clamped
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPBlobTools -count=1 -v
package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// w28Payload is a deterministic ASCII payload: every byte position carries its
// own index modulo the alphabet, so a range read that is off by one is visible
// in the assertion message rather than merely unequal.
func w28Payload(n int) []byte {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[i%len(alphabet)]
	}
	return out
}

// w28FetchResult is the wire form of a blob_fetch answer — decoded from the
// tool's text content, which is what a client actually parses.
type w28FetchResult struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Title    string `json:"title"`
	FileSize int64  `json:"file_size"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	File     string `json:"file"`
}

func TestMCPBlobTools(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// mkKey mints a real key row (context_access_log.api_key_id is FK-bound)
	// and returns the authenticated AuthResult with the probe's scope sets.
	mkKey := func(name, home string, allowed, write []string, confirmWrites bool) (string, context.Context, *auth.AuthResult) {
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
		ar.WriteScopes = write
		return row.ID, context.WithValue(ctx, authResultKey, ar), ar
	}

	cfgFor := func(blobLimit, queryLimit, stageMax int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query:  config.QueryConfig{RateLimitWrite: queryLimit},
			Pool:   config.PoolConfig{BlobRateLimitWrite: blobLimit, BlobStageMaxBytes: stageMax},
			Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
		}}
	}
	mcpCfg := func(blobLimit, queryLimit, stageMax int) MCPConfig {
		return MCPConfig{Pool: pool, Cfg: cfgFor(blobLimit, queryLimit, stageMax)}
	}
	// blobStore drives the tool exactly as the SDK would.
	blobStore := func(keyCtx context.Context, cfg MCPConfig, in blobStoreInput) (string, string) {
		t.Helper()
		r, _, err := mcpBlobStoreHandler(cfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_store %q: protocol error %v", in.Title, err)
		}
		return resultText(t, r), ecMCPCode(t, r)
	}
	blobFetch := func(keyCtx context.Context, cfg MCPConfig, in blobFetchInput) (string, bool) {
		t.Helper()
		r, _, err := mcpBlobFetchHandler(cfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_fetch %q: protocol error %v", in.ID, err)
		}
		return resultText(t, r), r.IsError
	}
	blobRows := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count blobs %q: %v", title, err)
		}
		return n
	}
	blobData := func(title string) []byte {
		t.Helper()
		var data []byte
		if err := pool.QueryRow(ctx,
			`SELECT data FROM context_blobs WHERE title = $1`, title).Scan(&data); err != nil {
			t.Fatalf("read blob %q: %v", title, err)
		}
		return data
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
	pendingRows := func(keyID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_pending_writes WHERE api_key_id = $1::uuid`, keyID).Scan(&n); err != nil {
			t.Fatalf("count pending writes: %v", err)
		}
		return n
	}

	t.Run("a_HomeScopeOnly", func(t *testing.T) {
		// A blob_store call that names NO scope lands in the key's HOME scope
		// — even for a key that demonstrably holds a second write scope. The
		// probe holds both halves together, because the interesting failure is
		// the silent one: a core that resolved an empty scope to something
		// other than home_scope would still store a blob, and only the scope
		// column would say where.
		//
		// Written under decision D4 (the tool had no scope field at all), it
		// survives E-M4 unchanged as the DEFAULT pin: the field is optional,
		// and its absence must keep resolving byte-identically to this. The
		// explicit-scope arm and the scope_denied verdict are probed in
		// mcp_write_scope_integration_test.go; the REST-side belege stay in
		// blob_write_scope_integration_test.go (a/b/c/d there).
		//
		// The one-path claim of this wave is carried structurally instead:
		// both surfaces run blobWriteGate/executeBlobWrite (blob_core.go), and
		// the observable half of that sharing is subtest c — the same
		// meterBlobWrite verdict, code and prose, on the MCP arm as on the
		// REST arm.
		_, keyCtx, ar := mkKey("w28-scope", "w28a", []string{"w28a", "w28b"}, []string{"w28b"}, false)
		cfg := mcpCfg(0, 0, 1<<20)

		text, code := blobStore(keyCtx, cfg, blobStoreInput{
			Category: "reference", Title: "w28-home-scope", Filename: "s.bin",
			MimeType: "application/octet-stream",
			File:     base64.StdEncoding.EncodeToString([]byte("home")),
		})
		if code != "" {
			t.Fatalf("blob_store was refused (%q): %s", code, text)
		}
		var scope string
		if err := pool.QueryRow(ctx,
			`SELECT scope FROM context_blobs WHERE title = 'w28-home-scope'`).Scan(&scope); err != nil {
			t.Fatalf("read stored scope: %v", err)
		}
		if scope != "w28a" {
			t.Errorf("MCP blob write landed in scope %q, want the home scope w28a — a call that names no scope resolves to it (E-M4 default)", scope)
		}

		// The key really does hold w28b as a write scope: the REST route,
		// which DOES take an explicit scope, writes there. Without this half
		// the assertion above would also pass for a key that simply had no
		// second scope to land in.
		restStatus, restResp := postBlobStore(t, NewBlobHandler(pool, cfgFor(0, 0, 1<<20)), ar, map[string]any{
			"file":      base64.StdEncoding.EncodeToString([]byte("explicit")),
			"filename":  "s.bin",
			"category":  "reference",
			"title":     "w28-scope-rest",
			"mime_type": "application/octet-stream",
			"scope":     "w28b",
		})
		if restStatus != http.StatusOK {
			t.Fatalf("REST blob-store with the key's second write scope status = %d, want 200 (body %v)", restStatus, restResp)
		}

		// And the REST gate still refuses a scope the key holds neither way.
		restStatus, restResp = postBlobStore(t, NewBlobHandler(pool, cfgFor(0, 0, 1<<20)), ar, map[string]any{
			"file":      base64.StdEncoding.EncodeToString([]byte("nope")),
			"filename":  "s.bin",
			"category":  "reference",
			"title":     "w28-scope-denied-rest",
			"mime_type": "application/octet-stream",
			"scope":     "w28z",
		})
		if restStatus != http.StatusForbidden || ecRESTCode(restResp) != "scope_denied" {
			t.Errorf("REST blob-store with an unheld scope: status %d / code %q, want 403 / scope_denied (body %v)",
				restStatus, ecRESTCode(restResp), restResp)
		}
	})

	t.Run("b_StagingParity", func(t *testing.T) {
		keyID, keyCtx, _ := mkKey("w28-staging", "w28s", nil, nil, true)
		cfg := mcpCfg(0, 0, 1<<20)
		payload := w28Payload(400)

		text, code := blobStore(keyCtx, cfg, blobStoreInput{
			Category: "reference", Title: "w28-staged", Filename: "staged.ndjson",
			MimeType: "application/x-ndjson",
			File:     base64.StdEncoding.EncodeToString(payload),
		})
		if code != "" {
			t.Errorf("staged answer carries code %q — a staged write is deliberately uncoded (D3-C3)", code)
		}
		if !strings.Contains(text, "STAGED — NOT saved yet") {
			t.Fatalf("blob_store for a confirm_writes key answered %q, want a STAGED card", text)
		}
		m := hashRe.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("staged answer carries no payload_hash: %q", text)
		}
		if got := blobRows("w28-staged"); got != 0 {
			t.Fatalf("a staged blob write already reached the table (%d rows), want 0", got)
		}
		if got := pendingRows(keyID); got != 1 {
			t.Fatalf("staged blob write produced %d pending row(s), want 1", got)
		}

		confirmed, _, err := mcpConfirmHandler(cfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
		if err != nil {
			t.Fatalf("confirm: protocol error %v", err)
		}
		ctext := resultText(t, confirmed)
		if confirmed.IsError {
			t.Fatalf("confirm of a staged blob write failed: %q", ctext)
		}
		if got := blobRows("w28-staged"); got != 1 {
			t.Fatalf("after confirm the blob table holds %d row(s) for the staged title, want 1 (answer %q)", got, ctext)
		}
		if got := blobData("w28-staged"); string(got) != string(payload) {
			t.Errorf("confirmed blob holds %d bytes, want the %d staged bytes", len(got), len(payload))
		}
		// The budget is charged at INTENT (stage), never twice — the block
		// stage path's semantics, applied to the blob action.
		if got := blobWriteRows(keyID); got != 1 {
			t.Errorf("stage+confirm booked %d blob-write row(s), want exactly 1 (charged at stage time)", got)
		}

		// Gegenprobe: over the stage cap ⇒ named refusal, never a silent
		// direct write (N-28).
		big := w28Payload(4096)
		text, code = blobStore(keyCtx, mcpCfg(0, 0, 1024), blobStoreInput{
			Category: "reference", Title: "w28-staged-too-big", Filename: "big.bin",
			MimeType: "application/octet-stream",
			File:     base64.StdEncoding.EncodeToString(big),
		})
		if code != "size_cap" {
			t.Errorf("over-cap staging rejection code = %q, want size_cap (answer %q)", code, text)
		}
		if !strings.Contains(text, "pool.blob_stage_max_bytes") {
			t.Errorf("over-cap rejection %q does not name pool.blob_stage_max_bytes — the reason must be actionable", text)
		}
		if got := blobRows("w28-staged-too-big"); got != 0 {
			t.Fatalf("an over-cap payload was written DIRECTLY (%d rows) — that is the silent bypass this cap exists to prevent", got)
		}
		if got := pendingRows(keyID); got != 1 {
			t.Errorf("over-cap payload produced a stage row (%d total, want the 1 from the confirmed write above)", got)
		}
	})

	t.Run("c_WriteBudget", func(t *testing.T) {
		// pool.blob_rate_limit_write = 10: the eleventh call in the window is
		// refused, proving the MCP arm runs the blob metering — not the block
		// budget, and not nothing.
		_, keyCtx, _ := mkKey("w28-budget", "w28b", nil, nil, false)
		cfg := mcpCfg(10, 0, 1<<20)
		for i := 1; i <= 10; i++ {
			text, code := blobStore(keyCtx, cfg, blobStoreInput{
				Category: "reference", Title: fmt.Sprintf("w28-budget-%02d", i), Filename: "b.bin",
				MimeType: "application/octet-stream", Text: "x",
			})
			if code != "" {
				t.Fatalf("blob_store #%d rejected with %q: %s", i, code, text)
			}
		}
		text, code := blobStore(keyCtx, cfg, blobStoreInput{
			Category: "reference", Title: "w28-budget-11", Filename: "b.bin",
			MimeType: "application/octet-stream", Text: "x",
		})
		if code != "rate_limit" {
			t.Fatalf("the 11th blob_store in the window answered code %q (%s), want rate_limit", code, text)
		}
		if !strings.Contains(text, "max 10 blob writes per 60 seconds") {
			t.Errorf("rate-limit prose = %q, want the blob wording of /api/blob/store", text)
		}
		if got := blobRows("w28-budget-11"); got != 0 {
			t.Errorf("the refused call still wrote %d blob(s)", got)
		}

		// Fallback VALUE semantics (config.go BlobWriteLimit): 0 does NOT mean
		// unlimited — it means query.rate_limit_write. The window is filled
		// with booked rows rather than 100 uploads: the gate reads exactly this
		// population (store.ActionBlobWrite rows), so seeding it probes the
		// same counter the handler counts.
		fbKeyID, fbCtx, _ := mkKey("w28-fallback", "w28f", nil, nil, false)
		fbCfg := mcpCfg(0, 100, 1<<20)
		for i := 0; i < 99; i++ {
			if _, err := store.LogAccessRef(ctx, pool, fbKeyID, "", store.ActionBlobWrite); err != nil {
				t.Fatalf("seed blob-write row %d: %v", i, err)
			}
		}
		text, code = blobStore(fbCtx, fbCfg, blobStoreInput{
			Category: "reference", Title: "w28-fallback-100", Filename: "f.bin",
			MimeType: "application/octet-stream", Text: "x",
		})
		if code != "" {
			t.Fatalf("the 100th blob write under a fallback limit of 100 was refused (%q): %s", code, text)
		}
		text, code = blobStore(fbCtx, fbCfg, blobStoreInput{
			Category: "reference", Title: "w28-fallback-101", Filename: "f.bin",
			MimeType: "application/octet-stream", Text: "x",
		})
		if code != "rate_limit" {
			t.Fatalf("the 101st blob write answered %q (%s), want rate_limit — "+
				"pool.blob_rate_limit_write=0 falls back to the VALUE of query.rate_limit_write, not to unlimited", code, text)
		}
		if !strings.Contains(text, "max 100 blob writes per 60 seconds") {
			t.Errorf("fallback rate-limit prose = %q, want the fallback value 100 named", text)
		}
	})

	t.Run("d_RangeRead", func(t *testing.T) {
		_, keyCtx, _ := mkKey("w28-range", "w28r", nil, nil, false)
		cfg := mcpCfg(0, 0, 1<<20)
		payload := w28Payload(512)

		text, code := blobStore(keyCtx, cfg, blobStoreInput{
			Category: "reference", Title: "w28-range", Filename: "range.ndjson",
			MimeType: "application/x-ndjson",
			File:     base64.StdEncoding.EncodeToString(payload),
		})
		if code != "" {
			t.Fatalf("blob_store for the range probe was refused (%q): %s", code, text)
		}
		var stored struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(text), &stored); err != nil || stored.ID == "" {
			t.Fatalf("blob_store answer carries no id: %q (%v)", text, err)
		}

		out, isErr := blobFetch(keyCtx, cfg, blobFetchInput{ID: stored.ID, Offset: 100, Length: 50})
		if isErr {
			t.Fatalf("ranged blob_fetch failed: %s", out)
		}
		var got w28FetchResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode blob_fetch answer %q: %v", out, err)
		}
		if got.Length != 50 {
			t.Errorf("blob_fetch returned length %d, want 50", got.Length)
		}
		if got.Content != string(payload[100:150]) {
			t.Errorf("blob_fetch(offset=100,length=50) = %q, want %q — the range must cut the STORED bytes",
				got.Content, string(payload[100:150]))
		}
		if got.FileSize != int64(len(payload)) {
			t.Errorf("blob_fetch reports file_size %d, want %d — a ranged read must still name the whole size",
				got.FileSize, len(payload))
		}

		// A length over the maximum is REFUSED, not silently clamped: a caller
		// that thinks it read the whole blob would build a wrong index.
		out, isErr = blobFetch(keyCtx, cfg, blobFetchInput{ID: stored.ID, Offset: 0, Length: blobFetchMaxLength + 1})
		if !isErr {
			t.Fatalf("blob_fetch with length over the maximum was answered, want a refusal: %s", out)
		}
		if !strings.Contains(out, fmt.Sprintf("%d", blobFetchMaxLength)) {
			t.Errorf("over-max refusal %q does not name the maximum", out)
		}

		// meta_only stays a metadata read — no payload, ranged or otherwise.
		out, isErr = blobFetch(keyCtx, cfg, blobFetchInput{ID: stored.ID, MetaOnly: true})
		if isErr {
			t.Fatalf("meta_only blob_fetch failed: %s", out)
		}
		got = w28FetchResult{}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("decode meta_only answer %q: %v", out, err)
		}
		if got.Content != "" || got.File != "" {
			t.Errorf("meta_only answer carries payload bytes (content %d, file %d)", len(got.Content), len(got.File))
		}
		if got.FileSize != int64(len(payload)) {
			t.Errorf("meta_only file_size = %d, want %d", got.FileSize, len(payload))
		}
	})
}

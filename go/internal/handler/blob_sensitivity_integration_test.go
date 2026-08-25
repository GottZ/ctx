//go:build integration

// W02-9 gates 1-6: the blob layer gets the net the block path has carried since
// G40 (BP-8) — a sensitivity scan on the ONE blob write core, a credentials
// read gate on blob_fetch, and an untrusted frame around every answer that
// carries payload.
//
// Ist-Stand before this wave: nothing on the blob layer ever looked at a
// payload. context_blobs has no sensitivity column, blob_core.go passed the
// caller's metadata straight into UpsertBlob, and blob_fetch handed back
// whatever bytes the range covered. Every claim below was therefore red.
//
// What the probes pin:
//
//	a StrongKindsRaise  — a structured credential signal (PEM header, JWT, AWS
//	                      key id) raises metadata.sensitivity to credentials and
//	                      names the kind, on BOTH surfaces (one write core)
//	b FetchGate         — the payload of such a blob does not come back over
//	                      blob_fetch; meta_only still does
//	c EntropyNoRaise    — an unlabelled 64-hex run is recorded in
//	                      metadata.entropy_flags and raises NOTHING. This is the
//	                      counter-probe to a naive scanner: on the payload layer
//	                      the build artefacts ARE the evidence (design §4.4)
//	d Unscanned         — non-UTF-8, over pool.blob_scan_max_bytes, and the cap
//	                      at 0 all write metadata.sensitivity='unscanned' and let
//	                      the write through (the 61 live blobs and every binary
//	                      upload must not become collateral)
//	e UntrustedFrame    — every answer WITH content carries untrusted:true plus
//	                      the origin of the bytes; meta_only carries neither
//	f RESTUnchanged     — POST /api/blob/fetch still answers the whole payload of
//	                      a credentials blob, with its pre-existing key set and no
//	                      frame. The operator path is not a model context, and
//	                      this wave does not touch it
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestBlobSensitivity -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w29 fixtures. Each one carries exactly ONE signal class, so a failing
// assertion names the rule that moved rather than "something in the payload".
const (
	// w29PEM/w29JWT are structured signals. The NDJSON shape is the real one:
	// the payloads this surface exists for are tool-result lines.
	w29PEM = `{"tool":"read_file","path":"/etc/ssl/id_ed25519","out":"-----BEGIN PRIVATE KEY-----\nMC4CAQ\n-----END PRIVATE KEY-----"}`
	w29JWT = `{"tool":"terminal","out":"bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"}`

	// w29Hex is the counter-probe: an UNLABELLED 64-hex run. Labelled runs
	// (sha256:…) are already skipped inside sensitivity.Scan, so a fixture with
	// a label would prove nothing about the blob layer's own differentiation.
	// 16 distinct symbols cap the entropy at 4.0 bits/char, below the 4.5 the
	// base64 rule needs — so this fires reHexBlob and nothing else.
	w29Hex = `{"tool":"terminal","out":"pulled 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}`

	// w29Clean carries no signal at all.
	w29Clean = `{"tool":"read_file","path":"README.md","out":"ctx is a context store."}`
)

// w29Fetch is the wire form of a blob_fetch answer — decoded from the tool's
// text content, which is what a client actually parses.
type w29Fetch struct {
	ID        string         `json:"id"`
	Category  string         `json:"category"`
	Title     string         `json:"title"`
	Scope     string         `json:"scope"`
	Metadata  map[string]any `json:"metadata"`
	Encoding  string         `json:"encoding"`
	Content   string         `json:"content"`
	File      string         `json:"file"`
	Untrusted bool           `json:"untrusted"`
	Origin    map[string]any `json:"origin"`
	Note      string         `json:"note"`
}

func TestBlobSensitivity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	mkKey := func(name, home string) (context.Context, *auth.AuthResult) {
		t.Helper()
		_, plain, err := store.CreateApiKey(ctx, pool, name, home, nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", name, err)
		}
		return context.WithValue(ctx, authResultKey, ar), ar
	}

	cfgFor := func(scanMax int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Pool:   config.PoolConfig{BlobScanMaxBytes: scanMax, BlobStageMaxBytes: 1 << 20},
			Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
		}}
	}
	mcpCfg := func(scanMax int) MCPConfig {
		return MCPConfig{Pool: pool, Cfg: cfgFor(scanMax)}
	}

	blobStore := func(keyCtx context.Context, scanMax int, in blobStoreInput) (string, string) {
		t.Helper()
		r, _, err := mcpBlobStoreHandler(mcpCfg(scanMax))(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_store %q: protocol error %v", in.Title, err)
		}
		return resultText(t, r), ecMCPCode(t, r)
	}
	blobFetch := func(keyCtx context.Context, in blobFetchInput) (string, bool) {
		t.Helper()
		r, _, err := mcpBlobFetchHandler(mcpCfg(1 << 24))(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_fetch %q: protocol error %v", in.Title, err)
		}
		return resultText(t, r), r.IsError
	}
	// storeText writes one text payload over MCP and returns the stored
	// metadata as the DATABASE holds it — the answer envelope is not the claim,
	// the row is.
	storeText := func(keyCtx context.Context, scanMax int, title, text string, md map[string]any) map[string]any {
		t.Helper()
		out, code := blobStore(keyCtx, scanMax, blobStoreInput{
			Category: "reference", Title: title, Filename: "e.ndjson",
			MimeType: "application/x-ndjson", Text: text, Metadata: md,
		})
		if code != "" {
			t.Fatalf("blob_store %q refused (%q): %s", title, code, out)
		}
		return storedMeta(t, ctx, pool, title)
	}

	t.Run("a_StrongKindsRaise", func(t *testing.T) {
		keyCtx, ar := mkKey("w29-strong", "w29a")

		// Gate 1, MCP arm: PEM header + JWT in one NDJSON payload.
		md := storeText(keyCtx, 1<<24, "w29-strong-mcp", w29PEM+"\n"+w29JWT, nil)
		if got := md["sensitivity"]; got != "credentials" {
			t.Errorf("metadata.sensitivity = %v, want \"credentials\" — a PEM private-key header reached the blob layer unclassified (BP-8)", got)
		}
		if got, _ := md["sensitivity_kind"].(string); got == "" {
			t.Errorf("metadata.sensitivity_kind is empty, want the rule that fired (metadata: %v)", md)
		} else if got != "pem-private-key" {
			t.Errorf("metadata.sensitivity_kind = %q, want \"pem-private-key\" (Scan's precedence: PEM before JWT)", got)
		}

		// The JWT fires on its own, so the assertion above is not carried by the
		// PEM line alone.
		md = storeText(keyCtx, 1<<24, "w29-strong-jwt", w29JWT, nil)
		if md["sensitivity"] != "credentials" || md["sensitivity_kind"] != "jwt" {
			t.Errorf("JWT-only payload: sensitivity=%v kind=%v, want credentials/jwt", md["sensitivity"], md["sensitivity_kind"])
		}

		// Gate 1, REST arm: the same payload over POST /api/blob/store must
		// answer the same classification — one write core, not two.
		status, resp := postBlobStore(t, NewBlobHandler(pool, cfgFor(1<<24)), ar, map[string]any{
			"file":      base64.StdEncoding.EncodeToString([]byte(w29PEM)),
			"filename":  "e.ndjson",
			"category":  "reference",
			"title":     "w29-strong-rest",
			"mime_type": "application/x-ndjson",
		})
		if status != http.StatusOK {
			t.Fatalf("REST blob-store status = %d, want 200 (body %v)", status, resp)
		}
		md = storedMeta(t, ctx, pool, "w29-strong-rest")
		if md["sensitivity"] != "credentials" || md["sensitivity_kind"] != "pem-private-key" {
			t.Errorf("REST arm: sensitivity=%v kind=%v, want credentials/pem-private-key — the scan sits in the shared core, not on one transport",
				md["sensitivity"], md["sensitivity_kind"])
		}

		// A clean payload gets no classification invented for it.
		md = storeText(keyCtx, 1<<24, "w29-clean", w29Clean, nil)
		if _, ok := md["sensitivity"]; ok {
			t.Errorf("clean payload got metadata.sensitivity = %v, want the field absent", md["sensitivity"])
		}

		// Caller-set sensitivity is overruled UPWARDS only: a caller that calls
		// a PEM key "public" does not get to.
		md = storeText(keyCtx, 1<<24, "w29-downgrade", w29PEM, map[string]any{"sensitivity": "public"})
		if md["sensitivity"] != "credentials" {
			t.Errorf("caller-declared public over a PEM payload: sensitivity = %v, want credentials (upgrade-only)", md["sensitivity"])
		}
		// …and a caller that rates a clean payload higher keeps that rating.
		md = storeText(keyCtx, 1<<24, "w29-caller-high", w29Clean, map[string]any{"sensitivity": "personal"})
		if md["sensitivity"] != "personal" {
			t.Errorf("caller-declared personal over a clean payload: sensitivity = %v, want personal (the detector only ever raises)", md["sensitivity"])
		}
	})

	t.Run("b_FetchGate", func(t *testing.T) {
		keyCtx, _ := mkKey("w29-gate", "w29b")
		storeText(keyCtx, 1<<24, "w29-gate-cred", w29PEM, nil)
		id := blobIDOf(t, ctx, pool, "w29-gate-cred")

		// Gate 2: the payload does not come back.
		text, isErr := blobFetch(keyCtx, blobFetchInput{ID: id})
		if !isErr {
			t.Fatalf("blob_fetch on a credentials blob succeeded, want a refusal — answer: %s", truncW29(text))
		}
		if !strings.Contains(text, "credentials") {
			t.Errorf("refusal does not name the reason: %q", text)
		}
		if strings.Contains(text, "BEGIN PRIVATE KEY") {
			t.Errorf("refusal echoed the payload it refused: %q", truncW29(text))
		}

		// meta_only stays open — an operator has to be able to see that the
		// blob exists and why it is gated.
		text, isErr = blobFetch(keyCtx, blobFetchInput{ID: id, MetaOnly: true})
		if isErr {
			t.Fatalf("meta_only on a credentials blob was refused, want the metadata: %s", text)
		}
		var got w29Fetch
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("decode meta_only answer: %v (%s)", err, truncW29(text))
		}
		if got.Metadata["sensitivity"] != "credentials" {
			t.Errorf("meta_only metadata.sensitivity = %v, want credentials", got.Metadata["sensitivity"])
		}
		if got.Content != "" || got.File != "" {
			t.Errorf("meta_only carried payload: content=%d bytes file=%d bytes", len(got.Content), len(got.File))
		}

		// The category+title selector is gated too — one gate, not one per
		// selector.
		text, isErr = blobFetch(keyCtx, blobFetchInput{Category: "reference", Title: "w29-gate-cred"})
		if !isErr {
			t.Errorf("category+title fetch of a credentials blob succeeded, want the same refusal: %s", truncW29(text))
		}
	})

	t.Run("c_EntropyNoRaise", func(t *testing.T) {
		keyCtx, _ := mkKey("w29-entropy", "w29c")

		// Gate 3. An unlabelled 64-hex run is exactly what a naive port of the
		// block-path detector would raise to credentials — and it is the line a
		// drill-down is made for (design §4.4).
		md := storeText(keyCtx, 1<<24, "w29-hex", w29Hex, nil)
		if got, ok := md["sensitivity"]; ok {
			t.Errorf("a 64-hex digest raised metadata.sensitivity to %v — the payload layer must not classify build artefacts as credentials (§4.4)", got)
		}
		if _, ok := md["sensitivity_kind"]; ok {
			t.Errorf("entropy hit set metadata.sensitivity_kind = %v, want the field absent (no raise, no kind)", md["sensitivity_kind"])
		}
		flags, ok := md["entropy_flags"].([]any)
		if !ok || len(flags) == 0 {
			t.Fatalf("metadata.entropy_flags = %v, want [\"hex-blob\"] — the observation is recorded, it just does not gate", md["entropy_flags"])
		}
		if flags[0] != "hex-blob" {
			t.Errorf("metadata.entropy_flags = %v, want [\"hex-blob\"]", flags)
		}

		// And the payload is readable — which is the whole point of not raising.
		text, isErr := blobFetch(keyCtx, blobFetchInput{ID: blobIDOf(t, ctx, pool, "w29-hex")})
		if isErr {
			t.Fatalf("blob_fetch on an entropy-flagged blob was refused: %s", text)
		}
		if !strings.Contains(text, "0123456789abcdef") {
			t.Errorf("entropy-flagged payload did not come back intact: %s", truncW29(text))
		}

		// A structured signal in the SAME payload still wins: the entropy rule
		// is a floor of its own, never a shield.
		md = storeText(keyCtx, 1<<24, "w29-hex-and-pem", w29Hex+"\n"+w29PEM, nil)
		if md["sensitivity"] != "credentials" {
			t.Errorf("hex + PEM: sensitivity = %v, want credentials", md["sensitivity"])
		}
	})

	t.Run("d_Unscanned", func(t *testing.T) {
		keyCtx, _ := mkKey("w29-unscanned", "w29d")

		// Gate 4a: non-UTF-8. A PNG/PDF upload and the 61 live blobs must pass,
		// visibly unscanned rather than silently refused or silently clean.
		binary := []byte{0xff, 0xfe, 0x00, 0x01, 0x80, 0x90, 0xc0, 0xff}
		out, code := blobStore(keyCtx, 1<<24, blobStoreInput{
			Category: "reference", Title: "w29-binary", Filename: "e.bin",
			MimeType: "application/octet-stream",
			File:     base64.StdEncoding.EncodeToString(binary),
		})
		if code != "" {
			t.Fatalf("binary blob_store refused (%q): %s — an unscannable payload must still be storable (§3.5)", code, out)
		}
		md := storedMeta(t, ctx, pool, "w29-binary")
		if md["sensitivity"] != "unscanned" {
			t.Errorf("non-UTF-8 payload: metadata.sensitivity = %v, want \"unscanned\"", md["sensitivity"])
		}

		// Gate 4b: over the cap. Same fixture as gate 1, cap set below it — the
		// classification must flip to unscanned, not stay credentials, because
		// the scan genuinely did not run.
		md = storeText(keyCtx, 16, "w29-oversize", w29PEM, nil)
		if md["sensitivity"] != "unscanned" {
			t.Errorf("payload over pool.blob_scan_max_bytes: metadata.sensitivity = %v, want \"unscanned\"", md["sensitivity"])
		}

		// Gate 4c: the cap at 0 turns the scan off entirely.
		md = storeText(keyCtx, 0, "w29-scan-off", w29PEM, nil)
		if md["sensitivity"] != "unscanned" {
			t.Errorf("pool.blob_scan_max_bytes=0: metadata.sensitivity = %v, want \"unscanned\" (scan off)", md["sensitivity"])
		}

		// An unscanned blob READS — absence of a verdict is not a verdict, and
		// this is the behaviour the 61 live blobs (which carry no field at all)
		// depend on.
		if _, isErr := blobFetch(keyCtx, blobFetchInput{ID: blobIDOf(t, ctx, pool, "w29-oversize")}); isErr {
			t.Errorf("blob_fetch refused an unscanned blob — the read gate must trip on credentials alone (§3.5)")
		}
	})

	t.Run("e_UntrustedFrame", func(t *testing.T) {
		keyCtx, _ := mkKey("w29-frame", "w29e")
		storeText(keyCtx, 1<<24, "w29-frame-blob", w29Clean, nil)
		id := blobIDOf(t, ctx, pool, "w29-frame-blob")

		// Gate 5: an answer WITH content is framed.
		text, isErr := blobFetch(keyCtx, blobFetchInput{ID: id})
		if isErr {
			t.Fatalf("blob_fetch refused a clean blob: %s", text)
		}
		var got w29Fetch
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("decode answer: %v (%s)", err, truncW29(text))
		}
		if !got.Untrusted {
			t.Errorf("answer carrying payload has no untrusted:true — a drill-down must never hand a model bare foreign text (BP-2)")
		}
		if got.Note == "" {
			t.Errorf("answer carrying payload has no note naming what the frame means")
		}
		for _, f := range []struct{ key, want string }{
			{"blob_id", id}, {"category", "reference"}, {"title", "w29-frame-blob"}, {"scope", "w29e"},
		} {
			if got.Origin[f.key] != f.want {
				t.Errorf("origin.%s = %v, want %q — the frame has to name where the bytes came from", f.key, got.Origin[f.key], f.want)
			}
		}

		// A base64 window is content too.
		bin := []byte{0xff, 0xfe, 0x00, 0x01}
		if _, code := blobStore(keyCtx, 1<<24, blobStoreInput{
			Category: "reference", Title: "w29-frame-bin", Filename: "e.bin",
			MimeType: "application/octet-stream", File: base64.StdEncoding.EncodeToString(bin),
		}); code != "" {
			t.Fatalf("store binary fixture refused (%q)", code)
		}
		text, _ = blobFetch(keyCtx, blobFetchInput{ID: blobIDOf(t, ctx, pool, "w29-frame-bin")})
		got = w29Fetch{}
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("decode binary answer: %v", err)
		}
		if got.Encoding != "base64" || got.File == "" {
			t.Fatalf("binary answer shape: encoding=%q file=%d bytes", got.Encoding, len(got.File))
		}
		if !got.Untrusted || got.Origin["blob_id"] == nil {
			t.Errorf("base64 answer is unframed (untrusted=%v origin=%v) — the frame follows the CONTENT, not the encoding", got.Untrusted, got.Origin)
		}

		// meta_only carries no frame: there is no foreign text in it, and a
		// frame around metadata would train a reader to ignore the real one.
		text, _ = blobFetch(keyCtx, blobFetchInput{ID: id, MetaOnly: true})
		var raw map[string]any
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			t.Fatalf("decode meta_only answer: %v", err)
		}
		for _, k := range []string{"untrusted", "origin", "note"} {
			if _, ok := raw[k]; ok {
				t.Errorf("meta_only answer carries %q = %v, want the key absent", k, raw[k])
			}
		}
	})

	t.Run("f_RESTUnchanged", func(t *testing.T) {
		// Gate 6, the regression anchor. The REST fetch route is the operator
		// path; it is not a model context, and this wave does not change it.
		// The probe fixes a credentials blob — the one case where the MCP tool
		// now refuses — and pins that REST still answers the WHOLE payload with
		// its pre-existing key set.
		keyCtx, ar := mkKey("w29-rest", "w29f")
		storeText(keyCtx, 1<<24, "w29-rest-cred", w29PEM, nil)

		status, resp := postBlobFetchW29(t, NewBlobHandler(pool, cfgFor(1<<24)), ar, map[string]any{
			"category": "reference", "title": "w29-rest-cred",
		})
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("REST blob-fetch of a credentials blob: status=%d body=%v, want the unchanged 200 answer", status, resp)
		}
		fileB64, _ := resp["file"].(string)
		payload, err := base64.StdEncoding.DecodeString(fileB64)
		if err != nil || string(payload) != w29PEM {
			t.Errorf("REST blob-fetch did not return the whole stored payload (err=%v, %d bytes) — the operator path is a regression anchor in this wave", err, len(payload))
		}
		for _, k := range []string{"untrusted", "origin", "note"} {
			if _, ok := resp[k]; ok {
				t.Errorf("REST answer grew a %q key — the framing is the MCP surface's, not this route's", k)
			}
		}
		// The exact envelope, pinned: a key added or dropped here is a wire
		// change on an operator surface and has to be a deliberate one.
		want := map[string]bool{
			"success": true, "id": true, "category": true, "title": true, "filename": true,
			"mime_type": true, "file_size": true, "checksum": true, "storage_type": true,
			"tags": true, "metadata": true, "scope": true, "created_at": true,
			"updated_at": true, "file": true,
		}
		for k := range resp {
			if !want[k] {
				t.Errorf("REST blob-fetch answer carries unexpected key %q", k)
			}
			delete(want, k)
		}
		for k := range want {
			if k != "checksum" { // omitted when the stored row carries none
				t.Errorf("REST blob-fetch answer lost key %q", k)
			}
		}
	})
}

// storedMeta reads a blob's metadata straight out of the row. The answer
// envelope is a report; the column is the fact.
func storedMeta(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title string) map[string]any {
	t.Helper()
	var md map[string]any
	if err := pool.QueryRow(ctx,
		`SELECT metadata FROM context_blobs WHERE title = $1`, title).Scan(&md); err != nil {
		t.Fatalf("read metadata of %q: %v", title, err)
	}
	return md
}

// blobIDOf resolves a fixture's id.
func blobIDOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM context_blobs WHERE title = $1`, title).Scan(&id); err != nil {
		t.Fatalf("read id of %q: %v", title, err)
	}
	return id
}

// postBlobFetchW29 drives POST /api/blob/fetch the way a client does.
func postBlobFetchW29(t *testing.T, h *BlobHandler, ar *auth.AuthResult, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/blob/fetch", bytes.NewReader(raw))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleBlobFetch(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, resp
}

func truncW29(s string) string {
	if len(s) <= 400 {
		return s
	}
	return fmt.Sprintf("%s… (%d bytes)", s[:400], len(s))
}

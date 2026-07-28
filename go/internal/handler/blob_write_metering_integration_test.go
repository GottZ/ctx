//go:build integration

// Wave B2 (Gap-C0-b): /api/blob/store must audit and meter its OWN writes.
//
// Ist-Stand before B2 — two defects on one code path:
//
//  1. blob.go:172 handed the BLOB id to store.LogAccess, which books it in
//     context_access_log.block_id — a column FK-bound to context_blocks
//     (001_initial.sql:114). Every successful blob write therefore raised
//     23503 inside the fire-and-forget goroutine, where the error was logged
//     and dropped: not ONE audit row exists for the entire blob surface. B1
//     (1aee20b) made this permanent by repairing UpsertBlob — before it, the
//     write failed earlier and the goroutine never ran.
//  2. the gate in front of it counted action='write' against
//     query.rate_limit_write, the BLOCK budget. Blob writes paid nothing into
//     it (defect 1) yet were refused as soon as block writes had exhausted it:
//     a budget that can never bite from the inside and always bites from the
//     outside.
//
// B2/E1 option A resolves both with a SEPARATE budget: action 'blob-write'
// (store.ActionBlobWrite) counted against pool.blob_rate_limit_write, and the
// blob id booked in a NEW context_access_log.blob_id column (migration 122),
// deliberately WITHOUT a foreign key so audit rows outlive their blob.
//
// The probes carry their brief letters:
//
//	a  AuditRowPerWrite       — exactly one blob-write row, blob_id set, block_id NULL
//	b  IntentSurvivesFailure  — a failing upsert still costs budget
//	c  BudgetRefusesOverLimit — N+1 over the limit ⇒ 429 (fallback budget)
//	d  BudgetIsDecoupled      — block limit does not gate blobs, and vice versa
//	e  MigrationIsPureDDL     — 122 touches no rows and adds no FK
//	f  AuditOutlivesBlob      — DELETE of the blob leaves the row and scans nothing
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestBlobWriteMetering -count=1 -v
package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// b2MigrationFile is the wave's migration, read from the embedded FS by probe e.
const b2MigrationFile = "122_blob_write_audit.sql"

// b2BlobWriteAction is the audit/metering action of a blob write, spelled out
// here on purpose: the test must not read it from the same constant the
// production code uses, or a rename would move both and pin nothing.
const b2BlobWriteAction = "blob-write"

func TestBlobWriteMetering(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// b2Key mints a REAL context_api_keys row. context_access_log.api_key_id is
	// FK-bound, and B2 books the metering row SYNCHRONOUSLY, so a synthetic key
	// id would turn every blob write into a 500 — in production ctx_auth only
	// ever reports is_valid=true for a key row that exists (095), so the
	// fixture matches the only shape the handler can see.
	b2Key := func(t *testing.T, label string) (string, *auth.AuthResult) {
		t.Helper()
		row, _, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create api key %q: %v", label, err)
		}
		return row.ID, &auth.AuthResult{
			ApiKeyID:   row.ID,
			IsValid:    true,
			HomeScope:  "private",
			ReadScopes: []string{"private"},
		}
	}

	// b2Cfg wires the handler with a block-write budget only — the blob budget
	// then resolves through the fallback (pool.blob_rate_limit_write unset ⇒
	// query.rate_limit_write), which is the semantics probe c pins.
	b2Cfg := func(blockLimit int) ConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query: config.QueryConfig{RateLimitWrite: blockLimit},
		}}
	}
	// b2CfgBlob sets BOTH keys, so the dedicated one must win over the fallback.
	b2CfgBlob := func(blobLimit, blockLimit int) ConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query: config.QueryConfig{RateLimitWrite: blockLimit},
			Pool:  config.PoolConfig{BlobRateLimitWrite: blobLimit},
		}}
	}

	// countAction returns the metering population of one key and action — the
	// exact rows store.CheckRateLimitByAction aggregates over.
	countAction := func(t *testing.T, keyID, action string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = $2`, keyID, action).Scan(&n); err != nil {
			t.Fatalf("count %q rows for key %s: %v", action, keyID, err)
		}
		return n
	}

	// waitAttributed polls until the key has `want` blob-write rows whose
	// blob_id is filled in (the handler attributes it in a background
	// goroutine) and returns what it saw.
	waitAttributed := func(t *testing.T, keyID string, want int) int {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*)::int FROM context_access_log
				 WHERE api_key_id = $1::uuid AND action = $2 AND blob_id IS NOT NULL`,
				keyID, b2BlobWriteAction).Scan(&n); err != nil {
				t.Fatalf("count attributed blob-write rows: %v", err)
			}
			if n >= want || time.Now().After(deadline) {
				return n
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	t.Run("a_AuditRowPerWrite", func(t *testing.T) {
		keyID, ar := b2Key(t, "b2-audit")
		h := NewBlobHandler(pool, b2Cfg(0))

		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b2-audit-one", "audit-one.bin", "application/octet-stream", []byte("b2-a")))
		if code != http.StatusOK {
			t.Fatalf("blob-store status = %d, want 200 (body %v)", code, resp)
		}
		blob, _ := resp["blob"].(map[string]any)
		blobID, _ := blob["id"].(string)
		if blobID == "" {
			t.Fatalf("response carries no blob id: %v", resp)
		}

		// RED pre-B2: 0 — the goroutine's INSERT died on the block_id FK.
		if got := countAction(t, keyID, b2BlobWriteAction); got != 1 {
			t.Fatalf("blob write booked %d %q rows, want exactly 1 (pre-B2 the FK violation was swallowed)",
				got, b2BlobWriteAction)
		}
		if got := waitAttributed(t, keyID, 1); got != 1 {
			t.Fatalf("%d blob-write rows carry a blob_id, want 1", got)
		}

		var gotBlob string
		var gotBlock *string
		if err := pool.QueryRow(ctx,
			`SELECT blob_id::text, block_id::text FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = $2`, keyID, b2BlobWriteAction).Scan(&gotBlob, &gotBlock); err != nil {
			t.Fatalf("read the blob-write row: %v", err)
		}
		if gotBlob != blobID {
			t.Errorf("audit row blob_id = %s, want the stored blob %s", gotBlob, blobID)
		}
		if gotBlock != nil {
			t.Errorf("audit row block_id = %v, want NULL (a blob is not a block)", *gotBlock)
		}
	})

	t.Run("b_IntentSurvivesFailure", func(t *testing.T) {
		keyID, ar := b2Key(t, "b2-intent")
		h := NewBlobHandler(pool, b2Cfg(0))

		// Same constraint fixture as blob_store_constraint_integration_test.go:
		// a second filename twin under a different (category,title) misses the
		// ON CONFLICT target and trips this index instead.
		mustExec(t, pool, `CREATE UNIQUE INDEX uq_blob_b2_filename ON context_blobs (filename)`)
		t.Cleanup(func() { mustExec(t, pool, `DROP INDEX uq_blob_b2_filename`) })

		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b2-intent-a", "intent.bin", "application/octet-stream", []byte("first")))
		if code != http.StatusOK {
			t.Fatalf("seed status = %d, want 200 (body %v)", code, resp)
		}
		if got := waitAttributed(t, keyID, 1); got != 1 {
			t.Fatalf("seed booked %d attributed rows, want 1", got)
		}

		code, resp = postBlobStore(t, h, ar,
			blobPayload("reference", "b2-intent-b", "intent.bin", "application/octet-stream", []byte("second")))
		if code != http.StatusConflict {
			t.Fatalf("duplicate status = %d, want 409 (body %v)", code, resp)
		}

		// The whole point of an INTENT booking: the failed upsert did the
		// expensive work (decode, checksum, INSERT attempt) and must be paid
		// for. A variant that books AFTER the upsert leaves 1 row here.
		if got := countAction(t, keyID, b2BlobWriteAction); got != 2 {
			t.Fatalf("failed upsert left %d %q rows in total, want 2 — the intent must be booked BEFORE the upsert",
				got, b2BlobWriteAction)
		}
		var unattributed int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = $2 AND blob_id IS NULL`,
			keyID, b2BlobWriteAction).Scan(&unattributed); err != nil {
			t.Fatalf("count unattributed rows: %v", err)
		}
		if unattributed != 1 {
			t.Errorf("%d blob-write rows without a blob_id, want exactly 1 (the failed write)", unattributed)
		}
	})

	t.Run("c_BudgetRefusesOverLimit", func(t *testing.T) {
		_, ar := b2Key(t, "b2-limit")
		// No pool.blob_rate_limit_write ⇒ the blob budget falls back to the
		// block limit's VALUE (2), while still counting its OWN action.
		h := NewBlobHandler(pool, b2Cfg(2))

		for i := 1; i <= 2; i++ {
			code, resp := postBlobStore(t, h, ar,
				blobPayload("reference", fmt.Sprintf("b2-limit-%d", i), fmt.Sprintf("limit-%d.bin", i),
					"application/octet-stream", []byte("payload")))
			if code != http.StatusOK {
				t.Fatalf("blob-store %d status = %d, want 200 (body %v)", i, code, resp)
			}
		}

		// RED pre-B2: the gate counts action='write', which blob writes never
		// produce ⇒ writeCount stays 0 ⇒ 200 forever.
		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b2-limit-3", "limit-3.bin", "application/octet-stream", []byte("payload")))
		if code != http.StatusTooManyRequests {
			t.Fatalf("blob-store 3 of 2 status = %d, want 429 (body %v)", code, resp)
		}
	})

	t.Run("c2_ExplicitKeyWinsOverFallback", func(t *testing.T) {
		_, ar := b2Key(t, "b2-explicit")
		// The dedicated key says 2, the fallback source says 999. Only a
		// resolver that prefers pool.blob_rate_limit_write refuses the third
		// write — one that reads the fallback first lets all three through.
		h := NewBlobHandler(pool, b2CfgBlob(2, 999))

		for i := 1; i <= 2; i++ {
			code, resp := postBlobStore(t, h, ar,
				blobPayload("reference", fmt.Sprintf("b2-explicit-%d", i), fmt.Sprintf("explicit-%d.bin", i),
					"application/octet-stream", []byte("payload")))
			if code != http.StatusOK {
				t.Fatalf("blob-store %d status = %d, want 200 (body %v)", i, code, resp)
			}
		}
		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b2-explicit-3", "explicit-3.bin", "application/octet-stream", []byte("payload")))
		if code != http.StatusTooManyRequests {
			t.Fatalf("blob-store 3 of 2 status = %d, want 429 (body %v) — "+
				"pool.blob_rate_limit_write must beat the query.rate_limit_write fallback", code, resp)
		}
	})

	t.Run("d_BudgetIsDecoupled", func(t *testing.T) {
		// Direction 1: a key AT its block-write limit may still write blobs.
		exhausted, exhaustedAR := b2Key(t, "b2-decoupled-block")
		h := NewBlobHandler(pool, b2Cfg(2))
		for i := 0; i < 2; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_access_log (api_key_id, action, metadata)
				 VALUES ($1::uuid, 'write', '{}'::jsonb)`, exhausted); err != nil {
				t.Fatalf("seed block-write row: %v", err)
			}
		}
		if got, err := store.CheckRateLimit(ctx, pool, exhausted); err != nil || got != 2 {
			t.Fatalf("block budget precondition: count = %d, err = %v, want 2/nil", got, err)
		}
		code, resp := postBlobStore(t, h, exhaustedAR,
			blobPayload("reference", "b2-decoupled-a", "decoupled-a.bin", "application/octet-stream", []byte("x")))
		if code != http.StatusOK {
			t.Fatalf("blob-store by a block-limited key: status = %d, want 200 (body %v) — "+
				"E1/A decouples the two budgets", code, resp)
		}

		// Direction 2: blob writes never feed the BLOCK budget. Asserted on
		// store.CheckRateLimit itself — that count IS the value every block
		// write gate compares against its limit, so pinning it here pins the
		// gate without dragging the whole /api/store stack into this test.
		fresh, freshAR := b2Key(t, "b2-decoupled-blob")
		for i := 1; i <= 2; i++ {
			code, resp := postBlobStore(t, h, freshAR,
				blobPayload("reference", fmt.Sprintf("b2-decoupled-b%d", i), fmt.Sprintf("decoupled-b%d.bin", i),
					"application/octet-stream", []byte("x")))
			if code != http.StatusOK {
				t.Fatalf("blob-store %d status = %d, want 200 (body %v)", i, code, resp)
			}
		}
		got, err := store.CheckRateLimit(ctx, pool, fresh)
		if err != nil {
			t.Fatalf("block budget after blob writes: %v", err)
		}
		if got != 0 {
			t.Fatalf("2 blob writes moved the BLOCK budget to %d, want 0 — blob writes must not consume it", got)
		}
	})

	t.Run("e_MigrationIsPureDDL", func(t *testing.T) {
		raw, err := migrations.FS.ReadFile(b2MigrationFile)
		if err != nil {
			t.Fatalf("read embedded %s: %v", b2MigrationFile, err)
		}
		sql := b2StripComments(string(raw))

		for _, forbidden := range []string{
			"INSERT INTO CONTEXT_ACCESS_LOG",
			"UPDATE CONTEXT_ACCESS_LOG",
			"DELETE FROM CONTEXT_ACCESS_LOG",
			"FOREIGN KEY",
			"CHECK (",
		} {
			if strings.Contains(sql, forbidden) {
				t.Errorf("migration %s contains %q — B2 is pure additive DDL: no DML on the audit trail, "+
					"no FK (audit rows must outlive their blob), no CHECK", b2MigrationFile, forbidden)
			}
		}
		if !strings.Contains(sql, "ADD COLUMN") || !strings.Contains(sql, "BLOB_ID") {
			t.Errorf("migration %s does not add a blob_id column: %s", b2MigrationFile, sql)
		}
	})

	t.Run("f_AuditOutlivesBlob", func(t *testing.T) {
		keyID, ar := b2Key(t, "b2-outlive")
		h := NewBlobHandler(pool, b2Cfg(0))

		code, resp := postBlobStore(t, h, ar,
			blobPayload("reference", "b2-outlive", "outlive.bin", "application/octet-stream", []byte("keep me")))
		if code != http.StatusOK {
			t.Fatalf("blob-store status = %d, want 200 (body %v)", code, resp)
		}
		blob, _ := resp["blob"].(map[string]any)
		blobID, _ := blob["id"].(string)
		if got := waitAttributed(t, keyID, 1); got != 1 {
			t.Fatalf("%d attributed rows, want 1", got)
		}

		// Structural oracle: no FK from the audit trail to the blob table.
		var fks int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM pg_constraint
			 WHERE conrelid = 'context_access_log'::regclass
			   AND contype = 'f'
			   AND confrelid = 'context_blobs'::regclass`).Scan(&fks); err != nil {
			t.Fatalf("inspect pg_constraint: %v", err)
		}
		if fks != 0 {
			t.Errorf("%d FK(s) from context_access_log to context_blobs, want 0", fks)
		}

		// Behavioural oracle: the DELETE fires no trigger at all.
		if lines := b2ExplainDelete(ctx, t, pool, blobID); b2HasTriggerLine(lines) {
			t.Errorf("DELETE of a blob fires a trigger scan:\n%s", strings.Join(lines, "\n"))
		}

		var stillThere string
		var block *string
		if err := pool.QueryRow(ctx,
			`SELECT blob_id::text, block_id::text FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = $2`, keyID, b2BlobWriteAction).Scan(&stillThere, &block); err != nil {
			t.Fatalf("read the audit row after the blob was deleted: %v", err)
		}
		if stillThere != blobID {
			t.Errorf("audit row blob_id = %s after the blob was deleted, want the unchanged %s", stillThere, blobID)
		}
		if block != nil {
			t.Errorf("audit row block_id = %v, want NULL", *block)
		}

		// Anti-fixture: the same oracles against a blob_id that DOES carry a
		// foreign key. Without it the probe above could be passing on an oracle
		// that cannot discriminate — here the FK variant must visibly break.
		t.Run("fk_variant_is_red", func(t *testing.T) {
			fkKey, fkAR := b2Key(t, "b2-outlive-fk")
			code, resp := postBlobStore(t, h, fkAR,
				blobPayload("reference", "b2-outlive-fk", "outlive-fk.bin", "application/octet-stream", []byte("doomed")))
			if code != http.StatusOK {
				t.Fatalf("blob-store status = %d, want 200 (body %v)", code, resp)
			}
			fkBlob, _ := resp["blob"].(map[string]any)
			fkBlobID, _ := fkBlob["id"].(string)
			if got := waitAttributed(t, fkKey, 1); got != 1 {
				t.Fatalf("%d attributed rows, want 1", got)
			}

			// Oracle 1, the sharpest one: the audit trail already holds the
			// dangling reference the parent probe left behind, so a VALIDATING
			// foreign key cannot even be created over this corpus. In a schema
			// migration that is a boot abort — the constraint is not merely
			// unwanted here, it is unreachable the moment one blob has been
			// deleted, which on a live system is immediately.
			_, addErr := pool.Exec(ctx, `ALTER TABLE context_access_log ADD CONSTRAINT fk_b2_probe_blob
				FOREIGN KEY (blob_id) REFERENCES context_blobs(id) ON DELETE SET NULL`)
			if addErr == nil {
				mustExec(t, pool, `ALTER TABLE context_access_log DROP CONSTRAINT fk_b2_probe_blob`)
				t.Fatalf("a validating FK over an audit trail with a dangling blob_id was accepted — " +
					"the parent probe's surviving reference did not survive after all")
			}
			var pgErr *pgconn.PgError
			if !errors.As(addErr, &pgErr) || pgErr.Code != "23503" {
				t.Fatalf("adding the FK failed with %v, want 23503 on the dangling reference", addErr)
			}

			// Oracle 2: the cost on the DELETE path, isolated from oracle 1.
			// NOT VALID skips the corpus scan but installs the RI triggers in
			// full, so this measures exactly what an FK would charge every
			// context_blobs DELETE forever.
			mustExec(t, pool, `ALTER TABLE context_access_log ADD CONSTRAINT fk_b2_probe_blob
				FOREIGN KEY (blob_id) REFERENCES context_blobs(id) ON DELETE SET NULL NOT VALID`)
			t.Cleanup(func() {
				mustExec(t, pool, `ALTER TABLE context_access_log DROP CONSTRAINT fk_b2_probe_blob`)
			})

			lines := b2ExplainDelete(ctx, t, pool, fkBlobID)
			if !b2HasTriggerLine(lines) {
				t.Fatalf("the FK variant fired no trigger — the oracle in the parent probe cannot "+
					"discriminate and proves nothing:\n%s", strings.Join(lines, "\n"))
			}
			var orphaned *string
			if err := pool.QueryRow(ctx,
				`SELECT blob_id::text FROM context_access_log
				 WHERE api_key_id = $1::uuid AND action = $2`, fkKey, b2BlobWriteAction).Scan(&orphaned); err != nil {
				t.Fatalf("read the audit row: %v", err)
			}
			if orphaned != nil {
				t.Fatalf("the FK variant left blob_id = %s — the second oracle cannot discriminate either", *orphaned)
			}
		})
	})
}

// b2ExplainDelete runs EXPLAIN ANALYZE over the DELETE of one blob (which
// executes it) and returns the plan lines. EXPLAIN reports every trigger that
// fired during the statement, so an RI trigger from a foreign key is visible
// as a "Trigger for constraint …" line — the discriminator probe f needs.
func b2ExplainDelete(ctx context.Context, t *testing.T, pool *pgxpool.Pool, blobID string) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `EXPLAIN (ANALYZE, TIMING OFF) DELETE FROM context_blobs WHERE id = $1::uuid`, blobID)
	if err != nil {
		t.Fatalf("explain delete: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	return lines
}

func b2HasTriggerLine(lines []string) bool {
	for _, l := range lines {
		if strings.Contains(l, "Trigger") {
			return true
		}
	}
	return false
}

// b2StripComments removes -- line comments and upper-cases what is left, so
// probe e reads the migration's STATEMENTS and not the prose that explains why
// the forbidden constructs are absent.
func b2StripComments(sql string) string {
	var b strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(strings.ToUpper(line))
		b.WriteString("\n")
	}
	return b.String()
}

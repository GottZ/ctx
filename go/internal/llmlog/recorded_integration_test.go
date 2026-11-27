//go:build integration

// The recorded row (issue #571) over real inserts:
//
//   - TestRecordedRowLandsWhole: a recorded row with a credentials-class
//     sensitivity, a request and an answer past 64 KiB lands with both
//     bodies whole and metadata.gw.recorded, and gives its texts back to the
//     recording account once written;
//   - TestRecordedRowInsertCost (opt-in, CTX_RECORD_PROBE=1): the insert time
//     and the stored size of recorded rows up to the largest non-streamed row
//     at the default caps (a request text of 2 × gateway.max_request_bytes =
//     16 MiB and a 24 MiB answer), against insertTimeout (5 s).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestRecordedRow' -v ./internal/llmlog/
//	CTX_RECORD_PROBE=1 go test -tags=integration -p 1 -count=1 -run TestRecordedRowInsertCost -v ./internal/llmlog/
package llmlog_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

func recordedGwiRow(request, response string) llmlog.GatewayRow {
	r := gwiRow(llmlog.PipelineGatewayChat)
	r.Telemetry.RequiredSensitivity = "credentials"
	r.Request, r.Response, r.Recorded = request, response, true
	return r
}

func TestRecordedRowLandsWhole(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	req := `[{"role":"user","content":"password=s3cr3t-571 ` + strings.Repeat("r", 100<<10) + `"}]`
	resp := `{"choices":[{"message":{"content":"` + strings.Repeat("a", 90<<10) + `"}}]}`
	held := int64(len(req) + len(resp))
	before := llmlog.RecordedHeld()
	if !llmlog.ReserveRecorded(held) {
		t.Fatal("reserve")
	}
	row := recordedGwiRow(req, resp)
	row.RecordedHeld = held
	llmlog.RecordGateway(pool, row)
	if err := llmlog.Settle(ctx, pool); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if got := llmlog.RecordedHeld(); got != before {
		t.Errorf("the recording account holds %d bytes after the insert, want %d", got, before)
	}
	var user, answer, sens, recorded string
	if err := pool.QueryRow(ctx, `SELECT request_user, response_content, required_sensitivity, metadata->'gw'->>'recorded'
		FROM context_llm_log WHERE consumer_id = $1::uuid`, gwiConsumer).Scan(&user, &answer, &sens, &recorded); err != nil {
		t.Fatalf("read: %v", err)
	}
	if user != req || answer != resp || sens != "credentials" || recorded != "true" {
		t.Errorf("stored request %d/%d bytes, answer %d/%d, sensitivity %s, recorded %s — want both whole, credentials, true",
			len(user), len(req), len(answer), len(resp), sens, recorded)
	}
}

func TestRecordedRowInsertCost(t *testing.T) {
	if os.Getenv("CTX_RECORD_PROBE") == "" {
		t.Skip("opt-in probe: CTX_RECORD_PROBE=1")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	// Text that compresses like a prompt (repeated words) and text that does
	// not (pseudo-random letters) — TOAST compresses the first.
	words := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString("the quick brown fox jumps over the lazy dog ")
		}
		return b.String()[:n]
	}
	noise := func(n int, seed uint64) string {
		b := make([]byte, n)
		x := seed
		for i := range b {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			b[i] = 'a' + byte(x%26)
		}
		return string(b)
	}
	for _, c := range []struct {
		name          string
		request, resp string
	}{
		{"1 MiB words", words(1 << 20), words(64 << 10)},
		{"8 MiB words + 4 MiB", words(8 << 20), words(4 << 20)},
		{"16 MiB + 24 MiB words", words(16 << 20), words(24 << 20)},
		{"16 MiB + 24 MiB noise", noise(16<<20, 571), noise(24<<20, 197)},
	} {
		start := time.Now()
		llmlog.RecordGateway(pool, recordedGwiRow(c.request, c.resp))
		if err := llmlog.Settle(ctx, pool); err != nil {
			t.Fatalf("%s: settle: %v", c.name, err)
		}
		took := time.Since(start)
		var size int64
		if err := pool.QueryRow(ctx, `SELECT pg_column_size(request_user) + pg_column_size(response_content)
			FROM context_llm_log ORDER BY created_at DESC LIMIT 1`).Scan(&size); err != nil {
			t.Fatalf("%s: size: %v", c.name, err)
		}
		t.Logf("%s: %d bytes of text → insert %v, stored %d bytes (failures so far %d)",
			c.name, len(c.request)+len(c.resp), took.Round(time.Millisecond), size, llmlog.InsertFailures())
	}
}

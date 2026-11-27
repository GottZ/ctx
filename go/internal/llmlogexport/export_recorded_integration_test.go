//go:build integration

// The export's seal of recorded rows (issue #571), fail-closed: a gateway row
// leaves ctx-llmlog-export with its bodies sealed unless its metadata.gw is an
// object that does not mark it recorded. The production writer always stamps
// metadata.gw on a gateway row, so the rows without a readable marker are
// written by hand here — they are the states the predicate must not trust
// (llmlog.SQLBodiesServerAdminOnly).
//
//	go test -tags=integration -p 1 -count=1 -run TestExportSealsRecordedRows -v ./internal/llmlogexport/
package llmlogexport_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/llmlogexport"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestExportSealsRecordedRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	const consumer = "0195a6f0-0000-7000-8000-00000000e571"
	rows := []struct {
		name, pipeline string
		consumer       bool
		metadata       string // JSON (the column is NOT NULL)
		sealed         bool
	}{
		{"recorded", "gateway-chat", true, `{"gw":{"v":1,"recorded":true}}`, true},
		{"recorded as a string", "gateway-chat", true, `{"gw":{"v":1,"recorded":"true"}}`, true},
		{"recorded false", "gateway-chat", true, `{"gw":{"v":1,"recorded":false}}`, false},
		{"unrecorded", "gateway-embeddings", true, `{"gw":{"v":1,"op":"embed"}}`, false},
		{"marker missing", "gateway-chat", true, `{"chain":[]}`, true},
		{"gw JSON null", "gateway-rerank", true, `{"gw":null}`, true},
		{"gw not an object", "gateway-chat", true, `{"gw":[1]}`, true},
		{"gateway pipeline without consumer", "gateway-chat", false, `{}`, true},
		{"background pipeline", "query-synthesize", false, `{}`, false},
	}
	for i, r := range rows {
		var cons any
		if r.consumer {
			cons = consumer
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (pipeline, model, host, request_user, response_content, request_system, metadata, consumer_id)
			VALUES ($1, 'm', 'h', $2, $3, '', $4::jsonb, $5::uuid)`,
			r.pipeline, fmt.Sprintf("SECRET-%d-%s", i, r.name), fmt.Sprintf("ANSWER-%d-%s", i, r.name), r.metadata, cons); err != nil {
			t.Fatalf("row %d (%s): %v", i, r.name, err)
		}
	}
	var buf bytes.Buffer
	sum, err := llmlogexport.Export(ctx, pool, &buf, llmlogexport.ExportOptions{UntilMargin: 1})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	want := 0
	for i, r := range rows {
		inExport := strings.Contains(buf.String(), fmt.Sprintf("SECRET-%d-", i)) || strings.Contains(buf.String(), fmt.Sprintf("ANSWER-%d-", i))
		if r.sealed {
			want++
		}
		if inExport == r.sealed {
			t.Errorf("%s: bodies in the export %v, want sealed %v", r.name, inExport, r.sealed)
		}
	}
	if sum.RowsRecordedSealed != int64(want) || sum.RowsTotal != int64(len(rows)) {
		t.Errorf("rows_recorded_sealed %d of %d, want %d of %d", sum.RowsRecordedSealed, sum.RowsTotal, want, len(rows))
	}
}

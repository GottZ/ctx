package llmlog

import (
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The readers of context_llm_log (issue #571, promise 5). A recorded row's
// request_system/request_user/response_content can carry credentials in
// clear text; its bodies are for a server-admin only and for no pipeline.
// This census walks every non-test .go file of the module and lists each file
// whose STRING LITERALS read or write the table (FROM, JOIN, TABLE, INTO,
// UPDATE context_llm_log) or name a body column — comments do not count.
// Every such file is classified below with what it reads and how many such
// literals it holds; a new reader of the table — a whole-row one included
// (SELECT l FROM context_llm_log l, to_jsonb, json_agg), and a new statement
// in a file already listed — turns this test red until it is classified here
// (and, if it reads bodies, given its test).
//
// Not covered, stated rather than assumed: SQL assembled from fragments that
// never spell the table's name, the shell scripts of the repository
// (backup.sh and coverage.sh dump the whole database — a backup, not a
// reader of the log), and the web frontend, which reads the bodies only
// through GET /api/llmlog/{id}.
// llmlogReader is one classified file: the number of its literals that
// read the table or name a body column (struct tags included), and what they
// read.
type llmlogReader struct {
	n    int
	what string
}

var llmlogReaders = map[string]llmlogReader{
	// Bodies: the writer's insert and the retention janitor (EvictBodies NULLs
	// the bodies of every row past scheduler.llmlog_retention_days, recorded
	// rows included — TestV1RecordedConsumerRowAndItsReaders/retention_janitor).
	"internal/llmlog/llmlog.go": {2, "bodies: writer and retention janitor"},
	// Bodies: GET /api/llmlog/{id} — a server-admin reads every row, anyone
	// else only rows of its own knowledge keys (api_key_id); a gateway row has
	// none, so a recorded row is a uniform 404 below server-admin
	// (TestV1RecordedConsumerRowAndItsReaders/detail).
	"internal/handler/llmlog.go": {4, "bodies: per-id detail, server-admin only for gateway rows"},
	// Bodies: ctx-llmlog-export — the bodies of a recorded row, and fail-closed
	// of a gateway row without a readable marker, leave sealed
	// (SQLBodiesServerAdminOnly; TestExportSealsRecordedRows,
	// TestV1RecordedConsumerRowAndItsReaders/export).
	"internal/llmlogexport/export.go": {4, "bodies: bulk export, recorded rows sealed"},
	// Metadata only (the projections name no body column):
	"internal/handler/llmlog_consumer.go": {2, "metadata: GET /api/llmlog list (llmlogListColumns)"},
	"internal/handler/events.go":          {1, "metadata: the llmcalls SSE feed, server-admin"},
	"internal/handler/status.go":          {1, "metadata: max(created_at) of the 24 h window"},
	"internal/llmusage/groups.go":         {2, "metadata: per-pipeline counts and sums (SELECT * over its own CTEs)"},
	"internal/llmusage/window.go":         {1, "metadata: usage windows of the tenant quota"},
	"internal/events/distill_spend.go":    {1, "metadata: count and duration of the distill pipeline's own rows"},
	"cmd/ctx-armcost/report.go":           {3, "metadata: an operator tool's cost report"},
	"cmd/ctx-armcost/pertopic.go":         {2, "metadata: an operator tool's per-topic cost"},
}

// tableRE matches a literal that reads or writes the table; bodyColumnRE one
// that names a body column.
var (
	tableRE      = regexp.MustCompile(`(?i)\b(from|join|table|into|update)\s+context_llm_log\b`)
	bodyColumnRE = regexp.MustCompile(`\b(request_system|request_user|response_content)\b`)
)

func TestLLMLogReadersCensus(t *testing.T) {
	root := gatewayCensusModuleRoot(t)
	fset := token.NewFileSet()
	files := productionFiles(t, fset, root)
	found := map[string][]string{}
	for _, cf := range files {
		ast.Inspect(cf.file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				s = lit.Value
			}
			if tableRE.MatchString(s) || bodyColumnRE.MatchString(s) {
				found[cf.rel] = append(found[cf.rel], fset.Position(lit.Pos()).String())
			}
			return true
		})
	}
	var unclassified, gone []string
	for rel, at := range found {
		if r, ok := llmlogReaders[rel]; !ok || r.n != len(at) {
			unclassified = append(unclassified, fmt.Sprintf("%s: %d literal(s), classified %d (%s)", rel, len(at), r.n, strings.Join(at, ", ")))
		}
	}
	for rel := range llmlogReaders {
		if _, ok := found[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(gone)
	t.Logf("census: %d production files, %d read or write context_llm_log: %v", len(files), len(found), keys(found))
	if len(unclassified) > 0 {
		t.Errorf("files whose literals read context_llm_log without a classification in llmlogReaders "+
			"(a recorded row's bodies are for a server-admin only — classify the reader and give it its test):\n  %s",
			strings.Join(unclassified, "\n  "))
	}
	if len(gone) > 0 {
		t.Errorf("classified readers that no longer read context_llm_log (drop them from llmlogReaders): %v", gone)
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

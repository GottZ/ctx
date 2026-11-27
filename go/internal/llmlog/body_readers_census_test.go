package llmlog

import (
	"go/ast"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The readers of the body columns (issue #571, promise 5). A recorded row's
// request_system/request_user/response_content can carry credentials in
// clear text; its bodies are for a server-admin only and for no pipeline.
// This census walks every non-test .go file of the module and lists each
// file whose STRING LITERALS name a body column (SQL, a JSON key) or read a
// row of context_llm_log whole — comments do not count. Every such file is
// classified below with what it does with a recorded row; a new reader of
// the columns turns this test red until it is classified here (and given its
// test).
//
// Not covered, stated rather than assumed: SQL assembled from fragments that
// never spell a column name (none today), the shell scripts of the
// repository (backup.sh and coverage.sh dump the whole database — a backup,
// not a reader of the log), and the web frontend, which reads the bodies
// only through GET /api/llmlog/{id}.
var bodyColumnReaders = map[string]string{
	// The writer: the insert and the retention janitor (EvictBodies NULLs the
	// bodies of every row past scheduler.llmlog_retention_days, recorded rows
	// included — TestRecordedRowsUnderTheRetentionJanitor).
	"internal/llmlog/llmlog.go": "writer and retention janitor",
	// GET /api/llmlog/{id}: a server-admin reads every row; anyone else only
	// rows of its own knowledge keys (api_key_id) — a gateway row has none, so
	// a recorded row is a uniform 404 below server-admin
	// (TestRecordedRowReaders, detail).
	"internal/handler/llmlog.go": "per-id detail, server-admin only for gateway rows",
	// ctx-llmlog-export: the bodies of a recorded row (and fail-closed of a
	// gateway row without a readable marker) leave sealed
	// (SQLBodiesServerAdminOnly; TestRecordedRowReaders, export).
	"internal/llmlogexport/export.go": "bulk export, recorded rows sealed",
	// The status page's per-pipeline counts: SELECT * reads its own CTEs
	// (edge, roll), the table only for counts and sums — no body.
	"internal/llmusage/groups.go": "aggregates only (SELECT * over its CTEs)",
}

// bodyColumnRE matches a body column name as a word inside a literal;
// wholeRowRE a literal that reads context_llm_log whole (SELECT *, alias.*,
// to_jsonb/row_to_json of a row), which carries the bodies without naming
// them.
var (
	bodyColumnRE = regexp.MustCompile(`\b(request_system|request_user|response_content)\b`)
	wholeRowRE   = regexp.MustCompile(`(?is)context_llm_log.*?(\bselect\s+\*|\w\.\*|to_jsonb\s*\(|row_to_json\s*\()|(\bselect\s+\*|\w\.\*|to_jsonb\s*\(|row_to_json\s*\().*?context_llm_log`)
)

func TestBodyColumnReadersCensus(t *testing.T) {
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
			if bodyColumnRE.MatchString(s) || wholeRowRE.MatchString(s) {
				found[cf.rel] = append(found[cf.rel], fset.Position(lit.Pos()).String())
			}
			return true
		})
	}
	var unclassified, gone []string
	for rel, at := range found {
		if _, ok := bodyColumnReaders[rel]; !ok {
			unclassified = append(unclassified, rel+" ("+strings.Join(at, ", ")+")")
		}
	}
	for rel := range bodyColumnReaders {
		if _, ok := found[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(gone)
	t.Logf("census: %d production files, %d name a body column: %v", len(files), len(found), keys(found))
	if len(unclassified) > 0 {
		t.Errorf("files whose literals name a context_llm_log body column without a classification in bodyColumnReaders "+
			"(a recorded row's bodies are for a server-admin only — classify the reader and give it its test):\n  %s",
			strings.Join(unclassified, "\n  "))
	}
	if len(gone) > 0 {
		t.Errorf("classified readers that no longer name a body column (drop them from bodyColumnReaders): %v", gone)
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

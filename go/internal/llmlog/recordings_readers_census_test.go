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

// The readers of context_llm_recordings (issue #571, migration 197). A
// recording row's request_body and response_body are what a recording
// consumer sent and got, whole — credentials in a prompt included; no ctx
// surface reads them, only a server operator in the database
// (docs/operations.md "Recording a consumer"). This census walks every
// non-test .go file of the module and lists each file whose STRING LITERALS
// name the table or one of its body columns — comments do not count. Every
// such file is classified below with how many such literals it holds; a new
// reader or writer of the table — a whole-row one included, and a new
// statement in a file already listed — turns this test red until it is
// classified here (and, if it hands bodies to anybody, given its test).
//
// Not covered, stated rather than assumed: SQL assembled from fragments that
// never spell the table's name, the shell scripts of the repository
// (backup.sh and coverage.sh dump the whole database — a backup, not a
// reader), and the web frontend, which has no route to the table at all.
// recordingsReader is one classified file: the number of its literals that
// name the table or a body column, and what they do.
type recordingsReader struct {
	n    int
	what string
}

var recordingsReaders = map[string]recordingsReader{
	// The writer (Open, Finish, Insert) and the retention janitor's statement
	// (evictRecordingBodies, run by EvictBodies) — no SELECT anywhere.
	"internal/llmlog/recordings.go": {4, "writes: open, finish, insert; the retention janitor NULLs the bodies"},
	// The janitor's presence probe (EvictBodies: no table before migration
	// 197) — a catalog lookup, no row read.
	"internal/llmlog/llmlog.go": {1, "catalog: to_regclass presence probe of the janitor"},
	// Text only: the refusal of record_bodies without its confirmation names
	// the table (recordConfirmRefusal) — no statement.
	"internal/handler/model_consumers.go": {1, "text: the confirmation refusal names the table"},
}

// tableRE matches a literal that reads or writes the table; bodyColumnRE one
// that names a body column.
var (
	tableRE      = regexp.MustCompile(`\bcontext_llm_recordings\b`)
	bodyColumnRE = regexp.MustCompile(`\b(request_body|response_body)\b`)
)

func TestRecordingsReadersCensus(t *testing.T) {
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
		if r, ok := recordingsReaders[rel]; !ok || r.n != len(at) {
			unclassified = append(unclassified, fmt.Sprintf("%s: %d literal(s), classified %d (%s)", rel, len(at), r.n, strings.Join(at, ", ")))
		}
	}
	for rel := range recordingsReaders {
		if _, ok := found[rel]; !ok {
			gone = append(gone, rel)
		}
	}
	sort.Strings(unclassified)
	sort.Strings(gone)
	t.Logf("census: %d production files, %d name context_llm_recordings or its bodies: %v", len(files), len(found), keys(found))
	if len(unclassified) > 0 {
		t.Errorf("files whose literals name context_llm_recordings or its bodies without a classification in recordingsReaders "+
			"(a recording's bodies reach no ctx surface — classify the file and give it its test):\n  %s",
			strings.Join(unclassified, "\n  "))
	}
	if len(gone) > 0 {
		t.Errorf("classified files that no longer name context_llm_recordings (drop them from recordingsReaders): %v", gone)
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

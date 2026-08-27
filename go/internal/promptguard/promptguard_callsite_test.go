// Wave H11 prompt-pipeline call-site sync gate (design/04 §4.1-e, §7-H11):
// the doctrine "foreign text enters a prompt through exactly ONE function"
// (design 04 §4.0) holds only for as long as EVERY prompt pipeline is actually
// wired to that function. Waves H2-H8 wired the pipelines that exist today;
// this test turns the doctrine into a gate, so a THIRTEENTH, unwired pipeline
// goes red on the day it is written instead of at the next audit.
//
// Cut = MODULE ROOT, not a hand-picked package list — modelled on
// internal/guard/guard_callsite_test.go. The cut is load-bearing, not
// cosmetic: a four-package cut (llm, dream, rrf, chat) misses
// internal/handler/query.go, which is precisely the site where the doctrine
// deliberately does NOT apply. A gate blind to its own exceptions describes a
// smaller world than the one it claims to cover, which is why the test asserts
// the handler site is IN the scanned set.
//
// Heuristic, spelled out here because it is the part a reader must be able to
// challenge: a `Pipeline:` STRING LITERAL marks a pipeline, and the FILE
// carrying it must show a real promptguard call — either directly
// (promptguard.Wrap(, promptguard.Neutralize(, …) or through a package-local
// wrapper (guardText/guardLine), which counts only if the same package
// declares that wrapper in a file that itself calls promptguard. Whole-line
// comments are stripped first, so a doc comment merely NAMING a guard cannot
// pass as wiring. Every file that fails the heuristic must be accounted for in
// one of the two closed lists below — and both lists are verified against the
// tree, so an entry can neither go stale nor mask real wiring.
//
// Out of scope by construction: the embed pipelines (query-embed,
// embed-backfill, dream-keyword-embed). They carry no prompt at all and reach
// llmlog through llm.LogEmbedWire(pool, "<name>", …) — a positional argument,
// not a `Pipeline:` field — so they never enter this scan.
//
// No DB, no build tag: runs under `go test -short`.
package promptguard

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// pipelineMarker is the llmlog.Entry field that names a pipeline. Only STRING
// LITERAL values are call sites: internal/llm/chain.go passes the name through
// as a variable (Pipeline: pipeline / c.Pipeline) and is therefore the generic
// carrier of OTHER pipelines' names, not a pipeline of its own.
const pipelineMarker = "Pipeline:"

// wantPipelineSites pins the number of production Pipeline: sites. It is a
// tripwire, not a budget — see the failure message for what to do when it moves.
//
// 14 since wave A02-8, and the jump from 12 is TWO pipelines rather than one.
// The scan used to read string LITERALS only, so every site whose value is a
// package constant was invisible to it — and the tree had two: cluster-label
// (topiclabel.go:651, `Pipeline: Pipeline`) since the label arm was built, and
// distill-insights (events/distill_extract.go, `Pipeline: distillPipeline`) as
// of this wave. Both build prompts out of foreign text, so a gate that could
// not see them described a smaller world than it claimed, exactly the way the
// module-root cut below exists to prevent. constPipelineSites closes it.
const wantPipelineSites = 14

// noPromptGuard is the CLOSED list of pipelines whose site carries no guard
// and needs none. One justification line each; a reason that does not survive
// review does not belong here. Value = the file the site must live in.
var noPromptGuard = map[string]string{
	// The prompt carries exclusively the caller's own query. Same principal
	// writes the text and reads the answer, so there is no role to escalate
	// into and nothing foreign to neutralise.
	"query-translate": "internal/llm/translate.go",
	"query-temporal":  "internal/llm/temporal.go",

	// Cross-encoder log site. The rerank backend (internal/rerank/rerank.go) is
	// a scoring API without a chat template: it returns a number, never a turn,
	// so no role switch is constructible from the document text.
	"query-rerank": "internal/handler/query.go",
}

// guardedElsewhere is the CLOSED list of Pipeline: sites that sit in a
// metadata-only llmlog helper while their pipeline's foreign text is guarded
// in a different file of the same package. Value = the file that must hold the
// guard; the test verifies it does, so this is a cross-reference, not a pass.
var guardedElsewhere = map[string]string{
	// internal/chat/engine.go recordLLM() is the metadata-only llmlog site
	// (design/05 §3.6/R9: prompt bodies stay empty by construction). The
	// pipeline's foreign text is the tool return channel, neutralised in
	// tools.go by wave H7.
	"web-chat": "internal/chat/tools.go",
}

// constPipelineSite is one closed-list entry: the pipeline an identifier names
// and the file whose declaration has to carry that value.
type constPipelineSite struct {
	pipeline string
	declIn   string
}

// constPipelineSites is the CLOSED list of Pipeline: sites whose value is a
// package CONSTANT rather than a string literal — the shape the doctrine
// actually prefers, because one constant is one authority for a name three
// consumers must agree about (promptguard/budget.go:82-96).
//
// KEYED BY (PACKAGE DIRECTORY, IDENTIFIER), not by the bare identifier
// (round-2 note #19). `Pipeline` is a name any package may declare; keyed
// globally, a second package's `Pipeline` constant with a different value would
// have been mapped onto cluster-label at its own call site as long as SOME
// declaration in the tree carried the claimed string. The directory makes the
// key as local as the constant is.
//
// distill-insights binds to PipelineDistill at COMPILE TIME, so that half can
// never drift; cluster-label is a literal because topiclabel imports this
// package and the reverse edge would be a cycle. Its value AND the file that
// declares it are pinned by checkConstPipelines against the tree.
var constPipelineSites = map[string]constPipelineSite{
	"internal/events:distillPipeline": {PipelineDistill, "internal/events/distill.go"},
	"internal/topiclabel:Pipeline":    {"cluster-label", "internal/topiclabel/topiclabel.go"},
}

// passthroughPipelineIdents is the counter-list: Pipeline: values that name NO
// pipeline of their own because they carry another one's name through.
// internal/llm/chain.go is the generic carrier of every pipeline above. Keyed
// the same way, for the same reason.
//
// Together the two lists are exhaustive by assertion — an identifier in neither
// makes the test red instead of silently vanishing from the count, which is the
// property the literal-only scan lacked.
var passthroughPipelineIdents = map[string]bool{
	"internal/llm:pipeline":   true, // ChatChain / rejection helpers
	"internal/llm:c.Pipeline": true, // ChainCall.Do onto llmlog.Entry
}

// constIdentAt matches an identifier (optionally one selector deep) at the
// start of a Pipeline: value.
var constIdentAt = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?`)

// constDecl finds the declaration of a constant whose value is a string
// literal, so an entry in constPipelineSites cannot claim a name the tree does
// not give it. Both spellings the tree uses are covered: `const X = "…"` and a
// name inside a const block.
var constDecl = regexp.MustCompile(`(?m)^\s*(?:const\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*"([^"]*)"`)

// constAlias finds a constant declared as ANOTHER constant — the shape the arm
// uses so the name has one authority (`const distillPipeline =
// promptguard.PipelineDistill`, events/distill.go:117). One level of
// indirection is resolved; a longer chain would be a shape nothing in the tree
// has, and guessing at it would make the check assert less than it reads.
var constAlias = regexp.MustCompile(`(?m)^\s*(?:const\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)?)\s*$`)

var (
	directGuardCall = regexp.MustCompile(`promptguard\.[A-Z][A-Za-z0-9_]*\(`)
	helperGuardCall = regexp.MustCompile(`\b(?:guardText|guardLine)\(`)
	helperGuardDecl = regexp.MustCompile(`func (?:guardText|guardLine)\(`)
)

// pipelineSite is one production Pipeline: string-literal occurrence.
type pipelineSite struct {
	rel      string
	pipeline string
	line     int
	guarded  bool
}

func TestPromptPipelineCallSitesAreGuarded(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	// Zuschnitt gate 1: the walk root really is the module root. A cut narrowed
	// to package directories loses go.mod and stops here.
	if _, serr := os.Stat(filepath.Join(root, "go.mod")); serr != nil {
		t.Fatalf("scan root %s is not the module root (no go.mod): %v", root, serr)
	}

	guardedFiles, sites := scanModuleForPipelines(t, root)
	byPipeline := make(map[string]pipelineSite, len(sites))
	for _, s := range sites {
		byPipeline[s.pipeline] = s
	}

	// Zuschnitt gate 2: the module-root cut MUST see the handler site. This is
	// the probe against a narrowed grep — internal/handler/query.go lies
	// outside the prompt-building packages and is the one site whose exception
	// is substantive rather than incidental.
	if _, ok := byPipeline["query-rerank"]; !ok {
		t.Errorf("scan did not reach internal/handler/query.go (query-rerank): the scan root is narrower than the module root")
	}

	if len(sites) != wantPipelineSites {
		t.Errorf("found %d Pipeline: string-literal sites, want %d.\n"+
			"NEW pipeline: wire promptguard into its prompt builder, then raise wantPipelineSites.\n"+
			"REMOVED pipeline: lower wantPipelineSites and drop its entry from noPromptGuard/guardedElsewhere.\n"+
			"seen: %v", len(sites), wantPipelineSites, slices.Sorted(maps.Keys(byPipeline)))
	}

	// The closed lists do not grow, shrink or drift. Compared against literals
	// on purpose: reading the expectation out of the maps would assert nothing.
	if got, want := slices.Sorted(maps.Keys(noPromptGuard)), []string{"query-rerank", "query-temporal", "query-translate"}; !slices.Equal(got, want) {
		t.Errorf("noPromptGuard list changed: got %v, want %v — a new unguarded pipeline needs wiring (H2-H8 pattern), not an exception", got, want)
	}
	if got, want := slices.Sorted(maps.Keys(guardedElsewhere)), []string{"web-chat"}; !slices.Equal(got, want) {
		t.Errorf("guardedElsewhere list changed: got %v, want %v", got, want)
	}

	for _, s := range sites {
		_, excused := noPromptGuard[s.pipeline]
		_, elsewhere := guardedElsewhere[s.pipeline]
		if !s.guarded && !excused && !elsewhere {
			t.Errorf("%s:%d: pipeline %q builds a prompt with no promptguard call in its file — wire it (promptguard.Wrap/Neutralize or the package guardText/guardLine), or justify it in noPromptGuard", s.rel, s.line, s.pipeline)
		}
	}

	// An identifier in neither closed list: counted above, named here.
	for _, s := range sites {
		if name, found := strings.CutPrefix(s.pipeline, unknownIdentPrefix); found {
			t.Errorf("%s:%d: Pipeline: %s is neither in constPipelineSites (it names a pipeline) nor in passthroughPipelineIdents (it carries another one's name) — decide which",
				s.rel, s.line, name)
		}
	}

	checkClosedList(t, byPipeline, noPromptGuard, "noPromptGuard", nil)
	checkClosedList(t, byPipeline, guardedElsewhere, "guardedElsewhere", guardedFiles)
	checkConstPipelines(t, root, byPipeline)
}

// checkClosedList holds each excused pipeline to its own claim: the site still
// exists, still lives in the named file and is still genuinely unguarded — an
// entry may not mask wiring that is actually there. When guardFiles is
// non-nil the map value is read as a cross-reference instead, and that file
// must itself carry a real promptguard call.
func checkClosedList(t *testing.T, sites map[string]pipelineSite, list map[string]string, name string, guardFiles map[string]bool) {
	t.Helper()
	for pipeline, file := range list {
		s, ok := sites[pipeline]
		if !ok {
			t.Errorf("%s[%q] is stale: no Pipeline: site carries that name any more", name, pipeline)
			continue
		}
		if s.guarded {
			t.Errorf("%s[%q]: %s:%d IS guarded — drop the entry instead of letting it mask the wiring", name, pipeline, s.rel, s.line)
		}
		if guardFiles == nil {
			if s.rel != file {
				t.Errorf("%s[%q] moved: site is %s, list says %s", name, pipeline, s.rel, file)
			}
			continue
		}
		if !guardFiles[file] {
			t.Errorf("%s[%q] points at %s, which holds no promptguard call — the pipeline is unguarded", name, pipeline, file)
		}
	}
}

// scanModuleForPipelines walks the module root once and returns the set of
// production files holding a direct promptguard call plus every Pipeline:
// string-literal site, each already resolved against the guard heuristic.
func scanModuleForPipelines(t *testing.T, root string) (map[string]bool, []pipelineSite) {
	t.Helper()
	type fileInfo struct{ rel, dir, text string }
	var files []fileInfo
	guardFiles := make(map[string]bool)
	pkgHelper := make(map[string]bool)

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch base := info.Name(); {
			case base == "vendor", base == ".git", base == "node_modules", base == "dist",
				strings.HasPrefix(base, ".gocache"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		text := stripLineComments(string(src))
		files = append(files, fileInfo{rel: rel, dir: filepath.Dir(rel), text: text})
		if directGuardCall.MatchString(text) {
			guardFiles[rel] = true
			if helperGuardDecl.MatchString(text) {
				pkgHelper[filepath.Dir(rel)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	var sites []pipelineSite
	for _, f := range files {
		guarded := guardFiles[f.rel] || (pkgHelper[f.dir] && helperGuardCall.MatchString(f.text))
		for _, p := range pipelineLiterals(f.text, f.dir) {
			sites = append(sites, pipelineSite{rel: f.rel, pipeline: p.name, line: p.line, guarded: guarded})
		}
	}
	return guardFiles, sites
}

// pipelineLiterals returns every `Pipeline: "…"` occurrence with its 1-based
// line. A value that is not a string literal (chain.go's pass-through variable)
// is skipped — it names no pipeline of its own.
func pipelineLiterals(text, dir string) []struct {
	name string
	line int
} {
	var out []struct {
		name string
		line int
	}
	for idx := 0; ; {
		pos := strings.Index(text[idx:], pipelineMarker)
		if pos < 0 {
			return out
		}
		start := idx + pos + len(pipelineMarker)
		idx = start
		rest := strings.TrimLeft(text[start:], " \t")
		name, ok := stringLiteralAt(rest)
		if !ok {
			// Not a literal: a constant reference names a pipeline just as much
			// (constPipelineSites), a pass-through names none.
			ident := constIdentAt.FindString(rest)
			var site constPipelineSite
			site, ok = constPipelineSites[dir+":"+ident]
			name = site.pipeline
			if !ok {
				if !passthroughPipelineIdents[dir+":"+ident] {
					out = append(out, struct {
						name string
						line int
					}{unknownIdentPrefix + dir + ":" + ident, 1 + strings.Count(text[:start], "\n")})
				}
				continue
			}
		}
		out = append(out, struct {
			name string
			line int
		}{name, 1 + strings.Count(text[:start], "\n")})
	}
}

// unknownIdentPrefix marks a site whose value is an identifier in neither
// closed list. It enters the count under a name that cannot collide with a real
// pipeline, so the site is both counted and reported rather than skipped.
const unknownIdentPrefix = "UNKNOWN-IDENT:"

// checkConstPipelines holds constPipelineSites to the tree, the way
// checkClosedList holds the other two lists to theirs: the identifier must be
// declared somewhere as a constant with exactly the claimed string value.
//
// Without it the map would be a second authority for a name — the failure
// class PipelineDistill's own doc names (budget.go:88-95): two spellings do not
// fail loudly, they leave a guard pointing at nothing.
func checkConstPipelines(t *testing.T, root string, sites map[string]pipelineSite) {
	t.Helper()
	// values maps a constant name onto its literal values; declFile records which
	// file declared each of them, so an entry cannot claim a value some OTHER
	// file in the tree happens to carry.
	values := map[string][]string{}
	declFile := map[string]string{} // "<name>=<value>" → rel path
	alias := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		text := stripLineComments(string(src))
		for _, m := range constDecl.FindAllStringSubmatch(text, -1) {
			values[m[1]] = append(values[m[1]], m[2])
			declFile[m[1]+"="+m[2]] = rel
		}
		for _, m := range constAlias.FindAllStringSubmatch(text, -1) {
			alias[m[1]] = append(alias[m[1]], m[2])
			declFile[m[1]+"@alias"] = rel
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for constant declarations: %v", root, err)
	}
	// One level of alias resolution: distillPipeline = promptguard.PipelineDistill
	// = "distill-insights". The qualifier is dropped for the LOOKUP — but the
	// declaring file recorded above is the alias' own, which is what the entry
	// has to name.
	for name, targets := range alias {
		for _, target := range targets {
			if i := strings.LastIndex(target, "."); i >= 0 {
				target = target[i+1:]
			}
			for _, v := range values[target] {
				values[name] = append(values[name], v)
				if _, seen := declFile[name+"="+v]; !seen {
					declFile[name+"="+v] = declFile[name+"@alias"]
				}
			}
		}
	}
	for key, entry := range constPipelineSites {
		ident := key
		if i := strings.LastIndex(key, ":"); i >= 0 {
			ident = key[i+1:]
		}
		if !slices.Contains(values[ident], entry.pipeline) {
			t.Errorf("constPipelineSites[%q] claims %q, but no constant of that name in the tree carries it (found %v) — the entry is stale",
				key, entry.pipeline, values[ident])
			continue
		}
		// The DECLARATION has to sit where the entry says. Without this the value
		// check alone would accept a same-named constant declared anywhere.
		if got := declFile[ident+"="+entry.pipeline]; got != entry.declIn {
			t.Errorf("constPipelineSites[%q] says the constant is declared in %s, but it is in %s",
				key, entry.declIn, got)
		}
		if _, ok := sites[entry.pipeline]; !ok {
			t.Errorf("constPipelineSites[%q] is stale: no Pipeline: site carries %q any more", key, entry.pipeline)
		}
	}
}

// stringLiteralAt reads the interpreted string literal at s[0]; false if s does
// not start one or the literal is unterminated on its line.
func stringLiteralAt(s string) (string, bool) {
	if s == "" || s[0] != '"' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return s[1:i], true
		case '\n':
			return "", false
		}
	}
	return "", false
}

// stripLineComments blanks whole-line // comments while preserving line count,
// so a doc comment that merely NAMES guardText or promptguard cannot be read as
// wiring. Trailing comments are left alone: separating them from a string
// literal correctly would require a parser, and no file in this tree carries
// call syntax in a trailing comment.
func stripLineComments(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

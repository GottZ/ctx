package prompts_test

// The completeness gate (E04-5 / T04-21). A registry whose entries are correct
// says nothing about the bodies that have NO entry, and that is the failure
// this file exists to make loud: a prompt added next year without a
// registration would go to a model and leave rows that name no prompt at all.
//
// Four questions, one AST walk over every non-test .go file of the module:
//
//  1. Does every prompt-shaped string declaration have a registration? A
//     declaration is prompt-shaped when its NAME contains "prompt" and its
//     value is a constant string of at least minBodyLen bytes. Anything that
//     legitimately has no identity stands in exempted with a reason.
//  2. Is every registration's Owner the import path of the package the
//     Register call actually lives in, and its ID that package's name plus the
//     identifier? A registration that lies about its home would send an
//     analyst to the wrong file.
//  3. Does every llm.ChainCall literal set Prompt? That struct is the shared
//     wire seam of six pipelines: a literal without the field compiles, runs,
//     and writes rows with no prompt_id — the one silent hole a registry alone
//     cannot close.
//  4. Does every function that builds a slim llmlog row stamp the identity?
//     Removing the one line in ChainCall.Do would take prompt_id off six
//     pipelines at once and leave every other gate in this file green — the
//     registry would still be perfect, and the rows would say nothing.
//  5. Does each registered body still hash to what its version claims? The
//     version is a label, not a checksum, so an edited body would otherwise
//     keep the old date and every row written after it would be mislabelled.
//     This is the pin that turns the label into a statement.
//
// An AST walk rather than a regexp over the file text, for the reason
// config/envscan.go's header gives at length: a struct tag or a comment that
// merely CONTAINS the pattern is not a declaration of it. Parsing without
// ParseComments keeps comments out of the tree entirely, which is also why the
// hash below is stable against the prose written around a body.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/prompts"
)

const (
	// moduleRoot is go/ — this test's working directory is go/internal/prompts.
	moduleRoot = "../.."
	modulePath = "github.com/GottZ/ctx"
	// minBodyLen keeps version constants ("v5.2") and other short strings out
	// of the candidate set. The shortest registered body is the legacy daily
	// report at 213 bytes; the longest non-body string named "…prompt…" in the
	// tree is 4 bytes.
	minBodyLen = 120
)

// exempted are the prompt-shaped declarations that deliberately carry no
// identity, each with the reason it does not. A new prompt anywhere in the
// tree fails the gate until it is either registered or listed here — which is
// the point: the decision gets made, once, in writing.
var exempted = map[string]string{
	"goldbench.taggingSystemPrompt":   "bench axis (cmd/ctx-goldbench), never a serving pipeline — writes no context_llm_log row",
	"goldbench.titleSystemPrompt":     "bench axis, as above",
	"goldbench.taggingSystemPromptV2": "bench A/B variant of taggingSystemPrompt, only ever sent by the bench harness",
	"goldbench.titleSystemPromptV2":   "bench A/B variant of titleSystemPrompt, as above",
	"goldset.gqSystemPrompt":          "gold-set generation tooling; already frozen by its own goldset.PromptSHA256 stamp",
	"goldset.sessSystemPrompt":        "gold-set generation tooling, as above",
	"goldset.mhSystemPrompt":          "gold-set generation tooling, as above",
	"goldset.globSystemPrompt":        "gold-set generation tooling, as above",
	"goldset.judgeSystemPrompt":       "gold-set judge tooling; frozen by goldset.JudgePromptSHA256",
}

// stampSites are the functions that must carry the stamp, with the reason
// each one is the place for it. They are named rather than derived because
// there are three of them and each is a deliberate choice about WHERE in a
// row's life the identity is written.
var stampSites = map[string]string{
	"llm.ChainCall.Do":    "the shared wire seam of six pipelines — classify, translate, temporal, rerank-judge, distill, cluster-label",
	"llm.Synthesize":      "query-synthesize builds its row by hand and picks its body through selectSystemPrompt",
	"dream.newDreamEntry": "one constructor for all five dream stages, so the stamp is on the row from its first line",
}

// stampExempt are the functions that build a slim row through newChainEntry
// and legitimately stamp nothing.
var stampExempt = map[string]string{
	"llm.embedWireEntry": "an embedding call sends no prompt at all",
}

// bodyPins holds, per registered id, the declarations whose SOURCE TEXT makes
// up the body as the model receives it, and the SHA-256 over them. Doc
// comments are outside the hashed span, so the prose explaining a version does
// not invalidate it.
//
// Two entries name more than one declaration because their text is assembled
// at the send site out of the body plus an appendix promoted from a bench A/B
// (rerankHarden, clusterHarden). Two entries name a FUNCTION, because their
// body is composed rather than declared: topiclabel.systemPromptFor builds its
// text from five literals, and dream.dailySynthesisPromptIntl IS the inline
// literal of dailySynthesisPromptFor's non-legacy branch.
//
// A red pin means the text changed. The fix is never to re-hash alone: decide
// whether the change is a new generation, bump the version in the Register
// call, and then update the hash in the same commit.
var bodyPins = map[string]struct {
	decls []string
	sha   string
}{
	"llm.classifySystemPrompt":           {decls: []string{"classifySystemPrompt"}, sha: "405dc65451808a2c"},
	"llm.translationSystemPrompt":        {decls: []string{"translationSystemPrompt"}, sha: "23024ee5188c3592"},
	"llm.recordingTranslateSystemPrompt": {decls: []string{"recordingTranslateSystemPrompt"}, sha: "4db2b8ae2e8d7445"},
	"llm.temporalPromptTemplate":         {decls: []string{"temporalPromptTemplate"}, sha: "4395901488d3293e"},
	"llm.systemPromptV52":                {decls: []string{"systemPromptV52"}, sha: "43d61b3933ab77ac"},
	"llm.systemPromptV6":                 {decls: []string{"systemPromptV6"}, sha: "7ca8b734e6802895"},
	"rrf.rerankSystemPrompt":             {decls: []string{"rerankSystemPrompt", "rerankHarden"}, sha: "704af02a4a835956"},
	"events.distillSystemPrompt":         {decls: []string{"distillSystemPrompt"}, sha: "43baaac40914addd"},
	"dream.temporalValidationPrompt":     {decls: []string{"temporalValidationPrompt"}, sha: "01760f56ce9bc305"},
	"dream.recurrenceSystemPrompt":       {decls: []string{"recurrenceSystemPrompt"}, sha: "7554cbdc9272a364"},
	"dream.keywordSystemPrompt":          {decls: []string{"keywordSystemPrompt"}, sha: "689196d3a2a42fc8"},
	"dream.dailySynthesisSystemPrompt":   {decls: []string{"dailySynthesisSystemPrompt"}, sha: "943ca266a09f0e83"},
	"dream.dailySynthesisPromptIntl":     {decls: []string{"dailySynthesisPromptFor"}, sha: "800f50380c204724"},
	"dream.dreamSystemPrompt":            {decls: []string{"dreamSystemPrompt"}, sha: "a3a0c9e4e6941edb"},
	"dream.decideEvalSystemPrompt":       {decls: []string{"decideEvalSystemPrompt"}, sha: "84d14295d57bf9df"},
	"dream.decideRecurrenceSystemPrompt": {decls: []string{"decideRecurrenceSystemPrompt"}, sha: "9262b8dca36bb537"},
	"topiclabel.systemPromptFor":         {decls: []string{"systemPromptFor", "clusterHarden"}, sha: "8cdb18e4db04bb57"},
}

// --- scan model.

type registration struct {
	id, version, owner string
	pkgPath            string // import path of the package the call lives in
	pkgName            string // its package clause name
	pos                string
}

// funcFacts is what one function does with the row-building helpers.
type funcFacts struct {
	pos             string
	callsChainEntry bool
	callsStamp      bool
}

type declaration struct {
	name    string
	pkgName string
	pos     string
	value   string // resolved constant string, "" when the value is not constant
	source  string // source text of the declaration, doc comment excluded
}

// pendingReg is a Register call before its arguments are resolved: two of
// them are written as the package's own version constants (PromptVersionV52 /
// V6, the words config key query.prompt_version accepts), so the arguments can
// only be folded once every declaration of the tree has been read.
type pendingReg struct {
	args             [3]ast.Expr
	pkgPath, pkgName string
	pos              string
}

type scan struct {
	regs    []registration
	pending []pendingReg
	// decls is keyed "<import path>\x00<identifier>".
	decls map[string]declaration
	// bodies are the prompt-shaped candidates, keyed "<package>.<identifier>".
	bodies map[string]declaration
	// chainCalls without a Prompt field, as "file:line".
	chainCallsWithoutPrompt []string
	// funcs is keyed "<package>.[<receiver type>.]<function>".
	funcs map[string]funcFacts
}

func declKey(pkgPath, name string) string { return pkgPath + "\x00" + name }

// --- The walk.

func scanModule(t *testing.T) *scan {
	t.Helper()
	s := &scan{decls: map[string]declaration{}, bodies: map[string]declaration{}, funcs: map[string]funcFacts{}}
	fset := token.NewFileSet()

	for _, top := range []string{"internal", "cmd", "web"} {
		root := filepath.Join(moduleRoot, top)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file, err := goparser.ParseFile(fset, path, src, goparser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("prompt scan: parse %s: %w", path, err)
			}
			s.collect(fset, file, src, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	s.resolveRegistrations(t)
	return s
}

// resolveRegistrations folds the three arguments of every Register call, now
// that every declaration is known: a literal is itself, an identifier is the
// string constant of the same package.
func (s *scan) resolveRegistrations(t *testing.T) {
	t.Helper()
	for _, p := range s.pending {
		var vals [3]string
		ok := true
		for i, arg := range p.args {
			v, found := s.resolveString(p.pkgPath, arg)
			if !found {
				t.Errorf("%s: argument %d of prompts.Register is not a readable string constant — "+
					"the identity has to be legible from the source, not assembled at run time", p.pos, i+1)
				ok = false
				break
			}
			vals[i] = v
		}
		if !ok {
			continue
		}
		s.regs = append(s.regs, registration{
			id: vals[0], version: vals[1], owner: vals[2],
			pkgPath: p.pkgPath, pkgName: p.pkgName, pos: p.pos,
		})
	}
}

// resolveString folds a string constant expression, following identifiers into
// the declarations of their own package.
func (s *scan) resolveString(pkgPath string, e ast.Expr) (string, bool) {
	if v, ok := constString(e); ok {
		return v, true
	}
	switch v := e.(type) {
	case *ast.Ident:
		d, ok := s.decls[declKey(pkgPath, v.Name)]
		if !ok || d.value == "" {
			return "", false
		}
		return d.value, true
	case *ast.ParenExpr:
		return s.resolveString(pkgPath, v.X)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := s.resolveString(pkgPath, v.X)
		if !ok {
			return "", false
		}
		r, ok := s.resolveString(pkgPath, v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

func (s *scan) collect(fset *token.FileSet, file *ast.File, src []byte, path string) {
	pkgPath := importPathOf(path)
	pkgName := file.Name.Name
	at := func(n ast.Node) string {
		p := fset.Position(n.Pos())
		return fmt.Sprintf("%s:%d", filepath.ToSlash(p.Filename), p.Line)
	}
	text := func(n ast.Node) string {
		lo := fset.Position(n.Pos()).Offset
		hi := fset.Position(n.End()).Offset
		return string(src[lo:hi])
	}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			s.funcs[pkgName+"."+receiverPrefix(d)+d.Name.Name] = funcFactsOf(d, at(d))
			if d.Recv != nil {
				continue // methods carry no package-level identity
			}
			s.decls[declKey(pkgPath, d.Name.Name)] = declaration{
				name: d.Name.Name, pkgName: pkgName, pos: at(d), source: text(d),
			}
		case *ast.GenDecl:
			if d.Tok != token.CONST && d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					dec := declaration{name: name.Name, pkgName: pkgName, pos: at(vs), source: text(vs)}
					if i < len(vs.Values) {
						if v, ok := constString(vs.Values[i]); ok {
							dec.value = v
						}
					}
					s.decls[declKey(pkgPath, name.Name)] = dec
					if isPromptShaped(dec) {
						s.bodies[pkgName+"."+name.Name] = dec
					}
				}
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if args, ok := registerArgs(node); ok {
				s.pending = append(s.pending, pendingReg{
					args: args, pkgPath: pkgPath, pkgName: pkgName, pos: at(node),
				})
			}
		case *ast.CompositeLit:
			if isChainCallLiteral(node.Type) && !hasKey(node, "Prompt") {
				s.chainCallsWithoutPrompt = append(s.chainCallsWithoutPrompt, at(node))
			}
		}
		return true
	})
}

func importPathOf(path string) string {
	rel, err := filepath.Rel(moduleRoot, filepath.Dir(path))
	if err != nil {
		return ""
	}
	return modulePath + "/" + filepath.ToSlash(rel)
}

// isPromptShaped is the candidate rule: a name that says "prompt" on a
// constant string long enough to be one.
func isPromptShaped(d declaration) bool {
	return strings.Contains(strings.ToLower(d.name), "prompt") && len(d.value) >= minBodyLen
}

// constString folds a string constant expression — a literal, or literals
// joined by +, which is how the distill and rerank bodies are written.
func constString(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.ParenExpr:
		return constString(v.X)
	case *ast.BinaryExpr:
		if v.Op != token.ADD {
			return "", false
		}
		l, ok := constString(v.X)
		if !ok {
			return "", false
		}
		r, ok := constString(v.Y)
		if !ok {
			return "", false
		}
		return l + r, true
	}
	return "", false
}

// registerArgs matches a prompts.Register(id, version, owner) call and hands
// back its three argument expressions, unresolved.
func registerArgs(call *ast.CallExpr) ([3]ast.Expr, bool) {
	var out [3]ast.Expr
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Register" {
		return out, false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "prompts" || len(call.Args) != 3 {
		return out, false
	}
	copy(out[:], call.Args)
	return out, true
}

// receiverPrefix renders "ChainCall." for a method and "" for a function, so
// a method reads as llm.ChainCall.Do in the tables above.
func receiverPrefix(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if ident, ok := t.(*ast.Ident); ok {
		return ident.Name + "."
	}
	return ""
}

func funcFactsOf(d *ast.FuncDecl, pos string) funcFacts {
	f := funcFacts{pos: pos}
	if d.Body == nil {
		return f
	}
	ast.Inspect(d.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "newChainEntry" {
				f.callsChainEntry = true
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == "StampPrompt" {
				f.callsStamp = true
			}
		}
		return true
	})
	return f
}

func isChainCallLiteral(t ast.Expr) bool {
	switch v := t.(type) {
	case *ast.SelectorExpr: // llm.ChainCall{…}
		x, ok := v.X.(*ast.Ident)
		return ok && x.Name == "llm" && v.Sel.Name == "ChainCall"
	case *ast.Ident: // ChainCall{…} inside package llm
		return v.Name == "ChainCall"
	}
	return false
}

func hasKey(lit *ast.CompositeLit, key string) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
			return true
		}
	}
	return false
}

// --- The gates.

// TestEveryPromptBodyIsRegistered is the completeness gate itself.
func TestEveryPromptBodyIsRegistered(t *testing.T) {
	s := scanModule(t)

	registered := map[string]bool{}
	for _, r := range s.regs {
		registered[r.id] = true
	}

	keys := make([]string, 0, len(s.bodies))
	for k := range s.bodies {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		body := s.bodies[key]
		if registered[key] {
			continue
		}
		if reason, ok := exempted[key]; ok {
			if reason == "" {
				t.Errorf("%s is exempted without a reason (%s)", key, body.pos)
			}
			continue
		}
		t.Errorf("%s (%s, %d bytes) sends a prompt body with no identity.\n"+
			"    Register it next to its text — var promptX = prompts.Register(%q, \"<version>\", %q) —\n"+
			"    and stamp it on the row its pipeline writes; or, if it never reaches a model,\n"+
			"    add it to exempted with the reason.",
			key, body.pos, len(body.value), key, modulePath+"/internal/"+body.pkgName)
	}

	// The other direction: an exemption for something that no longer exists is
	// stale prose that would quietly cover a future body of the same name.
	for key := range exempted {
		if _, ok := s.bodies[key]; !ok {
			t.Errorf("exempted names %q, which the tree no longer declares — drop the entry", key)
		}
	}
}

// TestRegistrationsAreHonestAboutTheirHome pins Owner and ID against the place
// the Register call actually sits, and against the live register.
func TestRegistrationsAreHonestAboutTheirHome(t *testing.T) {
	s := scanModule(t)
	if len(s.regs) != len(wantIdentities) {
		t.Errorf("the tree holds %d prompts.Register calls, the register answers with %d",
			len(s.regs), len(wantIdentities))
	}

	seen := map[string]bool{}
	for _, r := range s.regs {
		if r.owner != r.pkgPath {
			t.Errorf("%s: owner %q, but the call lives in %q", r.pos, r.owner, r.pkgPath)
		}
		if !strings.HasPrefix(r.id, r.pkgName+".") {
			t.Errorf("%s: id %q does not start with its package name %q", r.pos, r.id, r.pkgName)
		}
		if seen[r.id] {
			t.Errorf("%s: id %q registered twice in the source", r.pos, r.id)
		}
		seen[r.id] = true

		live, ok := prompts.Lookup(r.id)
		if !ok {
			t.Errorf("%s: %q is written in the source but missing from the running register — "+
				"is the registration in a file no import reaches?", r.pos, r.id)
			continue
		}
		if live.Version != r.version || live.Owner != r.owner {
			t.Errorf("%s: source says {%s %s}, register says {%s %s}",
				r.pos, r.version, r.owner, live.Version, live.Owner)
		}
	}
	for _, want := range wantIdentities {
		if !seen[want.ID] {
			t.Errorf("%q is in the register but no Register call in the tree declares it", want.ID)
		}
	}
}

// TestEveryChainCallCarriesItsPromptIdentity closes the hole a registry cannot
// see: the shared wire seam of six pipelines, used without the field.
func TestEveryChainCallCarriesItsPromptIdentity(t *testing.T) {
	s := scanModule(t)
	for _, pos := range s.chainCallsWithoutPrompt {
		t.Errorf("%s: llm.ChainCall literal without Prompt — the row it writes would carry no prompt_id", pos)
	}
}

// TestEveryRowBuilderStampsTheIdentity closes the hole the other gates cannot
// see: a stamp line deleted from a row-building function. Nothing else in this
// file would notice — the registry would stay complete and every ChainCall
// literal would keep its field — while six pipelines stopped naming their
// prompt.
func TestEveryRowBuilderStampsTheIdentity(t *testing.T) {
	s := scanModule(t)

	for name, why := range stampSites {
		f, ok := s.funcs[name]
		if !ok {
			t.Errorf("%s is named as a stamp site but no such function exists — "+
				"if it was renamed, move the entry with it (%s)", name, why)
			continue
		}
		if !f.callsStamp {
			t.Errorf("%s: %s — but its body no longer calls StampPrompt, so the rows it "+
				"writes carry no prompt_id", f.pos, name)
		}
	}

	// A NEW caller of newChainEntry has to decide which of the two it is,
	// rather than inheriting silence.
	for name, f := range s.funcs {
		if !f.callsChainEntry {
			continue
		}
		if _, ok := stampSites[name]; ok {
			continue
		}
		if _, ok := stampExempt[name]; ok {
			continue
		}
		t.Errorf("%s: %s builds a slim llmlog row and is in neither stampSites nor stampExempt — "+
			"stamp its prompt identity, or record why it has none", f.pos, name)
	}
}

// TestBodiesStillHashToTheirVersion is the drift pin described at bodyPins.
func TestBodiesStillHashToTheirVersion(t *testing.T) {
	s := scanModule(t)

	ids := make([]string, 0, len(bodyPins))
	for id := range bodyPins {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		pin := bodyPins[id]
		live, ok := prompts.Lookup(id)
		if !ok {
			t.Errorf("bodyPins names %q, which nothing registers", id)
			continue
		}
		var b strings.Builder
		missing := false
		for _, name := range pin.decls {
			d, ok := s.decls[declKey(live.Owner, name)]
			if !ok {
				t.Errorf("%s: declaration %q not found in %s", id, name, live.Owner)
				missing = true
				break
			}
			b.WriteString(d.source)
			b.WriteString("\n")
		}
		if missing {
			continue
		}
		sum := sha256.Sum256([]byte(b.String()))
		got := hex.EncodeToString(sum[:])[:16]
		if pin.sha == "" {
			t.Errorf("%s has no pinned hash yet — it is %s", id, got)
			continue
		}
		if got != pin.sha {
			t.Errorf("%s changed: hash %s, pinned %s (version %q).\n"+
				"    A changed body is a new generation: bump the version in its Register call,\n"+
				"    then update the pin in the same commit.", id, got, pin.sha, live.Version)
		}
	}
	for _, want := range wantIdentities {
		if _, ok := bodyPins[want.ID]; !ok {
			t.Errorf("%q has no entry in bodyPins — every registered body is pinned", want.ID)
		}
	}
}

// The registry's own contract, exercised against the LIVE register: the test
// package blank-imports every package that owns a prompt body, so All()
// answers with what a running ctxd would carry, not with a fixture.
//
// It is an EXTERNAL test package on purpose (prompts_test, not prompts): the
// owner packages import prompts, so an in-package test could not import them
// back without a cycle.
package prompts_test

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/prompts"

	// The five owner packages. Blank imports, because what is wanted from them
	// is their init: every prompt body registers itself when its package loads.
	_ "github.com/GottZ/ctx/internal/dream"
	_ "github.com/GottZ/ctx/internal/events"
	_ "github.com/GottZ/ctx/internal/llm"
	_ "github.com/GottZ/ctx/internal/rrf"
	_ "github.com/GottZ/ctx/internal/topiclabel"
)

// wantIdentities is the register as this tree carries it (with #575 the
// recordings view's reading aid, llm.recordingTranslateSystemPrompt): bodies over
// five packages. It is written out rather than counted so the failure of a
// removed registration names WHICH one went missing — a count would only say
// that something did.
//
// Fourteen, not the thirteen design/04 Naht 12 lists: the daily-report
// selector has a second body of its own (dream.dailySynthesisPromptIntl, the
// English branch of dailySynthesisPromptFor), which the design counted as part
// of the selector. See the deviation note in the T04-21 report.
var wantIdentities = []prompts.Identity{
	{ID: "dream.dailySynthesisPromptIntl", Version: "2026-07-31", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.dailySynthesisSystemPrompt", Version: "2026-07-31", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.decideEvalSystemPrompt", Version: "2026-09-20", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.decideRecurrenceSystemPrompt", Version: "2026-09-20", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.dreamSystemPrompt", Version: "v5", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.keywordSystemPrompt", Version: "2026-08-25", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.recurrenceSystemPrompt", Version: "2026-05-06", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "dream.temporalValidationPrompt", Version: "2026-04-24", Owner: "github.com/GottZ/ctx/internal/dream"},
	{ID: "events.distillSystemPrompt", Version: "2026-08-30", Owner: "github.com/GottZ/ctx/internal/events"},
	{ID: "llm.classifySystemPrompt", Version: "2026-06-13", Owner: "github.com/GottZ/ctx/internal/llm"},
	{ID: "llm.recordingTranslateSystemPrompt", Version: "2026-11-27", Owner: "github.com/GottZ/ctx/internal/llm"},
	{ID: "llm.systemPromptV52", Version: "v5.2", Owner: "github.com/GottZ/ctx/internal/llm"},
	{ID: "llm.systemPromptV6", Version: "v6", Owner: "github.com/GottZ/ctx/internal/llm"},
	{ID: "llm.temporalPromptTemplate", Version: "2026-11-21", Owner: "github.com/GottZ/ctx/internal/llm"},
	{ID: "llm.translationSystemPrompt", Version: "2026-11-24", Owner: "github.com/GottZ/ctx/internal/llm"},
	{ID: "rrf.rerankSystemPrompt", Version: "2026-08-16", Owner: "github.com/GottZ/ctx/internal/rrf"},
	{ID: "topiclabel.systemPromptFor", Version: "2026-08-16", Owner: "github.com/GottZ/ctx/internal/topiclabel"},
}

func TestAllIsTheWholeRegister(t *testing.T) {
	got := prompts.All()
	if len(got) != len(wantIdentities) {
		t.Errorf("All() has %d identities, want %d", len(got), len(wantIdentities))
	}

	byID := map[string]prompts.Identity{}
	for _, id := range got {
		if _, dup := byID[id.ID]; dup {
			t.Errorf("All() returned %q twice", id.ID)
		}
		byID[id.ID] = id
	}
	for _, want := range wantIdentities {
		have, ok := byID[want.ID]
		if !ok {
			t.Errorf("missing registration %q — a prompt body lost its identity", want.ID)
			continue
		}
		if have != want {
			t.Errorf("%s = %+v, want %+v", want.ID, have, want)
		}
		delete(byID, want.ID)
	}
	for id, ident := range byID {
		t.Errorf("unexpected registration %q (%+v) — add it to wantIdentities with its version's reason", id, ident)
	}
}

// TestAllIsSortedAndWellFormed holds the shape every consumer may rely on:
// sorted by ID, no empty field, the ID prefixed with its owner's last path
// segment. The byte-exact owner check (owner == the import path of the package
// the Register call lives in) is the AST gate's, in completeness_test.go.
func TestAllIsSortedAndWellFormed(t *testing.T) {
	got := prompts.All()
	for i, id := range got {
		if i > 0 && got[i-1].ID >= id.ID {
			t.Errorf("All() is not sorted at %d: %q after %q", i, id.ID, got[i-1].ID)
		}
		if id.ID == "" || id.Version == "" || id.Owner == "" {
			t.Errorf("incomplete identity %+v", id)
			continue
		}
		pkg := id.Owner[strings.LastIndex(id.Owner, "/")+1:]
		if !strings.HasPrefix(id.ID, pkg+".") {
			t.Errorf("id %q does not name its package %q — the id is <package>.<identifier>", id.ID, pkg)
		}
	}
}

func TestLookupAnswersForEveryRegisteredID(t *testing.T) {
	for _, want := range wantIdentities {
		got, ok := prompts.Lookup(want.ID)
		if !ok {
			t.Errorf("Lookup(%q) found nothing", want.ID)
			continue
		}
		if got != want {
			t.Errorf("Lookup(%q) = %+v, want %+v", want.ID, got, want)
		}
	}
	if _, ok := prompts.Lookup("llm.noSuchPrompt"); ok {
		t.Error("Lookup answered for an id nobody registered")
	}
}

// TestRegisterPanicsOnDuplicateID is the guard. Two bodies under one id would
// make every row carrying that id unattributable, which is worse than not
// starting — so Register panics rather than overwriting or ignoring.
//
// It re-registers an id that is ALREADY taken instead of adding a throwaway
// one, so the register this binary shares with the tests above is not moved by
// running this test; the deferred check asserts exactly that.
func TestRegisterPanicsOnDuplicateID(t *testing.T) {
	before := len(prompts.All())
	taken := wantIdentities[0]

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("Register(%q, ...) with a taken id did not panic", taken.ID)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, taken.ID) {
			t.Errorf("panic message %q does not name the duplicate id", msg)
		}
		if after := len(prompts.All()); after != before {
			t.Errorf("the register grew from %d to %d entries across a rejected registration", before, after)
		}
		if got, _ := prompts.Lookup(taken.ID); got != taken {
			t.Errorf("the rejected registration overwrote the standing one: %+v", got)
		}
	}()

	prompts.Register(taken.ID, "9999-99-99", "github.com/GottZ/ctx/internal/nowhere")
}

func TestRegisterPanicsOnAnEmptyField(t *testing.T) {
	cases := []struct {
		name             string
		id, version, own string
	}{
		{"no id", "", "v1", "github.com/GottZ/ctx/internal/llm"},
		{"no version", "llm.x", "", "github.com/GottZ/ctx/internal/llm"},
		{"no owner", "llm.x", "v1", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("an incomplete registration was accepted — a zero identity would reach an llmlog row")
				}
			}()
			prompts.Register(c.id, c.version, c.own)
		})
	}
}

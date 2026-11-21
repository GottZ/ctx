//go:build integration

// Issue #452 point 5, dream_keywords: the dream's keywords are generated from
// a block's title and content (GenerateKeywords) and stored on the block so
// the next cycles reuse them. They must follow the text: a write that changes
// the title or the content leaves the block without keywords, so its next
// dream generates them from the text it holds (store.InvalidateEmbeddingTx,
// the step every such write runs), and keywords generated from a text that
// was replaced while the model answered are not stored on the replacement.
//
// The tests observe what the next cycle does — whether it asks the model for
// keywords and what it sends — and the column. The window is forced by the
// stubbed keyword call, which blocks until the test releases it, not by
// sleeps. The writes are the production ones (UpdateBlock, UpsertBlock).
//
// Run with:
//
//	go test -tags=integration ./internal/dream/ -run 'TestDreamKeywords' -count=1 -v
package dream_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// i452KeywordMarker is a phrase of the keyword extractor's system prompt
// (keywordSystemPrompt): the stub answers that call.
const i452KeywordMarker = "You extract conceptual keywords"

// errI452Stop ends the cycle right after the keyword step: RunDreamCycle calls
// the throttle once after the temporal review and once after the keywords.
var errI452Stop = errors.New("i452: cycle stopped after the keywords")

// i452Keywords is the stubbed keyword call.
type i452Keywords struct {
	mu      sync.Mutex
	prompts []string
	hold    chan struct{} // nil: answer at once
	entered chan string
}

func (k *i452Keywords) reply(ctx context.Context, userPrompt string) (string, error) {
	k.mu.Lock()
	k.prompts = append(k.prompts, userPrompt)
	hold, entered := k.hold, k.entered
	k.hold, k.entered = nil, nil
	k.mu.Unlock()
	if hold != nil {
		entered <- userPrompt
		select {
		case <-hold:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return `["keyword one","keyword two","keyword three","keyword four","keyword five"]`, nil
}

// holdNext makes the next keyword call block until release is closed.
func (k *i452Keywords) holdNext() (entered <-chan string, release chan struct{}) {
	in := make(chan string, 1)
	release = make(chan struct{})
	k.mu.Lock()
	k.hold, k.entered = release, in
	k.mu.Unlock()
	return in, release
}

func (k *i452Keywords) calls() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.prompts...)
}

// newI452KeywordRig is the temporal rig with an LLM stub that answers the
// temporal review (no dates) and the keyword call.
func newI452KeywordRig(t *testing.T) (*i427Rig, *i452Keywords) {
	t.Helper()
	rig := newI427Rig(t)
	rig.review.record(i427Dates())
	kw := &i452Keywords{}
	swapChatJSON(t, func(ctx context.Context, _, _, _ string, _ *bool, systemPrompt, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		var content string
		var err error
		switch {
		case strings.Contains(systemPrompt, i427ReviewMarker):
			content, err = rig.review.reply(ctx, userPrompt)
		case strings.Contains(systemPrompt, i452KeywordMarker):
			content, err = kw.reply(ctx, userPrompt)
		default:
			return nil, fmt.Errorf("i452: unscripted LLM call")
		}
		if err != nil {
			return nil, err
		}
		return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: content}, EvalCount: 20, PromptTokens: 200}, nil
	})
	return rig, kw
}

// keywordCycle runs one dream cycle that ends after the keyword step.
func (rig *i427Rig) keywordCycle() <-chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), i427Wait)
		defer cancel()
		var n atomic.Int32
		stop := func(context.Context) error {
			if n.Add(1) == 2 {
				return errI452Stop
			}
			return nil
		}
		_, err := dream.RunDreamCycle(ctx, rig.pool, rig.router, dream.DreamOptions(),
			dream.BackoffConfig{Mode: "exp", Factor: 1.6, MinHours: 12, CapHours: 1080, InertOffset: 7},
			[]string{i427Scope}, stop)
		switch {
		case errors.Is(err, errI452Stop):
			done <- nil
		case err == nil:
			done <- errors.New("the cycle ended before its keyword step (no block picked?)")
		default:
			done <- err
		}
	}()
	return done
}

// storedKeywords reads the block's stored keywords and their stamp.
func (rig *i427Rig) storedKeywords(t *testing.T, id string) ([]string, bool) {
	t.Helper()
	var kws []string
	var stamped bool
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT dream_keywords, dream_keywords_generated_at IS NOT NULL FROM context_blocks WHERE id = $1`, id).
		Scan(&kws, &stamped); err != nil {
		t.Fatalf("read keywords: %v", err)
	}
	return kws, stamped
}

// TestDreamKeywordsFollowTheText: after a write that changes the title or the
// content, the block holds no keywords and its next dream asks the model for
// keywords of the text it holds. A write of the tags alone keeps them.
func TestDreamKeywordsFollowTheText(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name       string
		write      func(t *testing.T, rig *i427Rig, b *store.Block)
		newText    string // a part of the text the next keyword call must carry; "" = no call
		regenerate bool
	}{
		{"content update", func(t *testing.T, rig *i427Rig, b *store.Block) {
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Content: str("Now about the tidal power plant in the north bay.")})
		}, "tidal power plant", true},
		{"title update", func(t *testing.T, rig *i427Rig, b *store.Block) {
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Title: str("Harbour crane maintenance")})
		}, "Harbour crane maintenance", true},
		{"content edits", func(t *testing.T, rig *i427Rig, b *store.Block) {
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{{Old: "glacier survey", New: str("volcano survey")}}})
		}, "volcano survey", true},
		{"upsert of a new content", func(t *testing.T, rig *i427Rig, b *store.Block) {
			if _, err := store.UpsertBlock(context.Background(), rig.pool, "decisions", b.Title,
				"The ferry timetable for the winter season.", nil, nil, i427Scope, true,
				store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, ""); err != nil {
				t.Fatalf("upsert: %v", err)
			}
		}, "ferry timetable", true},
		{"tags only", func(t *testing.T, rig *i427Rig, b *store.Block) {
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Tags: []string{"field-work"}})
		}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, kw := newI452KeywordRig(t)
			b := rig.block(t, "Glacier notes", "Plans for the glacier survey in the upper valley.")

			i427Await(t, rig.keywordCycle(), "first cycle")
			if got := kw.calls(); len(got) != 1 || !strings.Contains(got[0], "glacier survey") {
				t.Fatalf("first cycle sent %d keyword call(s) %q, want one of the picked text", len(got), got)
			}
			if kws, stamped := rig.storedKeywords(t, b.ID); len(kws) == 0 || !stamped {
				t.Fatalf("precondition: keywords=%q stamped=%v after the first cycle, want them stored", kws, stamped)
			}

			tc.write(t, rig, b)

			kws, stamped := rig.storedKeywords(t, b.ID)
			if tc.regenerate && (len(kws) != 0 || stamped) {
				t.Errorf("after the write the block still holds keywords %q (stamped=%v) of the text it replaced", kws, stamped)
			}
			if !tc.regenerate && len(kws) == 0 {
				t.Errorf("a write of the tags alone dropped the keywords")
			}

			rig.embed(t, b.ID)
			rig.endClaim(t, b.ID)
			i427Await(t, rig.keywordCycle(), "second cycle")
			got := kw.calls()[1:]
			switch {
			case tc.regenerate && (len(got) != 1 || !strings.Contains(got[0], tc.newText)):
				t.Errorf("the next cycle sent %d keyword call(s) %q, want one carrying %q", len(got), got, tc.newText)
			case !tc.regenerate && len(got) != 0:
				t.Errorf("the next cycle asked for keywords again (%d call(s)) although title and content are unchanged", len(got))
			}
		})
	}
}

// TestDreamKeywordsOfAReplacedTextAreNotStored: the title or the content is
// replaced while the model generates the keywords. The cycle may use them for
// the text it picked, but it must not store them on the replacement: the next
// dream of the block generates keywords of the text it holds.
func TestDreamKeywordsOfAReplacedTextAreNotStored(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, tc := range []struct {
		name    string
		data    store.UpdateBlockData
		newText string
	}{
		{"content replaced", store.UpdateBlockData{Content: str("Now about the tidal power plant in the north bay.")}, "tidal power plant"},
		{"title replaced", store.UpdateBlockData{Title: str("Harbour crane maintenance")}, "Harbour crane maintenance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig, kw := newI452KeywordRig(t)
			b := rig.block(t, "Glacier notes", "Plans for the glacier survey in the upper valley.")

			entered, release := kw.holdNext()
			done := rig.keywordCycle()
			select {
			case p := <-entered:
				if !strings.Contains(p, "glacier survey") {
					t.Fatalf("the keyword call was sent %q, want the picked text", p)
				}
			case err := <-done:
				t.Fatalf("the cycle ended before its keyword call: %v", err)
			case <-time.After(i427Wait):
				t.Fatalf("the keyword call did not begin within %s", i427Wait)
			}
			rig.update(t, rig.pool, b.ID, tc.data)
			close(release)
			i427Await(t, done, "first cycle")

			if kws, stamped := rig.storedKeywords(t, b.ID); len(kws) != 0 || stamped {
				t.Errorf("keywords %q (stamped=%v) generated from the replaced text were stored on the block", kws, stamped)
			}

			rig.embed(t, b.ID)
			rig.endClaim(t, b.ID)
			i427Await(t, rig.keywordCycle(), "second cycle")
			got := kw.calls()[1:]
			if len(got) != 1 || !strings.Contains(got[0], tc.newText) {
				t.Errorf("the next cycle sent %d keyword call(s) %q, want one carrying %q", len(got), got, tc.newText)
			}
			if kws, _ := rig.storedKeywords(t, b.ID); len(kws) == 0 {
				t.Errorf("the keywords of the current text were not stored")
			}
		})
	}
}

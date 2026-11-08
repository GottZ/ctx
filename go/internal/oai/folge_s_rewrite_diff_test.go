package oai

// The differential gates of the FOLGE-S rewrite walk (RC-D S2, cluster C8)
// beyond the two goldens: rewriteChunkInto and rewriteChat against the map
// rewrite of the base (rewrite_base_test.go) — the client bytes, skip,
// runes, usage, prompt_n, the role carry after the chunk and the error
// text — over the golden corpora and, as fuzz targets, over any bytes.

import (
	"bytes"
	"fmt"
	"testing"
)

func folgeSChunkSame(t *testing.T, chunks [][]byte, alias, lane string, reasoningOff, includeUsage bool) {
	t.Helper()
	gr, wr := &roleCarry{}, &roleCarry{}
	var dst []byte
	for i, c := range chunks {
		got, gerr := rewriteChunkInto(dst, c, "chatcmpl-<x>&", 1700000000, alias, lane, reasoningOff, includeUsage, gr)
		want, werr := baseRewriteChunk(c, "chatcmpl-<x>&", 1700000000, alias, lane, reasoningOff, includeUsage, wr)
		if got.out != nil {
			dst = got.out
		}
		var wantOut []byte
		if werr == nil && !want.skip {
			wantOut = append(append(append(wantOut, "data: "...), want.out...), "\n\n"...)
		}
		ge, we := fmt.Sprint(gerr), fmt.Sprint(werr)
		g := fmt.Sprintf("out=%q skip=%v runes=%d usage=%v prompt_n=%d err=%s roles=%v", got.out, got.skip, got.runes, got.usage, got.promptN, ge, *gr)
		w := fmt.Sprintf("out=%q skip=%v runes=%d usage=%v prompt_n=%d err=%s roles=%v", wantOut, want.skip, want.runes, want.usage, want.promptN, we, *wr)
		if gerr != nil {
			g = "err=" + ge
		}
		if werr != nil {
			w = "err=" + we
		}
		if g != w {
			t.Fatalf("chunk %d %q (off=%v usage=%v):\n got %s\nbase %s", i, c, reasoningOff, includeUsage, g, w)
		}
	}
}

func folgeSChatSame(t *testing.T, upstream []byte, lane string, reasoningOff bool) {
	t.Helper()
	got, gerr := rewriteChat(upstream, "chatcmpl-<x>&", 1700000000, "alias- ", lane, reasoningOff)
	want, werr := baseRewriteChat(upstream, "chatcmpl-<x>&", 1700000000, "alias- ", lane, reasoningOff)
	if !bytes.Equal(got, want) || fmt.Sprint(gerr) != fmt.Sprint(werr) {
		t.Fatalf("%q (off=%v):\n got %q err=%v\nbase %q err=%v", upstream, reasoningOff, got, gerr, want, werr)
	}
	if gerr == nil && cap(got) == len(got) {
		t.Fatalf("%q: the body has no room for the route's newline (cap %d = len)", upstream, cap(got))
	}
}

// TestRewriteChunkIntoIsTheBase runs both corpora through the stream walk.
func TestRewriteChunkIntoIsTheBase(t *testing.T) {
	for _, s := range folgeSTraceCorpus() {
		var cs [][]byte
		for _, c := range s.chunks {
			cs = append(cs, []byte(c))
		}
		for _, off := range []bool{false, true} {
			for _, inc := range []bool{false, true} {
				folgeSChunkSame(t, cs, s.alias, s.lane, off, inc)
			}
		}
	}
	for _, c := range folgeSChatCorpus() {
		for _, inc := range []bool{false, true} {
			folgeSChunkSame(t, [][]byte{[]byte(c.upstream)}, "chat", c.lane, c.reasoningOff, inc)
		}
	}
}

// TestRewriteChatIsTheBase runs both corpora through the non-stream walk.
func TestRewriteChatIsTheBase(t *testing.T) {
	for _, c := range folgeSChatCorpus() {
		folgeSChatSame(t, []byte(c.upstream), c.lane, c.reasoningOff)
	}
	for _, s := range folgeSTraceCorpus() {
		for _, c := range s.chunks {
			folgeSChatSame(t, []byte(c), s.lane, s.reasoningOff)
		}
	}
}

func folgeSFuzzSeeds(f *testing.F) {
	for _, s := range folgeSTraceCorpus() {
		for i, c := range s.chunks {
			next := ""
			if i+1 < len(s.chunks) {
				next = s.chunks[i+1]
			}
			f.Add([]byte(c), []byte(next), s.reasoningOff, s.includeUsage)
		}
	}
	for _, c := range folgeSChatCorpus() {
		f.Add([]byte(c.upstream), []byte(nil), c.reasoningOff, true)
	}
}

func FuzzRewriteChunkIntoIsTheBase(f *testing.F) {
	folgeSFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, a, b []byte, reasoningOff, includeUsage bool) {
		folgeSChunkSame(t, [][]byte{a, b}, "chat", "", reasoningOff, includeUsage)
	})
}

func FuzzRewriteChatIsTheBase(f *testing.F) {
	folgeSFuzzSeeds(f)
	f.Fuzz(func(t *testing.T, a, _ []byte, reasoningOff, _ bool) {
		folgeSChatSame(t, a, "bulk", reasoningOff)
	})
}

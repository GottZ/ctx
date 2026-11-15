package llm

import (
	"strings"
	"testing"
)

// TestSynthesize_UntrustedOriginReachesResponse (issue #280): the source list
// of the answer carries each source's untrusted_origin record, and the prompt
// does not render it — the framing follows the block's current type.
func TestSynthesize_UntrustedOriginReachesResponse(t *testing.T) {
	rec := newPromptRecorder(t)
	sources := citationSources(3, 0.9)
	sources[1].UntrustedOrigin = "tool-evidence"
	res := runCitationSynthesize(t, rec, 32768, sources)

	if len(res.Sources) != 3 {
		t.Fatalf("len(sources) = %d, want 3", len(res.Sources))
	}
	for _, s := range res.Sources {
		want := ""
		if s.ID == "id2" {
			want = "tool-evidence"
		}
		if s.UntrustedOrigin != want {
			t.Errorf("source %s untrusted_origin = %q, want %q", s.ID, s.UntrustedOrigin, want)
		}
	}
	if p := rec.lastUser(); strings.Contains(p, "tool-evidence") || strings.Contains(p, `trust="untrusted"`) {
		t.Errorf("the prompt renders the record, or the trust class of a source whose type carries none:\n%s", p)
	}
}

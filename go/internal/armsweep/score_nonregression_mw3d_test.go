package armsweep_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// scoreBodyDigestBeforeMW3D is the sha256 of the `score` report body over the
// synthetic two-dump input, MEASURED ON THE CODE BEFORE wave M-W3d (base
// 15b2c1a6). It is pinned here rather than recomputed because the wave added a
// stamp field (DumpStamp.EfSearch) and a whole subcommand: the one thing that
// must not have happened is a shifted byte in the report an existing dump
// scores to.
const scoreBodyDigestBeforeMW3D = "36e233b3ab8af1b8c000a5bef6e1fea1180befe14ddce6933f6a2c8837a21a19"

// TestScoreBodyUnchangedByMW3D is the non-regression gate of the wave.
func TestScoreBodyUnchangedByMW3D(t *testing.T) {
	body := mustScore(t, synthInput(t, true))
	b, err := armsweep.MarshalBody(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := goldset.SHA256Hex(string(b)); got != scoreBodyDigestBeforeMW3D {
		t.Fatalf("score report body digest = %s, want %s (the pre-wave bytes)", got, scoreBodyDigestBeforeMW3D)
	}
}

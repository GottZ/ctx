package fedhop

// Gates of F-W3 on the monotony rules (design/04 §1.2, §4.1):
// MergeSensitivity is the rank maximum (a header only raises; base "" or
// unknown ⇒ credentials; header "" ⇒ the base), MergeClass the lower class
// (a header only lowers; lease "" or unknown ⇒ background; header "" ⇒ the
// lease). The urgency order is two-level — interactive and external are
// urgent, background and external_bulk bulk — because the order between
// in-house and external classes is policy (A-E2 dispatch.external_order),
// not part of the hop contract.
//
//	go test -count=1 -run 'TestMerge|TestVocabulary' ./internal/fedhop/

import (
	"slices"
	"testing"
)

func TestMergeSensitivity(t *testing.T) {
	for _, c := range []struct{ base, hdr, want string }{
		{"public", "", "public"},
		{"internal", "", "internal"},
		{"personal", "", "personal"},
		{"credentials", "", "credentials"},
		{"", "", "credentials"},
		{"bogus", "", "credentials"},
		{"public", "personal", "personal"},
		{"personal", "public", "personal"},
		{"internal", "internal", "internal"},
		{"public", "credentials", "credentials"},
		{"credentials", "public", "credentials"},
		{"public", "bogus", "credentials"},
		{"", "public", "credentials"},
		{"PUBLIC", "public", "credentials"},
	} {
		if got := MergeSensitivity(c.base, c.hdr); got != c.want {
			t.Errorf("MergeSensitivity(%q, %q) = %q, want %q", c.base, c.hdr, got, c.want)
		}
	}
	// Never below either side, over the whole vocabulary.
	all := append(SensitivityVocabulary(), "", "bogus")
	for _, b := range all {
		for _, h := range all {
			got := MergeSensitivity(b, h)
			if sensitivityRank(got) < sensitivityRank(b) || (h != "" && sensitivityRank(got) < sensitivityRank(h)) {
				t.Errorf("MergeSensitivity(%q, %q) = %q is below an input", b, h, got)
			}
			if !knownSensitivity(got) {
				t.Errorf("MergeSensitivity(%q, %q) = %q is outside the vocabulary", b, h, got)
			}
		}
	}
}

func TestMergeClass(t *testing.T) {
	for _, c := range []struct{ lease, hdr, want string }{
		{"interactive", "", "interactive"},
		{"external", "", "external"},
		{"background", "", "background"},
		{"external_bulk", "", "external_bulk"},
		{"", "", "background"},
		{"bogus", "", "background"},
		{"interactive", "background", "background"},
		{"interactive", "external_bulk", "external_bulk"},
		{"background", "interactive", "background"},
		{"external_bulk", "external", "external_bulk"},
		{"interactive", "external", "interactive"},
		{"external", "interactive", "external"},
		{"background", "external_bulk", "background"},
		{"interactive", "bogus", "background"},
		{"", "interactive", "background"},
	} {
		if got := MergeClass(c.lease, c.hdr); got != c.want {
			t.Errorf("MergeClass(%q, %q) = %q, want %q", c.lease, c.hdr, got, c.want)
		}
	}
	all := append(ClassVocabulary(), "", "bogus")
	for _, l := range all {
		for _, h := range all {
			got := MergeClass(l, h)
			if classRank(got) > classRank(normalizeClass(l)) || (h != "" && classRank(got) > classRank(normalizeClass(h))) {
				t.Errorf("MergeClass(%q, %q) = %q is above an input", l, h, got)
			}
			if !knownClass(got) {
				t.Errorf("MergeClass(%q, %q) = %q is outside the vocabulary", l, h, got)
			}
		}
	}
}

func TestVocabulary(t *testing.T) {
	if got, want := SensitivityVocabulary(), []string{"public", "internal", "personal", "credentials"}; !slices.Equal(got, want) {
		t.Errorf("SensitivityVocabulary = %v, want %v (rank order)", got, want)
	}
	if got, want := ClassVocabulary(), []string{"interactive", "external", "background", "external_bulk"}; !slices.Equal(got, want) {
		t.Errorf("ClassVocabulary = %v, want %v", got, want)
	}
	// The results are copies: a caller cannot change the tables.
	SensitivityVocabulary()[0] = "x"
	ClassVocabulary()[0] = "x"
	if !knownSensitivity("public") || !knownClass("interactive") {
		t.Error("a caller changed a vocabulary table through its copy")
	}
}

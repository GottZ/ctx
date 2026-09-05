package config

import (
	"reflect"
	"strings"
	"testing"
)

// The parse tag has exactly two states, and buildEntry (registry.go:180) is
// where that is decided: no tag = tolerant (a malformed value WARNs and keeps
// the default — TestFromSourcesSafeMalformedWarnsAndDefaults), parse:"strict"
// = boot abort (TestRegistryStrictSet pins the members). A third value used to
// pass the tag check and then vanish, because entry has no field to store it
// in: registry.go:25 keeps Strict bool alone, and :193 sets it from
// parse == "strict". A field tagged with that third value was therefore
// classified tolerant while its tag claimed a class of its own — no field in
// the tree ever carried it. The vocabulary is closed now, and this file is the
// lock: a tag value the registry cannot store must not build.
//
// Vehicle: synthetic structs through the REAL buildRegistry (same shape as
// synthreg_test.go:62-67), so tag parsing, class validation and the parser
// dispatch are the production ones and no live key is touched.

type parseTagUnknownGroup struct {
	X string `key:"legacy.parsetag_unknown" env:"-" default:"private" mut:"hot" parse:"safe" tenancy:"global-only"`
}

type parseTagStrictGroup struct {
	X string `key:"legacy.parsetag_strict" env:"-" default:"private" mut:"hot" parse:"strict" tenancy:"global-only"`
}

type parseTagAbsentGroup struct {
	X string `key:"legacy.parsetag_absent" env:"-" default:"private" mut:"hot" tenancy:"global-only"`
}

// TestParseTagVocabularyIsClosed is the wave gate: an unknown parse value must
// stop the registry build with a named error instead of being silently folded
// into the tolerant class.
func TestParseTagVocabularyIsClosed(t *testing.T) {
	_, err := buildRegistry(reflect.TypeOf(parseTagUnknownGroup{}))
	if err == nil {
		t.Fatal("buildRegistry accepted an unknown parse tag value — a class the entry cannot store must not build")
	}
	if want := `invalid parse tag "safe"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

// TestParseTagStrictStillBuilds is the positive control: the surviving tag
// value keeps building and keeps setting the boot-abort class.
func TestParseTagStrictStillBuilds(t *testing.T) {
	built, err := buildRegistry(reflect.TypeOf(parseTagStrictGroup{}))
	if err != nil {
		t.Fatalf(`buildRegistry over parse:"strict": %v — strict must stay valid vocabulary`, err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d entries, want 1", len(built))
	}
	if !built[0].Strict {
		t.Error(`entry.Strict = false for parse:"strict", want true — a malformed value must stay a boot abort`)
	}
}

// TestParseTagAbsentIsTolerant pins the other half of the pair: no tag is a
// class of its own, not an error, and it does not abort the boot.
func TestParseTagAbsentIsTolerant(t *testing.T) {
	built, err := buildRegistry(reflect.TypeOf(parseTagAbsentGroup{}))
	if err != nil {
		t.Fatalf("buildRegistry over an untagged field: %v — no parse tag is the default class", err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d entries, want 1", len(built))
	}
	if built[0].Strict {
		t.Error("entry.Strict = true without a parse tag, want false — the untagged class WARNs and keeps the default")
	}
}

package armsweep

import (
	"strings"
	"testing"
)

// M-W2 review finding #2 — gate (l) says "Override nur explizit UND im Report
// ausgewiesen". buildEnv took InstanceKind and ShadowTypes from StampA alone
// while OR-ing AllowLiveInstance across both, and the markdown section rendered
// only on those A-only fields. A pair with the shadow dump as B therefore
// carried AllowLiveInstance=true and printed nothing at all: the override was
// set and invisible, which is the one state the gate exists to prevent.
//
// F-32 ("all dumps of a campaign against ONE instance") would forbid the mixed
// pair — but `score` does not gate that rule (it is M-W3d's job), so this line
// is the last place a live-instance dump can still show up.

// mw2StampPair builds the pair the review's probe used: A is an ordinary base
// dump, B the shadow dump taken against a live instance under the override.
func mw2StampPair() (DumpStamp, DumpStamp) {
	a := DumpStamp{RunID: "A", BaseURL: "http://a", MigrationsMax: 142}
	b := DumpStamp{RunID: "B", BaseURL: "http://a", MigrationsMax: 142,
		InstanceKind:      InstanceKindLive,
		ShadowTypes:       []string{"mw2-shadow"},
		AllowLiveInstance: true,
	}
	return a, b
}

// TestMW2EnvCarriesShadowProvenanceFromEitherDump pins the stamp side.
//
// RED before the fix: `env.InstanceKind = ""`, `env.ShadowTypes = []`.
func TestMW2EnvCarriesShadowProvenanceFromEitherDump(t *testing.T) {
	a, b := mw2StampPair()

	for _, tc := range []struct {
		name           string
		in             ScoreInput
		wantKindHas    []string
		wantShadowHas  []string
		wantAllowLive  bool
		wantSectionSet bool
	}{
		{
			name:           "shadow dump is B",
			in:             ScoreInput{StampA: a, StampB: &b},
			wantKindHas:    []string{InstanceKindLive},
			wantShadowHas:  []string{"mw2-shadow"},
			wantAllowLive:  true,
			wantSectionSet: true,
		},
		{
			name:           "shadow dump is A",
			in:             ScoreInput{StampA: b, StampB: &a},
			wantKindHas:    []string{InstanceKindLive},
			wantShadowHas:  []string{"mw2-shadow"},
			wantAllowLive:  true,
			wantSectionSet: true,
		},
		{
			name:           "neither dump is a shadow dump",
			in:             ScoreInput{StampA: a, StampB: &a},
			wantSectionSet: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := buildEnv(tc.in)
			for _, want := range tc.wantKindHas {
				if !strings.Contains(env.InstanceKind, want) {
					t.Errorf("env.InstanceKind = %q, want it to name %q", env.InstanceKind, want)
				}
			}
			for _, want := range tc.wantShadowHas {
				if !contains(env.ShadowTypes, want) {
					t.Errorf("env.ShadowTypes = %v, want it to carry %q", env.ShadowTypes, want)
				}
			}
			if env.AllowLiveInstance != tc.wantAllowLive {
				t.Errorf("env.AllowLiveInstance = %v, want %v", env.AllowLiveInstance, tc.wantAllowLive)
			}
			if !tc.wantSectionSet {
				if env.InstanceKind != "" || len(env.ShadowTypes) > 0 {
					t.Errorf("an ordinary pair invented shadow provenance: kind=%q types=%v",
						env.InstanceKind, env.ShadowTypes)
				}
			}
		})
	}
}

// TestMW2MarkdownShowsTheOverride pins the RENDER side separately: the stamp
// may carry the truth and the report still hide it, which is exactly the shape
// the review found.
//
// RED before the fix: no "Instanz-Art", no "Schatten-Typen", no
// "-allow-live-instance" line for the B-dump pair.
func TestMW2MarkdownShowsTheOverride(t *testing.T) {
	a, b := mw2StampPair()
	md := RenderMarkdown("2026-08-27T00:00:00Z", ReportBody{Env: buildEnv(ScoreInput{StampA: a, StampB: &b})})

	for _, want := range []string{"Instanz-Art", "Schatten-Typen", "-allow-live-instance", InstanceKindLive} {
		if !strings.Contains(md, want) {
			t.Errorf("report does not mention %q:\n%s", want, md)
		}
	}

	// A pair without any shadow dump keeps the section out — the line is a
	// statement about a measurement that happened, not boilerplate.
	plain := RenderMarkdown("2026-08-27T00:00:00Z", ReportBody{Env: buildEnv(ScoreInput{StampA: a, StampB: &a})})
	if strings.Contains(plain, "Instanz-Art") {
		t.Errorf("an ordinary report carries the shadow provenance section:\n%s", plain)
	}
}

// TestMW2EnvNamesBothInstanceKinds pins the mixed case the review asked for
// explicitly: with two DIFFERENT kinds the report names both instead of
// silently picking one — the reader must see that the pair is incongruent, and
// `compare` (M-W3d) is the gate that will later refuse it.
func TestMW2EnvNamesBothInstanceKinds(t *testing.T) {
	a := DumpStamp{RunID: "A", InstanceKind: InstanceKindMeasureCopy, ShadowTypes: []string{"mw2-a"}}
	b := DumpStamp{RunID: "B", InstanceKind: InstanceKindLive, ShadowTypes: []string{"mw2-b"}}
	env := buildEnv(ScoreInput{StampA: a, StampB: &b})

	for _, want := range []string{InstanceKindMeasureCopy, InstanceKindLive} {
		if !strings.Contains(env.InstanceKind, want) {
			t.Errorf("env.InstanceKind = %q, want it to name %q", env.InstanceKind, want)
		}
	}
	for _, want := range []string{"mw2-a", "mw2-b"} {
		if !contains(env.ShadowTypes, want) {
			t.Errorf("env.ShadowTypes = %v, want it to carry %q", env.ShadowTypes, want)
		}
	}
}

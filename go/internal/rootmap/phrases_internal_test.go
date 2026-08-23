// Internal gates of the phrases tables (issue #34). These assert against
// package-private structure — the two tables and the byte reserves they have to
// fit into — which the external gates in lang_test.go cannot see.
package rootmap

import (
	"fmt"
	"reflect"
	"testing"
)

func TestMapLanguagePrimarySubtag(t *testing.T) {
	for in, want := range map[string]string{
		"":           "",
		"  ":         "",
		"de":         "de",
		"DE":         "de",
		"  de-DE  ":  "de",
		"de-CH":      "de",
		"en":         "en",
		"en-GB":      "en",
		"fr":         "fr",
		"zh-hans-cn": "zh",
	} {
		if got := mapLanguage(in); got != want {
			t.Errorf("mapLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPhrasesForTwoTables is the policy itself: exactly two tables, the empty
// tag and German on the frozen one, everything else on the English one.
func TestPhrasesForTwoTables(t *testing.T) {
	for _, tag := range []string{"", "  ", "de", "DE", "de-CH", "de-AT"} {
		if !reflect.DeepEqual(phrasesFor(tag), phrasesDE) {
			t.Errorf("%q did not select the frozen German table", tag)
		}
	}
	for _, tag := range []string{"en", "en-GB", "fr", "tr", "ja", "xx-yy"} {
		if !reflect.DeepEqual(phrasesFor(tag), phrasesEN) {
			t.Errorf("%q did not select the English table", tag)
		}
	}
}

// TestPhrasesTablesComplete walks both tables by reflection: a field left at the
// empty string, or a freeze reason present in one table and missing in the
// other, would silently DROP a line from one language's map. There is no other
// place that failure becomes visible.
func TestPhrasesTablesComplete(t *testing.T) {
	tables := map[string]phrases{"de": phrasesDE, "en": phrasesEN}
	for name, p := range tables {
		v := reflect.ValueOf(p)
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.Type.Kind() != reflect.String {
				continue
			}
			if v.Field(i).String() == "" {
				t.Errorf("%s: phrases.%s is unset — that language loses the line it writes", name, f.Name)
			}
		}
	}
	for reason := range phrasesDE.freeze {
		if _, ok := phrasesEN.freeze[reason]; !ok {
			t.Errorf("freeze reason %q has no English clause", reason)
		}
	}
	for reason := range phrasesEN.freeze {
		if _, ok := phrasesDE.freeze[reason]; !ok {
			t.Errorf("freeze reason %q has no German clause", reason)
		}
	}
}

// TestSuperCutFitsReserve is the constraint superCutReserve encodes but cannot
// enforce: the section stops appending while these bytes are still free, so a
// cut line that does NOT fit is written past the room it reserved — the section
// would omit groups without saying so, which is the exact failure the reserve
// exists to prevent.
//
// Measured against a five-digit count: root_map.super_max_nodes ships at 20000,
// so five digits is the widest number the line can carry.
func TestSuperCutFitsReserve(t *testing.T) {
	for name, p := range map[string]phrases{"de": phrasesDE, "en": phrasesEN} {
		line := fmt.Sprintf(p.superCut, num(99999, p))
		if len(line) > superCutReserve {
			t.Errorf("%s: cut line %q is %d B against a %d B reserve", name, line, len(line), superCutReserve)
		}
	}
}

// TestFooterFitsDefaultReserve is the same reflex one level up: Render refuses a
// map whose footer outgrows FooterReserveBytes, so an overlong footer phrase
// takes the artefact away rather than degrading it. 512 is the shipped default
// (root_map.footer_reserve_bytes).
func TestFooterFitsDefaultReserve(t *testing.T) {
	const defaultReserve = 512
	in := Input{SmallClusterMax: 2}
	cov := Coverage{SmallClusterN: 99999, SmallClusterSize: 99999,
		CappedClusterN: 99999, CappedBlocks: 99999}
	for name, p := range map[string]phrases{"de": phrasesDE, "en": phrasesEN} {
		if n := len(renderFooter(in, cov, p)); n >= defaultReserve {
			t.Errorf("%s: footer is %d B against the %d B default reserve", name, n, defaultReserve)
		}
	}
}

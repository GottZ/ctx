package oai

// Hardening 5, B-1: the bound past which the parse leaves the model name
// undecoded, at its edge, and its source — the alias name CHECK of the
// migrations (the registry loads aliases from those rows only).

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestModelPastTheBoundIsNotDecoded(t *testing.T) {
	esc := strings.Repeat(`\u0061`, aliasNameMaxBytes)
	if len(esc) != modelRawMax {
		t.Fatalf("the longest alias name, every character escaped, is %d raw bytes, the bound %d", len(esc), modelRawMax)
	}
	for _, tc := range []struct {
		name, raw, model string
		overlong         bool
	}{
		{"the longest name, escaped, at the bound", esc, strings.Repeat("a", aliasNameMaxBytes), false},
		{"one byte past the bound", esc + "a", "", true},
		{"invalid UTF-8 at the bound", strings.Repeat("\xff", modelRawMax), strings.Repeat("\uFFFD", modelRawMax), false},
		{"invalid UTF-8 one past the bound", strings.Repeat("\xff", modelRawMax+1), "", true},
		{"short", "chat", "chat", false},
	} {
		req, perr := parseTopLevel([]byte(`{"model":"` + tc.raw + `","messages":[]}`))
		if perr != nil {
			t.Fatalf("%s: %+v", tc.name, perr)
		}
		if req.model != tc.model || req.modelOverlong != tc.overlong {
			t.Errorf("%s (%d raw bytes): model %d bytes, overlong %v; want %d bytes, overlong %v", tc.name, len(tc.raw), len(req.model),
				req.modelOverlong, len(tc.model), tc.overlong)
		}
	}
	// Past the bound is no type check: a non-string is invalid_type as before.
	for _, body := range []string{`{"model":1,"messages":[]}`, `{"model":["` + esc + `a"],"messages":[]}`} {
		if _, perr := parseTopLevel([]byte(body)); perr == nil || perr.code != CodeInvalidType || perr.param != "model" {
			t.Errorf("%.40s…: %+v, want invalid_type model", body, perr)
		}
	}
}

// TestAliasNameBoundIsTheMigrationCheck pins aliasNameMaxBytes to the alias
// name CHECK of the migrations (the last one that defines it): 1 + the
// upper count of ^[A-Za-z0-9][A-Za-z0-9._:/-]{0,N}$ — ASCII only, so one
// character is one byte.
func TestAliasNameBoundIsTheMigrationCheck(t *testing.T) {
	files, err := filepath.Glob("../../migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v, %d files", err, len(files))
	}
	re := regexp.MustCompile(`(?s)CONSTRAINT ck_model_aliases_name\s+CHECK \(name ~ '\^\[A-Za-z0-9\]\[A-Za-z0-9\._:/-\]\{0,(\d+)\}\$'\)`)
	last := -1
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(b, -1) {
			last, _ = strconv.Atoi(string(m[1]))
		}
	}
	if last < 0 {
		t.Fatal("no alias name CHECK found in the migrations")
	}
	if 1+last != aliasNameMaxBytes {
		t.Errorf("the migrations allow alias names of %d characters, aliasNameMaxBytes is %d", 1+last, aliasNameMaxBytes)
	}
}

// distillsource_test.go — the contract's own gates. The package carries no
// behaviour, so there is exactly one thing to prove here: the error class
// STRINGS, because they are the values a caller writes into a journal column
// that has a CHECK constraint behind it.
//
// Source: https://github.com/GottZ/ctx
package distillsource_test

import (
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/distillsource"
)

// TestErrorClassStrings pins the four class strings. Two of them are members of
// dr_error_class_known (migrations/135_distill_run.sql:155-158) and are written
// through unchanged; the other two are NOT and must be mapped by the arm. A
// rename here that is not mirrored in a migration turns into a CHECK violation
// at runtime, which is why the strings are asserted rather than trusted.
func TestErrorClassStrings(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{distillsource.ErrSourceUnavailable, "source_unavailable"},
		{distillsource.ErrSchemaUntrusted, "schema_untrusted"},
		{distillsource.ErrQueryFailed, "query_failed"},
		{distillsource.ErrNoActiveRows, "no_active_rows"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("class string = %q, want %q", got, tc.want)
		}
	}
}

// TestErrorClassesAreDistinct proves the four sentinels do not collapse into
// each other. errors.Is on a wrapped chain is the only way a caller tells them
// apart, so two classes that match each other would make the taxonomy
// decorative.
func TestErrorClassesAreDistinct(t *testing.T) {
	all := []error{
		distillsource.ErrSourceUnavailable,
		distillsource.ErrSchemaUntrusted,
		distillsource.ErrQueryFailed,
		distillsource.ErrNoActiveRows,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("class %q matches %q", a, b)
			}
		}
	}
}

package handler

import (
	"strings"
	"testing"
)

// The batch shape (issue #211): no item, the cap, and the cap's 0 as the
// switch that refuses every batch by name.
func TestStoreBatchShapeReject(t *testing.T) {
	for _, c := range []struct {
		n, max int
		code   string
		text   string
	}{
		{0, 100, "missing_fields", "items: a batch needs at least one item"},
		{1, 100, "", ""},
		{100, 100, "", ""},
		{101, 100, "size_cap", "items: 101 items exceed the batch cap of 100"},
		{1, 0, "size_cap", "items: 1 items exceed the batch cap of 0 (writes.batch_max_items; 0 switches batches off)"},
	} {
		rej := storeBatchShapeReject(c.n, c.max)
		switch {
		case c.code == "" && rej != nil:
			t.Errorf("n=%d max=%d: refused %+v", c.n, c.max, rej)
		case c.code != "" && (rej == nil || rej.Code != c.code || !strings.HasPrefix(rej.Msg, c.text)):
			t.Errorf("n=%d max=%d: %+v; want %s %q", c.n, c.max, rej, c.code, c.text)
		}
	}
}

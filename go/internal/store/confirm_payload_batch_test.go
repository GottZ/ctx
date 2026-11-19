package store

import (
	"strings"
	"testing"
)

// batchWrite is a staged store_batch card of two items (issue #211).
func batchWrite() CanonicalWrite {
	a, b := baseWrite(), baseWrite()
	b.Title = "Ein zweiter Titel"
	return CanonicalWrite{Op: OpStoreBatch, Scope: "private", Items: []CanonicalWrite{a, b}}
}

// The batch hash binds every item, their order and each item's resolved
// sensitivity, and is stable under the orders the single write is stable
// under (tags, metadata construction) inside each item.
func TestPayloadHashBindsTheWholeBatch(t *testing.T) {
	h, canonical, err := batchWrite().PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"items":[`) {
		t.Fatalf("the batch canonicalizes without its items: %s", canonical)
	}

	reordered := batchWrite()
	reordered.Items[1].Tags = []string{"mike", "zulu", "alpha"}
	if h2, _, _ := reordered.PayloadHash(); h2 != h {
		t.Fatalf("tag order inside an item changed the batch hash")
	}
	if reordered.Items[1].Tags[0] != "mike" {
		t.Fatalf("canonicalization sorted the caller's item tags in place: %v", reordered.Items[1].Tags)
	}

	for name, mutate := range map[string]func(*CanonicalWrite){
		"one byte of an item's content": func(c *CanonicalWrite) { c.Items[1].Content += "." },
		"an item's scope":               func(c *CanonicalWrite) { c.Items[0].Scope = "shared" },
		"an item's sensitivity":         func(c *CanonicalWrite) { c.Items[0].SensitivityManual = true },
		"an item's supersedes":          func(c *CanonicalWrite) { c.Items[1].Supersedes = "0198cafe-0000-7000-8000-00000000beef" },
		"the item order":                func(c *CanonicalWrite) { c.Items[0], c.Items[1] = c.Items[1], c.Items[0] },
		"an item dropped":               func(c *CanonicalWrite) { c.Items = c.Items[:1] },
	} {
		w := batchWrite()
		mutate(&w)
		if h2, _, _ := w.PayloadHash(); h2 == h {
			t.Errorf("%s does not change the batch hash", name)
		}
	}
}

// A write without items canonicalizes without the key: every hash a client
// staged before the field existed stays confirmable.
func TestCanonicalOmitsAbsentItems(t *testing.T) {
	_, canonical, err := baseWrite().PayloadHash()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "items") {
		t.Fatalf("a single write canonicalizes with an items key: %s", canonical)
	}
}

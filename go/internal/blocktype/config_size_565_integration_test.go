//go:build integration

package blocktype_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestConfigSize565_Integration: the size cap measures a config as the
// database stores it, written compact — the same number for a request, for a
// merged config and for the jsonb text Reload reads (PR #567 review, R2-4).
//
// (1) The measure of every input equals the compact length of what jsonb
// renders for it — whitespace, escapes of printable characters, control
// characters, numbers in exponent notation.
// (2) A row whose jsonb text is over the cap and whose compact form is at it —
// stored by the production write, as any config at the cap is — loads.
//
// RED on 9c701044: (2) the Reload fails (`config exceeds 16384 bytes`), the
// text being measured by its length.
func TestConfigSize565_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	for _, in := range []string{
		`{"v":1}`,
		`{"v": 1, "classify": {"title_patterns": ["a", "b"], "priority": 3}}`,
		`{"x":["é\/€", "a\"b\\c\n\t\r\b\f", "\u0001\u001f", "😀  "]}`,
		`{"n":[1e-5, 1.5e2, 1E+2, 12.345e1, 0.50, -0.0, -1.25, 0e5, 0.001e2, 5e-324, 1.7976931348623157e308, 9007199254740993, 0.12345678901234567890123456789]}`,
		`{"v":1,"retrieval":{"policy":"damped","damping_factor":1e-5},"guard":{"threshold_review":0.9e0}}`,
	} {
		var text string
		if err := pool.QueryRow(ctx, `SELECT $1::jsonb::text`, in).Scan(&text); err != nil {
			t.Fatalf("render %s: %v", in, err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(text)); err != nil {
			t.Fatal(err)
		}
		if got := blocktype.ConfigSizeForTest([]byte(in)); got != compact.Len() {
			t.Errorf("measure of %s = %d, want %d (the database stores %s)", in, got, compact.Len(), compact.String())
		}
		if got := blocktype.ConfigSizeForTest([]byte(text)); got != compact.Len() {
			t.Errorf("measure of the jsonb text %s = %d, want %d", text, got, compact.Len())
		}
	}

	// (2) A config at the cap, compact, through the store write: the jsonb text
	// carries a blank after every ',' and ':' and is over the cap.
	cfg := atCap(t)
	if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
		Name: "size565", Scope: store.GlobalScope, Config: json.RawMessage(cfg),
	}, nil, ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	var textLen int
	if err := pool.QueryRow(ctx, `SELECT octet_length(config::text) FROM context_block_types WHERE name = 'size565'`).Scan(&textLen); err != nil {
		t.Fatal(err)
	}
	if textLen <= 16384 {
		t.Fatalf("fixture premise: stored text is %d bytes, want more than the cap", textLen)
	}
	reg := blocktype.NewRegistry()
	if err := reg.Reload(ctx, pool); err != nil {
		t.Fatalf("Reload over a row of %d text bytes, %d compact: %v", textLen, len(cfg), err)
	}
	if _, ok := reg.Snapshot().Resolve("size565"); !ok {
		t.Fatal("the row at the cap is not served")
	}
}

// atCap is a valid operator config of exactly 16384 bytes, compact: two
// classify arrays of 128-character entries, the last one cut to fit.
func atCap(t *testing.T) string {
	t.Helper()
	const limit = 16384
	var tp, sp []string
	render := func() string {
		s := `{"v":1,"classify":{"title_patterns":["` + strings.Join(tp, `","`) + `"]`
		if len(sp) > 0 {
			s += `,"source_prefixes":["` + strings.Join(sp, `","`) + `"]`
		}
		return s + `}}`
	}
	for i := 0; i < 64; i++ {
		tp = append(tp, fmt.Sprintf("t%02d", i)+strings.Repeat("x", 125))
	}
	for len(render()) < limit {
		sp = append(sp, fmt.Sprintf("s%02d", len(sp))+strings.Repeat("y", 125))
	}
	over := len(render()) - limit
	last := sp[len(sp)-1]
	sp[len(sp)-1] = last[:len(last)-over]
	if s := render(); len(s) != limit {
		t.Fatalf("atCap: %d bytes", len(s))
	}
	return render()
}

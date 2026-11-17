package events

import (
	"errors"
	"strings"
	"testing"
)

// TestIndexProbeStatement_ManagedDDLs: every DDL ensureConcurrentIndexValid
// takes is read by the probe, which keeps everything after the table name
// byte for byte (issue #312).
func TestIndexProbeStatement_ManagedDDLs(t *testing.T) {
	all := append(verifyNextIndexes(), struct{ name, ddl string }{migrationPendingIndexName, migrationPendingIndexDDL})
	if len(all) != 7 {
		t.Fatalf("managed runtime indexes = %d, want 7 (HNSW twin, five sisters, pending index)", len(all))
	}
	for _, ix := range all {
		got, err := indexProbeStatement(ix.name, ix.ddl, "p_i", "p")
		if err != nil {
			t.Errorf("%s: %v", ix.name, err)
			continue
		}
		_, tail, _ := strings.Cut(ix.ddl, "ON context_blocks")
		if want := "CREATE INDEX p_i ON p" + tail; got != want {
			t.Errorf("%s:\n got  %q\n want %q", ix.name, got, want)
		}
	}
}

func TestIndexProbeStatement_RefusesOtherShapes(t *testing.T) {
	for _, c := range []struct{ label, name, ddl string }{
		{"other name", "idx_a", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_b ON context_blocks (x)`},
		{"name prefix", "idx_a", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a_next ON context_blocks (x)`},
		{"other table", "idx_a", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON context_blocks_old (x)`},
		{"qualified table", "idx_a", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON public.context_blocks (x)`},
		{"not concurrent", "idx_a", `CREATE INDEX idx_a ON context_blocks (x)`},
		{"unique", "idx_a", `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON context_blocks (x)`},
		{"nothing after the table", "idx_a", `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_a ON context_blocks`},
	} {
		if got, err := indexProbeStatement(c.name, c.ddl, "p_i", "p"); !errors.Is(err, errIndexDefinitionUndetermined) {
			t.Errorf("%s: got %q, err %v, want errIndexDefinitionUndetermined", c.label, got, err)
		}
	}
}

func TestComparableIndexDefinition(t *testing.T) {
	for _, c := range []struct{ def, want string }{
		{`CREATE INDEX idx_x ON public.context_blocks USING btree (created_at) WHERE (NOT is_archived)`,
			`USING btree (created_at) WHERE (NOT is_archived)`},
		{`CREATE INDEX ctx_index_probe_ab_i ON public.ctx_index_probe_ab USING btree (created_at) WHERE (NOT is_archived)`,
			`USING btree (created_at) WHERE (NOT is_archived)`},
		{`CREATE UNIQUE INDEX idx_x ON public.context_blocks USING btree (id)`,
			`UNIQUE USING btree (id)`},
	} {
		got, err := comparableIndexDefinition(c.def)
		if err != nil || got != c.want {
			t.Errorf("%q: got %q, %v, want %q", c.def, got, err, c.want)
		}
	}
	for _, def := range []string{"", "CREATE INDEX idx_x ON public.context_blocks", "ALTER INDEX x"} {
		if got, err := comparableIndexDefinition(def); !errors.Is(err, errIndexDefinitionUndetermined) {
			t.Errorf("%q: got %q, %v, want errIndexDefinitionUndetermined", def, got, err)
		}
	}
}

// TestManagedRuntimeIndexNamesNeverServe pins the sweep of issue #312: the
// helper that may drop an index is called with runtime names only — the
// _next twins the cutover renames away and the pending index — never with a
// name an index carries while it serves (before the swap the canonical
// names, after it the _next twins' targets).
func TestManagedRuntimeIndexNamesNeverServe(t *testing.T) {
	serving := map[string]bool{}
	nextTwins := map[string]bool{}
	for _, r := range cutoverIndexRenames {
		if strings.HasSuffix(r[1], "_old") {
			serving[r[0]] = true // canonical → _old: serves before the swap
		} else {
			nextTwins[r[0]] = true // twin → canonical: serves after the swap
			serving[r[1]] = true
		}
	}
	managed := map[string]bool{migrationPendingIndexName: true}
	for _, ix := range verifyNextIndexes() {
		managed[ix.name] = true
		if !nextTwins[ix.name] {
			t.Errorf("%s is built by the verify gate but not renamed by the cutover", ix.name)
		}
	}
	for name := range managed {
		if serving[name] {
			t.Errorf("%s is managed by ensureConcurrentIndexValid and a serving name", name)
		}
	}
	for name := range nextTwins {
		if !managed[name] {
			t.Errorf("cutover renames %s, which the verify gate does not build", name)
		}
	}
}

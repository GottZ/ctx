package migrations

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// rowsFunctionSince is the migration that creates ctx_migration_rows (issue
// #399). From it on, a file that decides by the size of a table asks the
// function instead of copying the pg_class.reltuples query of 115, 140,
// 171-173, 175, 176, 182 and 184-187, which reads an estimate that can trail
// the table.
const rowsFunctionSince = 191

var (
	migrationVersion = regexp.MustCompile(`^(\d+)_.*\.sql$`)
	// sizeRead is what a migration would decide a table's size by itself:
	// the stored estimate and pages, the live-row counter, the file size.
	sizeRead = regexp.MustCompile(`(?i)\b(reltuples|relpages|n_live_tup|pg_stat_get_live_tuples|pg_relation_size|pg_total_relation_size|pg_table_size)\b`)
	// rowsFnHead opens a definition of ctx_migration_rows up to the tag of
	// its dollar-quoted body.
	rowsFnHead = regexp.MustCompile(`(?is)\bCREATE\s+(OR\s+REPLACE\s+)?FUNCTION\s+ctx_migration_rows\s*\(.*?\bAS\s+(\$[A-Za-z0-9_]*\$)`)
)

// sqlCode is src without its comments (-- to the end of the line, /* */):
// code keeps the single-quoted string literals (a size read inside dynamic
// SQL is a read), bare is the same text with their content blanked, byte for
// byte, so that a string cannot pass for a statement. Dollar-quoted bodies
// are lexed as code, as PL/pgSQL reads them. Not handled: a dollar-quoted
// string literal holding an apostrophe, and an E'' string with a
// backslash-escaped quote; either throws the string tracking off from there
// on. A file that needs one makes this lexer learn it.
func sqlCode(src string) (code, bare string) {
	var c, b strings.Builder
	both := func(ch byte, blank bool) {
		c.WriteByte(ch)
		if blank {
			b.WriteByte(' ')
		} else {
			b.WriteByte(ch)
		}
	}
	inString := false
	for i := 0; i < len(src); i++ {
		ch := src[i]
		switch {
		case inString:
			if ch == '\'' {
				if i+1 < len(src) && src[i+1] == '\'' {
					both(ch, true)
					both(src[i+1], true)
					i++
					continue
				}
				inString = false
				both(ch, false)
				continue
			}
			both(ch, true)
		case ch == '\'':
			inString = true
			both(ch, false)
		case ch == '-' && i+1 < len(src) && src[i+1] == '-':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				both('\n', false)
			}
		case ch == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return c.String(), b.String()
			}
			i += 2 + end + 1
		default:
			both(ch, false)
		}
	}
	return c.String(), b.String()
}

// withoutRowsFnDefinitions cuts every definition of ctx_migration_rows, from
// CREATE to the end of its dollar-quoted body, out of code; the definitions
// are found in bare, so neither a comment nor a string literal opens one.
func withoutRowsFnDefinitions(code, bare string) string {
	for {
		m := rowsFnHead.FindStringSubmatchIndex(bare)
		if m == nil {
			return code
		}
		tag := bare[m[4]:m[5]]
		end := len(bare)
		if close := strings.Index(bare[m[1]:], tag); close >= 0 {
			end = m[1] + close + len(tag)
		}
		code = code[:m[0]] + code[end:]
		bare = bare[:m[0]] + bare[end:]
	}
}

// checkSizeDecision refuses a file whose code decides a size itself — reads
// one of sizeRead outside a definition of ctx_migration_rows. A later
// CREATE OR REPLACE of the function may read them in its body, a file that
// calls the function for one index and copies a query for another may not.
func checkSizeDecision(src string) error {
	code, bare := sqlCode(src)
	if read := sizeRead.FindString(withoutRowsFnDefinitions(code, bare)); read != "" {
		return fmt.Errorf("reads %s: decide the size with ctx_migration_rows(<table>::regclass, <threshold>) "+
			"(docs/development.md, \"A migration with a row threshold\")", read)
	}
	return nil
}

// TestMigrationsDecideSizeWithTheRowsFunction holds every migration from
// rowsFunctionSince on to checkSizeDecision.
func TestMigrationsDecideSizeWithTheRowsFunction(t *testing.T) {
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		m := migrationVersion.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, _ := strconv.Atoi(m[1])
		if v < rowsFunctionSince {
			continue
		}
		src, err := FS.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if err := checkSizeDecision(string(src)); err != nil {
			t.Errorf("%s: %v", e.Name(), err)
		}
	}
	if checked == 0 {
		t.Fatalf("no migration from %d on: the file that creates ctx_migration_rows is missing", rowsFunctionSince)
	}
}

// TestSizeDecisionCheckRefusesTheCopiedQuery: the check refuses every frozen
// file that decides a size itself (it would have refused them had they come
// after 191) and every form of such a read a later file could take, and
// passes a file that asks the function, names the reads in comments only, or
// redefines the function.
func TestSizeDecisionCheckRefusesTheCopiedQuery(t *testing.T) {
	frozen := []string{
		"115_hnsw_ef_construction_canonical.sql", "140_trigram_gist_knn.sql",
		// 145 decides by the mass of two GIN indexes (pg_relation_size of the
		// indexes), not by rows; a later file that must decide so names its
		// exception in the check, with its reason.
		"145_partial_fts_gin.sql",
		"171_embed_pending_class_indexes.sql", "172_block_fk_lookup_indexes.sql", "173_embedding_stale.sql",
		"175_embed_excluded.sql", "176_embed_excluded_owner.sql", "182_guard_pending_type.sql",
		"184_access_token_fk_lookup_indexes.sql", "185_api_key_fk_lookup_indexes.sql",
		"186_blocks_created_at_not_null.sql", "187_derived_identity_index.sql",
	}
	// Every file before 191 whose code reads one of sizeRead is in the list:
	// the list is the inventory, not a sample. These files are shipped and
	// exempt; the check holds the files after them.
	var readers []string
	entries, err := FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		m := migrationVersion.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if v, _ := strconv.Atoi(m[1]); v >= rowsFunctionSince {
			continue
		}
		src, err := FS.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if checkSizeDecision(string(src)) != nil {
			readers = append(readers, e.Name())
		}
	}
	sort.Strings(readers)
	if strings.Join(readers, ",") != strings.Join(frozen, ",") {
		t.Errorf("files before %d that decide a size themselves:\n  %v\nwant\n  %v", rowsFunctionSince, readers, frozen)
	}

	const definition = `CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)
RETURNS bigint LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $fn$
BEGIN
    RETURN (SELECT greatest(reltuples, pg_stat_get_live_tuples(oid))::bigint FROM pg_class WHERE oid = p_rel);
END
$fn$;
`
	for name, src := range map[string]string{
		"asks the function": `DO $do$ DECLARE v_rows BIGINT; BEGIN
			v_rows := ctx_migration_rows('public.context_blocks', 500000);
			IF v_rows >= 500000 THEN RAISE WARNING 'out of band at % rows', v_rows; END IF; END $do$;`,
		"names the reads in comments only": "-- not pg_class.reltuples, relpages or n_live_tup\n/* pg_relation_size(oid) */ SELECT 1;",
		"redefines the function, reading inside its body": definition + "SELECT 1;",
	} {
		if err := checkSizeDecision(src); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, src := range map[string]string{
		"reads reltuples":                   `SELECT c.reltuples::BIGINT INTO v_rows FROM pg_class c WHERE c.relname = 'context_blocks';`,
		"reads reltuples in upper case":     `SELECT c.RELTUPLES::BIGINT INTO v_rows FROM pg_class c WHERE c.relname = 'context_blocks';`,
		"reads reltuples in mixed case":     `SELECT c.RelTuples INTO v_rows FROM pg_class c;`,
		"reads relpages":                    `SELECT relpages INTO v_pages FROM pg_class WHERE oid = 'context_blocks'::regclass;`,
		"reads n_live_tup":                  `SELECT n_live_tup INTO v_rows FROM pg_stat_user_tables WHERE relname = 'context_blocks';`,
		"reads N_LIVE_TUP":                  `SELECT N_LIVE_TUP INTO v_rows FROM pg_stat_user_tables WHERE relname = 'context_blocks';`,
		"reads pg_stat_get_live_tuples":     `v_rows := pg_stat_get_live_tuples('context_blocks'::regclass);`,
		"reads pg_relation_size":            `IF pg_relation_size('context_blocks') > 1073741824 THEN RETURN; END IF;`,
		"reads pg_total_relation_size":      `IF pg_total_relation_size('context_blocks') > 1073741824 THEN RETURN; END IF;`,
		"reads pg_table_size":               `IF pg_table_size('context_blocks') > 1073741824 THEN RETURN; END IF;`,
		"reads reltuples in dynamic SQL":    `EXECUTE 'SELECT reltuples FROM pg_class WHERE oid = $1' INTO v_rows USING v_rel;`,
		"after a string with an apostrophe": "RAISE NOTICE 'the block''s -- size'; SELECT reltuples FROM pg_class;",
		"asks the function for one index and copies the query for another": `v_rows := ctx_migration_rows('public.context_blocks', 500000);
			SELECT c.reltuples::BIGINT INTO v_rows FROM pg_class c WHERE c.relname = 'context_write_log';`,
		"a comment that names a definition": "-- CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint) AS $fn$\n" +
			"SELECT reltuples FROM pg_class WHERE relname = 'context_blocks';",
		// Taken for a definition, the string would open a body that never
		// closes and swallow the read after it.
		"a string literal that names a definition": "RAISE NOTICE 'CREATE FUNCTION ctx_migration_rows(x) AS $q$';\n" +
			"SELECT reltuples FROM pg_class WHERE relname = 'context_blocks';",
		"redefines the function and copies a query beside it": definition +
			"SELECT n_live_tup FROM pg_stat_user_tables WHERE relname = 'context_blocks';",
	} {
		if checkSizeDecision(src) == nil {
			t.Errorf("%s: passes the check", name)
		}
	}
}

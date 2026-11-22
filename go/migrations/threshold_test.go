package migrations

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The size-decision gate (issues #399 and #493). From rowsFunctionSince on, a
// migration that decides "inline or runbook" by the size of a table asks
// ctx_migration_rows; checkSizeDecision refuses a file that decides a size
// itself. It reads a file as the PostgreSQL lexer does: a comment separates
// tokens (block comments nest, a -- comment ends at a line feed or a carriage
// return), a quote ends where PostgreSQL ends it ('…'
// with '', E'…' with its backslash escapes, $tag$…$tag$ at the first closing
// tag, "…" for an identifier with its case), and the content of every string
// and dollar-quoted body is read again as SQL. A body after DO or AS is code
// whose statements stand by themselves; any other string is a value of the
// statement around it — a ";" inside it ends no statement — and is read as
// SQL too, since EXECUTE may run it.
//
// Refused, as a decision by a size:
//   - pg_relation_size, pg_total_relation_size, pg_table_size and
//     pg_indexes_size; the statistics views pg_stat_[xact_]{all,user,sys}_tables;
//     the statistics functions that count a table's tuples
//     (pg_stat_get_live_tuples, pg_stat_get_dead_tuples,
//     pg_stat_get_[xact_]tuples_{inserted,deleted}, pg_stat_get_ins_since_vacuum,
//     pg_stat_get_mod_since_analyze); the TimescaleDB estimates and sizes of a
//     hypertable (approximate_row_count, hypertable_size,
//     hypertable_detailed_size, hypertable_approximate_size,
//     hypertable_approximate_detailed_size, hypertable_index_size,
//     chunks_detailed_size); and EXPLAIN as a command, the planner's estimate
//     (followed by its options or the statement it plans; a column named
//     explain is none);
//   - the pg_class columns reltuples and relpages, in a statement that names
//     pg_class, or qualified by a name that is no relation of the statement
//     (r.reltuples, (r).reltuples: a row variable, whatever filled it);
//   - a SELECT count(*) of every row of one table, or the PL/pgSQL assignment
//     v := count(*) FROM t that PostgreSQL evaluates as one (with INTO, an
//     alias or a cast, and no WHERE, join or grouping; an ORDER BY, LIMIT or
//     OFFSET on the count's one row bounds nothing). A count stopped at the
//     threshold in a subquery, as ctx_migration_rows counts, is no estimate
//     and passes.
//
// Not a decision: a size a statement only reports. A RAISE (its message, its
// arguments, its USING options) and a COMMENT ON cannot steer the file, so a
// read there passes. The same read anywhere else is refused, an assignment
// included: a text lint does not follow a variable to its readers, so a size
// the WARNING prints goes into the RAISE itself. A column of a user table
// named relpages or reltuples is no read of pg_class: unqualified, or
// qualified by the table, its alias or the row of a FOR … IN over it in the
// same statement.
//
// A definition of ctx_migration_rows(regclass, bigint) itself at the top level
// of a file — this exact signature (the types also as their catalog names,
// quoted or not), the name unqualified or under public — may
// read in its body (a later file that fixes the function). The exemption runs
// from CREATE to the end of that body (a closed string after AS, BEGIN ATOMIC
// to its END, RETURN to the end of the statement) and no further; an overload
// is another function, and its body is checked, as is a definition inside a
// DO block, a string or another routine's BEGIN ATOMIC body.
//
// Named limits. The gate refuses the reads named above as a file spells them;
// a size read it does not name passes. It does not see a name spelled to get
// past it — an escape in an E'' string or in a U& identifier, a name
// concatenated from strings, a catalog column renamed by a column alias list
// — nor a read it would have to follow through the file: a copy of pg_class
// in a table the file creates, a view or function the file defines, a count
// with further columns or a table name built from strings. Nor does it
// resolve the scopes of SQL: a column named raise after THEN or ELSE in a
// CASE expression is taken for a RAISE that reports (the read after it
// passes), and an alias anywhere in a statement counts as a relation of all
// of it, so a subquery alias named like a row variable hides r.reltuples of
// the row (it passes); and a statement that names pg_class counts every
// relpages and reltuples in it, a user table's beside a join on pg_class
// too (it is refused). reviewedForms lists such forms as "limit".

// rowsFunctionSince is the migration that creates ctx_migration_rows (issue
// #399). From it on, a file that decides by the size of a table asks the
// function instead of copying the pg_class.reltuples query of 115, 140,
// 171-173, 175, 176, 182 and 184-187, which reads an estimate that can trail
// the table.
const rowsFunctionSince = 191

// maxQuoteDepth bounds how often checkSizeDecision reads the content of a
// string again as SQL (a string in a body in a string …). Past it a string's
// words still count; only its statements are no longer told apart.
const maxQuoteDepth = 8

// sqlName is an unquoted name in the text joinTokens builds.
const sqlName = `[a-z_][a-z0-9_$]*`

var (
	migrationVersion = regexp.MustCompile(`^(\d+)_.*\.sql$`)
	// sizeCall is a name that reads a table's size by itself: the file
	// sizes, the statistics views, the statistics functions that count a
	// table's tuples and the TimescaleDB estimates and sizes of a
	// hypertable. EXPLAIN is explainCommand.
	sizeCall = regexp.MustCompile(`^(pg_relation_size|pg_total_relation_size|pg_table_size|pg_indexes_size|` +
		`pg_stat_(xact_)?(all|user|sys)_tables|` +
		`pg_stat_get_(live_tuples|dead_tuples|(xact_)?tuples_(inserted|deleted)|ins_since_vacuum|mod_since_analyze)|` +
		`approximate_row_count|hypertable_(detailed_|approximate_|approximate_detailed_|index_)?size|chunks_detailed_size)$`)
	// explainCommand is what follows EXPLAIN as a command — its option list
	// or the statement it plans; after any other token "explain" is a name.
	explainCommand = map[string]bool{"(": true, "analyze": true, "analyse": true, "verbose": true, "select": true,
		"with": true, "values": true, "table": true, "insert": true, "update": true, "delete": true, "merge": true,
		"execute": true, "create": true, "declare": true}
	// catalogColumn is a column of pg_class that holds a size; the same name
	// in a user table is no read of it.
	catalogColumn = map[string]bool{"reltuples": true, "relpages": true}
	// wordIn finds the words of a string that is read as words only.
	wordIn = regexp.MustCompile(sqlName)
	// countEveryRow is a count of every row of one table in the text of a
	// statement (joinTokens), as a SELECT or as the PL/pgSQL assignment
	// v := count(*) FROM t: an INTO before or after the count, a cast and
	// an alias of it, and after the table at most an INTO and an ORDER BY,
	// LIMIT or OFFSET on the count's one row (with one level of parentheses)
	// — no WHERE, join or grouping. The table
	// may be a format() placeholder. Group 1 is the table, group 2 its alias
	// (a clauseWord there is no alias: the count has a clause).
	countEveryRow = regexp.MustCompile(`(?:\bselect(?: into (?:strict )?` + sqlName + `)?|(?:^| )=) count \( (?:\*|[0-9]+|` +
		sqlName + `(?: \. ` + sqlName + `)?) \)(?: : : ` + sqlName + `)?(?: (?:as )?` + sqlName + `)?` +
		`(?: into (?:strict )?` + sqlName + `)?` +
		` from (?:only )?((?:% [a-z]|` + sqlName + `)(?: \. (?:% [a-z]|` + sqlName + `))?)` +
		`(?: (?:as )?(` + sqlName + `))?(?: into (?:strict )?` + sqlName + `)?` +
		`(?: (?:order by|limit|offset) (?:[^()']|\( [^()']* \))*)?(?: \)| '|$)`)
	// clauseWord opens a clause after a table and is never its alias.
	clauseWord = map[string]bool{"where": true, "group": true, "having": true, "window": true, "order": true,
		"limit": true, "offset": true, "fetch": true, "for": true, "join": true, "inner": true, "left": true,
		"right": true, "full": true, "cross": true, "natural": true, "union": true, "intersect": true,
		"except": true, "tablesample": true}
	// statementStart is a token after which a PL/pgSQL statement begins.
	statementStart = map[string]bool{";": true, "then": true, "else": true, "loop": true, "begin": true}
)

type sqlTokenKind int

const (
	tWord     sqlTokenKind = iota // key word or unquoted name, lower-cased
	tIdent                        // "quoted identifier": its name, case kept
	tString                       // '…', E'…', U&'…': the content, its quotes undone
	tDollar                       // $tag$…$tag$: the content
	tOther                        // operator character, number, $n parameter
	tBoundary                     // a seam in a string read again as SQL: its start, its end, a ";" of a value
)

type sqlToken struct {
	kind sqlTokenKind
	text string
	open bool // a quote the input ends inside
}

// lexSQL splits src into tokens as the PostgreSQL lexer does, without the
// comments and the white space.
func lexSQL(src string) []sqlToken {
	var toks []sqlToken
	for i := 0; i < len(src); {
		next, t, ok := nextToken(src, i)
		if ok {
			toks = append(toks, t)
		}
		i = next
	}
	return toks
}

// nextToken reads the token at src[i]; ok is false for white space and a
// comment. It always moves past i.
func nextToken(src string, i int) (next int, t sqlToken, ok bool) {
	c := src[i]
	switch {
	case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
		return i + 1, t, false
	case strings.HasPrefix(src[i:], "--"):
		if end := strings.IndexAny(src[i:], "\r\n"); end >= 0 {
			return i + end + 1, t, false
		}
		return len(src), t, false
	case strings.HasPrefix(src[i:], "/*"):
		return skipBlockComment(src, i), t, false
	case c == '\'':
		return lexQuoted(src, i+1, '\'', false, tString)
	case c == '"':
		return lexQuoted(src, i+1, '"', false, tIdent)
	case c == '$':
		return lexDollar(src, i)
	case isNameStart(c):
		return lexWord(src, i)
	case c >= '0' && c <= '9':
		j := i + 1
		for j < len(src) && (isNameStart(src[j]) || src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
			j++
		}
		return j, sqlToken{kind: tOther, text: src[i:j]}, true
	}
	return i + 1, sqlToken{kind: tOther, text: src[i : i+1]}, true
}

// skipBlockComment returns the index after the block comment at src[i];
// block comments nest.
func skipBlockComment(src string, i int) int {
	depth := 0
	for i < len(src) {
		switch {
		case strings.HasPrefix(src[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(src[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(src)
}

// lexQuoted reads a quote whose content starts at src[i] and ends at q; a
// doubled q stands for itself, and with backslash a backslash escapes the
// next byte (E'…'). Other escapes stay undone.
func lexQuoted(src string, i int, q byte, backslash bool, kind sqlTokenKind) (int, sqlToken, bool) {
	var b strings.Builder
	for i < len(src) {
		c := src[i]
		switch {
		case backslash && c == '\\' && i+1 < len(src):
			b.WriteByte(src[i+1])
			i += 2
		case c == q && i+1 < len(src) && src[i+1] == q:
			b.WriteByte(q)
			i += 2
		case c == q:
			return i + 1, sqlToken{kind: kind, text: b.String()}, true
		default:
			b.WriteByte(c)
			i++
		}
	}
	return len(src), sqlToken{kind: kind, text: b.String(), open: true}, true
}

// lexDollar reads the token at src[i] == '$': a dollar-quoted string, which
// ends at the first occurrence of its opening tag, or a $n parameter.
func lexDollar(src string, i int) (int, sqlToken, bool) {
	j := i + 1
	if j < len(src) && isNameStart(src[j]) {
		for j < len(src) && (isNameStart(src[j]) || src[j] >= '0' && src[j] <= '9') {
			j++
		}
	}
	if j < len(src) && src[j] == '$' {
		tag := src[i : j+1]
		body := j + 1
		if end := strings.Index(src[body:], tag); end >= 0 {
			return body + end + len(tag), sqlToken{kind: tDollar, text: src[body : body+end]}, true
		}
		return len(src), sqlToken{kind: tDollar, text: src[body:], open: true}, true
	}
	k := i + 1
	for k < len(src) && src[k] >= '0' && src[k] <= '9' {
		k++
	}
	return k, sqlToken{kind: tOther, text: src[i:k]}, true
}

// lexWord reads a key word or an unquoted name at src[i], or the E'…', U&'…'
// or U&"…" its letters prefix.
func lexWord(src string, i int) (int, sqlToken, bool) {
	j := i + 1
	for j < len(src) && (isNameStart(src[j]) || src[j] >= '0' && src[j] <= '9' || src[j] == '$') {
		j++
	}
	w := lowerASCII(src[i:j])
	switch {
	case w == "e" && j < len(src) && src[j] == '\'':
		return lexQuoted(src, j+1, '\'', true, tString)
	case w == "u" && j+1 < len(src) && src[j] == '&' && src[j+1] == '\'':
		return lexQuoted(src, j+2, '\'', false, tString)
	case w == "u" && j+1 < len(src) && src[j] == '&' && src[j+1] == '"':
		return lexQuoted(src, j+2, '"', false, tIdent)
	}
	return j, sqlToken{kind: tWord, text: w}, true
}

func isNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func isWordAt(toks []sqlToken, i int, w string) bool {
	return i >= 0 && i < len(toks) && toks[i].kind == tWord && toks[i].text == w
}

func isPunct(toks []sqlToken, i int, p string) bool {
	return i >= 0 && i < len(toks) && toks[i].kind == tOther && toks[i].text == p
}

// isName: a key word, an unquoted name or a quoted identifier.
func isName(t sqlToken) bool {
	return t.kind == tWord || t.kind == tIdent
}

func isNameAt(toks []sqlToken, i int, name string) bool {
	return i >= 0 && i < len(toks) && isName(toks[i]) && toks[i].text == name
}

// withoutRowsFnDefinitions drops every definition of
// ctx_migration_rows(regclass, bigint), from CREATE to the end of its body,
// from the tokens of a file.
func withoutRowsFnDefinitions(toks []sqlToken) []sqlToken {
	out := make([]sqlToken, 0, len(toks))
	for i := 0; i < len(toks); i++ {
		end, ok := rowsFnDefinition(toks, i)
		if !ok && isWordAt(toks, i, "begin") && isWordAt(toks, i+1, "atomic") {
			// Another routine's BEGIN ATOMIC body: a definition in it is not
			// at the top level of the file, so it stays and is checked.
			if end, ok = atomicEnd(toks, i+2); ok {
				out = append(out, toks[i:end+1]...)
				i = end
				continue
			}
		}
		switch {
		case ok:
			i = end
			continue
		case end == bodyRunsToEnd:
			return append(out, toks[i:]...)
		}
		out = append(out, toks[i])
	}
	return out
}

// bodyRunsToEnd is what rowsFnDefinition returns for a BEGIN ATOMIC body the
// input ends inside: no definition, and none after it, since every later
// CREATE is inside that body.
const bodyRunsToEnd = -1

// maxHeadTokens bounds the head of a definition the gate exempts: its
// parameter list, and the attributes between that list and the body (191's
// take 7 and 12 tokens). A longer head is no such definition; the bound also
// keeps the work linear on input that opens heads it never closes.
const maxHeadTokens = 64

// definitionSteps counts the tokens the search for definitions visits;
// TestSizeDecisionCheckEndsOnUnbalancedQuotes holds it linear in the input.
var definitionSteps int

// rowsFnDefinition reports whether toks[i] opens a definition of
// ctx_migration_rows(regclass, bigint) with a body, and the index of the
// body's last token.
func rowsFnDefinition(toks []sqlToken, i int) (int, bool) {
	if !isWordAt(toks, i, "create") {
		return 0, false
	}
	j := i + 1
	if isWordAt(toks, j, "or") && isWordAt(toks, j+1, "replace") {
		j += 2
	}
	if !isWordAt(toks, j, "function") {
		return 0, false
	}
	j++
	if isNameAt(toks, j, "public") && isPunct(toks, j+1, ".") {
		j += 2
	}
	if !isNameAt(toks, j, "ctx_migration_rows") {
		return 0, false
	}
	j, ok := rowsParams(toks, j+1)
	if !ok {
		return 0, false
	}
	return rowsFnBodyEnd(toks, j)
}

// rowsParams reads the parameter list at toks[i] and reports whether it is
// (regclass, bigint), with the index after its ")".
func rowsParams(toks []sqlToken, i int) (int, bool) {
	if !isPunct(toks, i, "(") {
		return 0, false
	}
	var commas []int
	depth := 0
	for j := i + 1; j < len(toks) && j <= i+maxHeadTokens; j++ {
		definitionSteps++
		switch {
		case isPunct(toks, j, ")") && depth == 0:
			if len(commas) != 1 {
				return j + 1, false
			}
			c := commas[0]
			return j + 1, paramOfType(toks[i+1:c], "regclass") && paramOfType(toks[c+1:j], "bigint", "int8")
		case isPunct(toks, j, ",") && depth == 0:
			commas = append(commas, j)
		case isPunct(toks, j, "("):
			depth++
		case isPunct(toks, j, ")"):
			depth--
		}
	}
	return 0, false
}

// paramOfType reports whether a parameter is an input of one of types:
// [IN] [name] [pg_catalog.]type [DEFAULT …]. A quoted type is its catalog
// name, case and all ("regclass", "int8"); "bigint" quoted names no type,
// since bigint is a key word, not a catalog name.
func paramOfType(p []sqlToken, types ...string) bool {
	for k := range p {
		if isWordAt(p, k, "default") || isPunct(p, k, "=") {
			p = p[:k]
			break
		}
	}
	if len(p) > 1 && isWordAt(p, 0, "in") {
		p = p[1:]
	}
	n := len(p)
	if n == 0 || !slices.Contains(types, p[n-1].text) || p[n-1].kind != tWord && (p[n-1].kind != tIdent || p[n-1].text == "bigint") {
		return false
	}
	p = p[:n-1]
	if len(p) >= 2 && isPunct(p, len(p)-1, ".") && isNameAt(p, len(p)-2, "pg_catalog") {
		p = p[:len(p)-2]
	}
	if len(p) == 0 {
		return true
	}
	return len(p) == 1 && isName(p[0]) && !slices.Contains([]string{"in", "out", "inout", "variadic"}, p[0].text)
}

// rowsFnBodyEnd finds the body of a definition whose parameter list ends
// before toks[j]: a closed string after AS, BEGIN ATOMIC to its END, RETURN
// to the end of the statement. It returns the index of the body's last
// token; no body before the statement ends or within maxHeadTokens, or one
// the input ends inside, is no definition to exempt.
func rowsFnBodyEnd(toks []sqlToken, j int) (int, bool) {
	for k := j; k < len(toks) && k < j+maxHeadTokens; k++ {
		definitionSteps++
		switch {
		case isPunct(toks, k, ";"):
			return 0, false
		case isWordAt(toks, k, "as") && k+1 < len(toks) && (toks[k+1].kind == tString || toks[k+1].kind == tDollar):
			return k + 1, !toks[k+1].open
		case isWordAt(toks, k, "begin") && isWordAt(toks, k+1, "atomic"):
			return atomicEnd(toks, k+2)
		case isWordAt(toks, k, "return"):
			for k < len(toks) && !isPunct(toks, k, ";") {
				definitionSteps++
				k++
			}
			return k - 1, true
		}
	}
	return 0, false
}

// atomicEnd returns the index of the END that closes a BEGIN ATOMIC body
// whose first token is toks[k], or bodyRunsToEnd. A nested BEGIN ATOMIC and
// a CASE open what an END closes; begin alone is an unreserved word a column
// alias may take.
func atomicEnd(toks []sqlToken, k int) (int, bool) {
	depth := 1
	for ; k < len(toks); k++ {
		definitionSteps++
		switch {
		case isWordAt(toks, k, "begin") && isWordAt(toks, k+1, "atomic"), isWordAt(toks, k, "case"):
			depth++
		case isWordAt(toks, k, "end"):
			depth--
			if depth == 0 {
				return k, true
			}
		}
	}
	return bodyRunsToEnd, false
}

// flattenStatements appends toks to out with the content of every string and
// dollar-quoted body read again as SQL in its place (to maxQuoteDepth),
// between two tBoundary tokens, and without the statements that only report
// — a RAISE and a COMMENT ON, from their key word to their ";". In the
// content of a value (value: a string that is no body) a ";" becomes a
// tBoundary: it ends no statement around the string.
func flattenStatements(toks []sqlToken, depth int, value bool, out *[]sqlToken) {
	emit := func(t sqlToken) {
		if value && t.kind == tOther && t.text == ";" {
			t = sqlToken{kind: tBoundary}
		}
		*out = append(*out, t)
	}
	prev := ";"
	for k := 0; k < len(toks); k++ {
		t := toks[k]
		if statementStart[prev] && (isWordAt(toks, k, "raise") || isWordAt(toks, k, "comment") && isWordAt(toks, k+1, "on")) {
			for k < len(toks) && !isPunct(toks, k, ";") {
				k++
			}
			if k < len(toks) {
				emit(toks[k])
			}
			prev = ";"
			continue
		}
		if (t.kind == tString || t.kind == tDollar) && depth < maxQuoteDepth {
			*out = append(*out, sqlToken{kind: tBoundary})
			flattenStatements(lexSQL(t.text), depth+1, !isBody(toks, k), out)
			*out = append(*out, sqlToken{kind: tBoundary})
		} else {
			emit(t)
		}
		prev = ""
		if t.kind == tWord || t.kind == tOther {
			prev = t.text
		}
	}
}

// isBody reports whether the string at toks[k] is a body — after DO, DO
// LANGUAGE name or AS — whose statements stand by themselves.
func isBody(toks []sqlToken, k int) bool {
	return isWordAt(toks, k-1, "do") || isWordAt(toks, k-1, "as") ||
		isWordAt(toks, k-3, "do") && isWordAt(toks, k-2, "language")
}

// splitStatements cuts flat at every ";".
func splitStatements(flat []sqlToken) [][]sqlToken {
	var stmts [][]sqlToken
	start := 0
	for i := range flat {
		if isPunct(flat, i, ";") {
			stmts = append(stmts, flat[start:i])
			start = i + 1
		}
	}
	return append(stmts, flat[start:])
}

// tokenWords: the name of a name token, the words of a string that is read
// as words only (past maxQuoteDepth).
func tokenWords(t sqlToken) []string {
	switch t.kind {
	case tWord, tIdent:
		return []string{t.text}
	case tString, tDollar:
		return wordIn.FindAllString(lowerASCII(t.text), -1)
	}
	return nil
}

// namesCatalog reports whether a statement names pg_class.
func namesCatalog(stmt []sqlToken) bool {
	for _, t := range stmt {
		if slices.Contains(tokenWords(t), "pg_class") {
			return true
		}
	}
	return false
}

// qualifiedByRow reports whether stmt[i] is reached through a row —
// row.column or (row).column, where row is no relation of the statement: a
// row variable, whatever filled it (a SELECT … INTO, a cursor, a function).
func qualifiedByRow(stmt []sqlToken, i int) bool {
	var q string
	switch {
	case i >= 2 && isPunct(stmt, i-1, ".") && isName(stmt[i-2]):
		q = stmt[i-2].text
	case i >= 4 && isPunct(stmt, i-1, ".") && isPunct(stmt, i-2, ")") && isName(stmt[i-3]) && isPunct(stmt, i-4, "("):
		q = stmt[i-3].text
	default:
		return false
	}
	for k := range stmt {
		if isNameAt(stmt, k, q) && isRelationAt(stmt, k) {
			return false
		}
	}
	return true
}

// isRelationAt reports whether stmt[k] names a relation of the statement or
// its alias: after FROM, JOIN, UPDATE, INTO or ONLY (behind its schema, if
// any), after such a relation with or without AS, or the row of FOR … IN.
func isRelationAt(stmt []sqlToken, k int) bool {
	return relationAt(stmt, k) || isWordAt(stmt, k-1, "as") && relationAt(stmt, k-2) ||
		relationAt(stmt, k-1) || isWordAt(stmt, k-1, "for") && isWordAt(stmt, k+1, "in")
}

// relationAt reports whether stmt[k] is a name right after FROM, JOIN,
// UPDATE, INTO or ONLY, or the name behind such a schema.
func relationAt(stmt []sqlToken, k int) bool {
	if k < 0 || k >= len(stmt) || !isName(stmt[k]) {
		return false
	}
	if isPunct(stmt, k-1, ".") {
		k -= 2
	}
	for _, w := range []string{"from", "join", "update", "into", "only"} {
		if isWordAt(stmt, k-1, w) {
			return true
		}
	}
	return false
}

// joinTokens is the text of a statement for countEveryRow: its tokens joined
// by single spaces, a quoted identifier as a plain name, a string boundary
// as "'".
func joinTokens(stmt []sqlToken) string {
	parts := make([]string, 0, len(stmt))
	for _, t := range stmt {
		switch t.kind {
		case tWord, tOther:
			parts = append(parts, t.text)
		case tIdent:
			b := []byte(t.text)
			for i, c := range b {
				if c != '_' && c != '$' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
					b[i] = '_'
				}
			}
			parts = append(parts, "_"+string(b))
		default:
			parts = append(parts, "'")
		}
	}
	return strings.Join(parts, " ")
}

// sizeDecisionIn names the size a statement decides by, or returns "".
func sizeDecisionIn(stmt []sqlToken) string {
	catalog := namesCatalog(stmt)
	for i, t := range stmt {
		for _, w := range tokenWords(t) {
			if sizeCall.MatchString(w) {
				return "reads " + w
			}
			if catalogColumn[w] && (catalog || qualifiedByRow(stmt, i)) {
				return "reads pg_class." + w
			}
		}
		if isWordAt(stmt, i, "explain") && i+1 < len(stmt) && (stmt[i+1].kind == tWord || stmt[i+1].kind == tOther) &&
			explainCommand[stmt[i+1].text] {
			return "reads the planner's estimate (EXPLAIN)"
		}
	}
	for _, m := range countEveryRow.FindAllStringSubmatch(joinTokens(stmt), -1) {
		if !clauseWord[m[2]] {
			return "counts every row of " + strings.ReplaceAll(m[1], " ", "") + " (whether a table has rows: EXISTS)"
		}
	}
	return ""
}

// checkSizeDecision refuses a file whose code decides a size itself (the
// head of this file says what that is).
func checkSizeDecision(src string) error {
	var flat []sqlToken
	flattenStatements(withoutRowsFnDefinitions(lexSQL(src)), 0, false, &flat)
	for _, s := range splitStatements(flat) {
		if what := sizeDecisionIn(s); what != "" {
			return fmt.Errorf("%s: decide the size with ctx_migration_rows(<table>::regclass, <threshold>); "+
				"a size the file only reports belongs in the RAISE itself (docs/development.md, \"A migration with a row threshold\")", what)
		}
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
	// Every file before 191 that the check refuses is in the list: the list
	// is the inventory, not a sample. These files are shipped and exempt;
	// the check holds the files after them.
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
		"reads N_LIVE_TUP from PG_STAT_USER_TABLES": `SELECT N_LIVE_TUP INTO v_rows FROM PG_STAT_USER_TABLES WHERE relname = 'context_blocks';`,
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

// unbalancedInputs are inputs that end inside a quote, a comment or a body,
// and nest deeper than maxQuoteDepth.
func unbalancedInputs() []string {
	var nested strings.Builder
	for i := 0; i < 64; i++ {
		fmt.Fprintf(&nested, "EXECUTE $t%d$ ", i)
	}
	nested.WriteString("SELECT reltuples FROM pg_class")
	for i := 63; i >= 0; i-- {
		fmt.Fprintf(&nested, " $t%d$;", i)
	}
	return []string{
		"'", "E'\\", "E'it\\'s", "U&'", "U&\"", "\"", "$", "$a", "$a$", "$a$ $b$", "$1$", "/*", "/* /* */", "--", "-",
		"CREATE OR REPLACE FUNCTION ctx_migration_rows(",
		"CREATE OR REPLACE FUNCTION ctx_migration_rows(p regclass, t bigint) AS",
		"CREATE OR REPLACE FUNCTION ctx_migration_rows(p regclass, t bigint) BEGIN ATOMIC SELECT 1;",
		"CREATE OR REPLACE FUNCTION ctx_migration_rows(p regclass, t bigint) RETURN",
		"CREATE OR REPLACE FUNCTION public.",
		"RAISE", "COMMENT ON", "SELECT count(", "SELECT count(*) FROM",
		strings.Repeat("'", 4097), strings.Repeat("$a$", 4097), strings.Repeat("/*", 4097),
		strings.Repeat("E'\\", 4097), strings.Repeat("(", 4097), strings.Repeat("BEGIN ATOMIC ", 1024),
		strings.Repeat("EXECUTE '", 512), nested.String(),
	}
}

// endsInTime runs checkSizeDecision on src and fails on a panic or when it
// does not return within the bound.
func endsInTime(t *testing.T, src string) error {
	t.Helper()
	type result struct {
		err   error
		panic any
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{panic: p}
			}
		}()
		done <- result{err: checkSizeDecision(src)}
	}()
	short := src
	if len(short) > 80 {
		short = short[:80] + "…"
	}
	select {
	case r := <-done:
		if r.panic != nil {
			t.Fatalf("panics on %q: %v", short, r.panic)
		}
		return r.err
	case <-time.After(10 * time.Second):
		t.Fatalf("no decision within 10 s on %q (%d bytes)", short, len(src))
	}
	return nil
}

// TestSizeDecisionCheckEndsOnUnbalancedQuotes: on input that ends inside a
// quote, a comment or a body the check returns, and a read nested past
// maxQuoteDepth is still refused. On heads opened over and over and never
// closed the search for definitions visits at most 2·maxHeadTokens+1 tokens
// per token of the input (maxHeadTokens, bodyRunsToEnd); without the bound it
// was quadratic, 8 192 open heads took about 5 s.
func TestSizeDecisionCheckEndsOnUnbalancedQuotes(t *testing.T) {
	inputs := unbalancedInputs()
	for _, src := range inputs {
		endsInTime(t, src)
	}
	if endsInTime(t, inputs[len(inputs)-1]) == nil {
		t.Errorf("a read nested %d strings deep passes the check", 64)
	}
	const head = "CREATE FUNCTION ctx_migration_rows("
	for _, src := range []string{
		strings.Repeat(head, 4096),
		strings.Repeat(head+"p regclass, t bigint) AS ", 4096),
		strings.Repeat(head+"p regclass, t bigint) BEGIN ATOMIC ", 4096),
	} {
		definitionSteps = 0
		endsInTime(t, src)
		if n := len(lexSQL(src)); definitionSteps > (2*maxHeadTokens+1)*n {
			t.Errorf("%.40q…: the search for definitions visits %d tokens for %d", src, definitionSteps, n)
		}
	}
}

// TestSizeDecisionCheckRefusesEveryNamedRead: each name the head of this file
// refuses is refused on its own.
func TestSizeDecisionCheckRefusesEveryNamedRead(t *testing.T) {
	for _, name := range []string{
		"pg_relation_size", "pg_total_relation_size", "pg_table_size", "pg_indexes_size",
		"pg_stat_all_tables", "pg_stat_user_tables", "pg_stat_sys_tables",
		"pg_stat_xact_all_tables", "pg_stat_xact_user_tables", "pg_stat_xact_sys_tables",
		"pg_stat_get_live_tuples", "pg_stat_get_dead_tuples", "pg_stat_get_tuples_inserted", "pg_stat_get_tuples_deleted",
		"pg_stat_get_xact_tuples_inserted", "pg_stat_get_xact_tuples_deleted",
		"pg_stat_get_ins_since_vacuum", "pg_stat_get_mod_since_analyze",
		"approximate_row_count", "hypertable_size", "hypertable_detailed_size", "hypertable_approximate_size",
		"hypertable_approximate_detailed_size", "hypertable_index_size", "chunks_detailed_size", "explain",
	} {
		if checkSizeDecision(inDo("    v_rows := "+name+"('public.context_blocks');")) == nil {
			t.Errorf("%s: passes the check", name)
		}
	}
}

// FuzzSizeDecisionCheck: no input makes the check panic or hang.
func FuzzSizeDecisionCheck(f *testing.F) {
	for _, form := range reviewedForms {
		f.Add(form.src)
	}
	for _, src := range unbalancedInputs() {
		f.Add(src)
	}
	f.Fuzz(func(t *testing.T, src string) {
		endsInTime(t, src)
	})
}

// sizeForm is one migration the gate decides: the forms of issue #493 (the
// independent review of #484 fed the gate 26 constructed migrations), the
// boundary between deciding by a size and reporting one, and the named
// limits of a text lint.
type sizeForm struct {
	name   string
	src    string
	refuse bool
	// class says why the form is in the table: "too loose" and "too strict"
	// are the review's findings, "never covered" the forms it found the gate
	// never covered, "limit" a form only an author who wants past the gate
	// writes, a read the gate does not follow or a scope it does not resolve
	// (the table pins the decision it makes, see the head of this file),
	// "pin" a decision of the rebuilt gate that a regression of its lexer,
	// statement scope or report rule would turn.
	class string
}

// inDo wraps body in the DO block of the pattern in docs/development.md.
func inDo(body string) string {
	return "DO $do$\nDECLARE\n    inline_rows CONSTANT BIGINT := 500000;\n    v_rows      BIGINT;\n    v_heap      BIGINT;\n" +
		"BEGIN\n" + body + "\nEND\n$do$;\n"
}

const (
	// asksAndBuilds is the pattern's decision: the function, then the build.
	asksAndBuilds = `    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        RAISE WARNING 'idx_example is missing at % rows: build it out of band', v_rows;
        RETURN;
    END IF;
    CREATE INDEX idx_example ON context_blocks (col);`
	// copiedQuery is the threshold query of 172 and 184 as a later file
	// would copy it.
	copiedQuery = `    SELECT c.reltuples::BIGINT INTO v_rows
      FROM pg_class c
      JOIN pg_namespace n ON n.oid = c.relnamespace
     WHERE n.nspname = 'public' AND c.relname = 'context_blocks';
    IF v_rows >= inline_rows THEN
        RETURN;
    END IF;`
	// laterDollarAS is a helper after a definition whose dollar-quoted body
	// copies the query: the "later dollar-quoted AS" of the review.
	laterDollarAS = `
CREATE FUNCTION m193_rows(p_rel regclass) RETURNS bigint LANGUAGE sql STABLE AS $fn$
    SELECT reltuples::bigint FROM pg_class WHERE oid = p_rel
$fn$;
`
	// overloadBody is a dollar-quoted body that copies the query.
	overloadBody = `RETURNS bigint LANGUAGE sql STABLE AS $fn$
    SELECT reltuples::bigint FROM pg_class WHERE oid = p_rel
$fn$;
`
	// redefinition is the function itself, replaced, reading in its body.
	redefinitionBody = `RETURNS bigint LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $fn$
BEGIN
    RETURN (SELECT greatest(reltuples, pg_stat_get_live_tuples(oid))::bigint FROM pg_class WHERE oid = p_rel);
END
$fn$;
`
)

var reviewedForms = []sizeForm{
	// Too loose: the review's constructed migrations that passed although
	// they read an estimate or copy the query.
	{class: "too loose", refuse: true, name: "a definition with a single-quoted body, then a later dollar-quoted AS",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql AS 'SELECT p_threshold';\n" + laterDollarAS},
	{class: "too loose", refuse: true, name: "a definition with a BEGIN ATOMIC body, then a later dollar-quoted AS",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql\nBEGIN ATOMIC\n    SELECT CASE WHEN p_threshold > 0 THEN p_threshold END;\nEND;\n" + laterDollarAS},
	{class: "too loose", refuse: true, name: "a definition with a RETURN body, then a later dollar-quoted AS",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql RETURN p_threshold;\n" + laterDollarAS},
	{class: "too loose", refuse: true, name: "an overload with the copied query as its body",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass)\n" + overloadBody +
			inDo("    v_rows := ctx_migration_rows('public.context_blocks'::regclass);\n    IF v_rows >= inline_rows THEN\n        RETURN;\n    END IF;")},
	{class: "too loose", refuse: true, name: "an overload of other parameter types",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel text, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql STABLE AS $fn$\n    SELECT reltuples::bigint FROM pg_class WHERE oid = p_rel::regclass\n$fn$;\n"},
	{class: "too loose", refuse: true, name: "an overload with a third parameter that has a default",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint, p_estimate boolean DEFAULT true)\n" + overloadBody},
	{class: "too loose", refuse: true, name: "a comment set off by spaces between CREATE OR REPLACE and FUNCTION of an overload",
		src: "CREATE OR REPLACE /* the estimate is enough here */ FUNCTION ctx_migration_rows(p_rel regclass)\n" + overloadBody},
	{class: "pin", refuse: true, name: "a comment glued between CREATE OR REPLACE and FUNCTION of an overload",
		src: "CREATE OR REPLACE/* the estimate is enough here */FUNCTION ctx_migration_rows(p_rel regclass)\n" + overloadBody},
	{class: "too loose", refuse: true, name: "a definition whose dollar-quoted body never closes",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE plpgsql AS $fn$\nBEGIN\n    RETURN p_threshold;\nEND;\n" +
			"SELECT reltuples FROM pg_class WHERE relname = 'context_blocks';\n"},
	{class: "too loose", refuse: true, name: "a definition without a body (PostgreSQL refuses it), then a later dollar-quoted AS",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql;\n" + laterDollarAS},
	{class: "pin", refuse: true, name: "the exact signature under another schema",
		src: "CREATE OR REPLACE FUNCTION pg_temp.ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" + overloadBody},
	{class: "pin", refuse: true, name: "an upper-case quoted name is another function",
		src: "CREATE OR REPLACE FUNCTION \"CTX_MIGRATION_ROWS\"(p_rel regclass, p_threshold bigint)\n" + overloadBody},
	{class: "pin", refuse: true, name: "the function redefined in a procedure's BEGIN ATOMIC body is checked",
		src: "CREATE PROCEDURE m193_redefine() LANGUAGE sql\nBEGIN ATOMIC\n" +
			"    CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"    RETURNS bigint LANGUAGE sql AS $fn$ SELECT reltuples::bigint FROM pg_class WHERE oid = p_rel $fn$;\nEND;\n"},
	{class: "pin", refuse: true, name: "the function redefined inside a DO block is checked",
		src: "DO $do$\nBEGIN\n    CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"    RETURNS bigint LANGUAGE sql AS $fn$ SELECT reltuples::bigint FROM pg_class WHERE oid = p_rel $fn$;\nEND\n$do$;\n"},

	// Too strict: legitimate migrations the gate refused.
	{class: "too strict", name: "a WARNING whose text names pg_relation_size",
		src: inDo(`    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        RAISE WARNING 'idx_example is missing at % rows (rows, not pg_relation_size or reltuples): build it out of band', v_rows;
        RETURN;
    END IF;`)},
	{class: "too strict", name: "a WARNING that prints the heap size",
		src: inDo(`    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        RAISE WARNING 'idx_example is missing at % rows, heap %: build it out of band', v_rows,
            pg_size_pretty(pg_relation_size('public.context_blocks'));
        RETURN;
    END IF;`)},
	{class: "too strict", name: "a NOTICE after ELSE that prints the heap size",
		src: inDo(`    v_rows := ctx_migration_rows('public.context_blocks', inline_rows);
    IF v_rows >= inline_rows THEN
        RETURN;
    ELSE
        RAISE NOTICE 'building idx_example over % rows, heap % bytes', v_rows, pg_relation_size('public.context_blocks');
    END IF;`)},
	{class: "too strict", name: "a user table with a column named relpages",
		src: `CREATE TABLE context_page_marks (relid oid PRIMARY KEY, relpages integer NOT NULL DEFAULT 0);
COMMENT ON COLUMN context_page_marks.relpages IS 'pages seen at the last mark';
INSERT INTO context_page_marks (relid, relpages) VALUES ('public.context_blocks'::regclass, 0);
UPDATE context_page_marks SET relpages = relpages + 1 WHERE relid = 'public.context_blocks'::regclass;
`},
	{class: "too strict", name: "a comment glued between CREATE OR REPLACE and FUNCTION of the function itself",
		src: "CREATE OR REPLACE/* 191, again */FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" + redefinitionBody},
	{class: "too strict", name: "a COMMENT ON whose text names pg_relation_size",
		src: "COMMENT ON FUNCTION ctx_migration_rows(regclass, bigint) IS 'Rows, not pg_relation_size: the heap is the runbook''s business.';\n"},
	{class: "too strict", name: "the function redefined under its schema with the catalog names of its types",
		src: "CREATE OR REPLACE FUNCTION public.ctx_migration_rows(p_rel pg_catalog.regclass, p_threshold int8)\n" + redefinitionBody},
	{class: "too strict", name: "the function redefined with quoted catalog type names",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel pg_catalog.\"regclass\", p_threshold \"pg_catalog\".\"int8\")\n" + redefinitionBody},
	{class: "pin", refuse: true, name: "a quoted \"bigint\" names no type of the function",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold \"bigint\")\n" + redefinitionBody},
	{class: "too strict", name: "the function redefined with a single-quoted body",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql AS 'SELECT greatest(reltuples, pg_stat_get_live_tuples(oid))::bigint FROM pg_class WHERE oid = p_rel';\n"},
	{class: "too strict", name: "the function redefined with a BEGIN ATOMIC body",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql\nBEGIN ATOMIC\n" +
			"    SELECT CASE WHEN c.reltuples > 0 THEN c.reltuples::bigint END FROM pg_class c WHERE c.oid = p_rel;\nEND;\n"},
	{class: "too strict", name: "the function redefined with a BEGIN ATOMIC body that aliases a column begin",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE sql\nBEGIN ATOMIC\n    SELECT c.reltuples::bigint AS begin FROM pg_class c WHERE c.oid = p_rel;\nEND;\n"},
	{class: "too strict", name: "a nested block comment that holds a query",
		src: "/* runbook /* step 1 */ SELECT reltuples FROM pg_class WHERE relname = 'context_blocks'; */\n" + inDo(asksAndBuilds)},

	// The boundary between deciding by a size and reporting one.
	{class: "pin", refuse: true, name: "the same heap size in an IF",
		src: inDo(`    IF pg_relation_size('public.context_blocks') > 1073741824 THEN
        RAISE WARNING 'heap %: build it out of band', pg_size_pretty(pg_relation_size('public.context_blocks'));
        RETURN;
    END IF;`)},
	{class: "pin", refuse: true, name: "the heap size in an assignment that only a WARNING prints",
		src: inDo(`    v_heap := pg_relation_size('public.context_blocks');
    RAISE WARNING 'heap % bytes', v_heap;`)},
	{class: "pin", refuse: true, name: "a column named raise opens no report",
		src: inDo(`    SELECT c.relname AS raise, c.reltuples INTO v_heap, v_rows FROM pg_class c WHERE c.oid = 'public.context_blocks'::regclass;`)},
	{class: "pin", name: "a user table's relpages read through its alias",
		src: inDo(`    SELECT max(m.relpages) INTO v_rows FROM public.context_page_marks AS m;
    UPDATE context_page_marks SET relpages = context_page_marks.relpages + 1;`)},
	{class: "pin", name: "the pattern's index check beside a user table's relpages",
		src: inDo(`    SELECT i.indisvalid INTO v_heap FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid WHERE c.relname = 'idx_example';
    UPDATE context_page_marks SET relpages = 0;`)},
	{class: "pin", name: "a user table's relpages read in a loop over it",
		src: inDo(`    FOR m IN SELECT * FROM context_page_marks LOOP v_rows := v_rows + m.relpages; END LOOP;`)},
	{class: "pin", refuse: true, name: "a null check of a row variable makes it no relation",
		src: inDo(`    r := m193_catalog_row('public.context_blocks');
    IF r IS NOT NULL AND r.reltuples >= inline_rows THEN
        RETURN;
    END IF;`)},
	{class: "pin", refuse: true, name: "relpages read from pg_class beside a user table with a column of that name",
		src: "CREATE TABLE context_page_marks (relid oid PRIMARY KEY, relpages integer NOT NULL DEFAULT 0);\n" +
			inDo("    SELECT relpages INTO v_rows FROM pg_class WHERE oid = 'public.context_blocks'::regclass;")},
	{class: "pin", name: "a NOTICE right after BEGIN that prints the heap size",
		src: "DO $do$\nBEGIN\n    RAISE NOTICE 'context_blocks: heap % bytes', pg_relation_size('public.context_blocks');\nEND\n$do$;\n"},
	{class: "pin", name: "a NOTICE right after LOOP that prints the heap size",
		src: inDo(`    FOR v_rows IN 1..1 LOOP
        RAISE NOTICE 'heap % bytes', pg_relation_size('public.context_blocks');
    END LOOP;`)},
	{class: "pin", refuse: true, name: "a dollar-quoted apostrophe in a RAISE hides no read after it",
		src: inDo("    RAISE NOTICE $m$it's 191's function$m$;\n" + copiedQuery)},
	{class: "pin", refuse: true, name: "an escaped quote in an E-string of a RAISE hides no read after it",
		src: inDo("    RAISE NOTICE E'the block\\'s size';\n" + copiedQuery)},
	{class: "pin", refuse: true, name: "a semicolon in a string literal ends no statement",
		src: inDo(`    SELECT sum(reltuples)::bigint, string_agg(relname, '; ') INTO v_rows, v_heap
      FROM pg_class WHERE relname IN ('context_blocks', 'context_write_log');`)},
	{class: "pin", refuse: true, name: "a semicolon string beside the read",
		src: inDo(`    SELECT c.reltuples::bigint, ';' INTO v_rows, v_heap FROM pg_class c WHERE c.oid = 'public.context_blocks'::regclass;`)},

	// Never covered before: what an author who copies the threshold query or
	// estimates a size in good faith writes.
	{class: "never covered", refuse: true, name: "an unbounded count(*) decides",
		src: inDo(`    SELECT count(*) INTO v_rows FROM public.context_blocks;
    IF v_rows >= inline_rows THEN
        RETURN;
    END IF;`)},
	{class: "never covered", refuse: true, name: "an unbounded count(*) in the condition",
		src: inDo(`    IF (SELECT count(*) FROM context_blocks) >= inline_rows THEN
        RETURN;
    END IF;`)},
	{class: "never covered", refuse: true, name: "a LIMIT on the one row of a count bounds nothing",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks LIMIT 500000;`)},
	{class: "never covered", refuse: true, name: "a LIMIT in parentheses on the one row of a count bounds nothing",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks LIMIT (500000);`)},
	{class: "never covered", refuse: true, name: "an ORDER BY on the one row of a count",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks ORDER BY count(*);`)},
	{class: "never covered", refuse: true, name: "an unbounded count(*) in a PL/pgSQL assignment",
		src: inDo(`    v_rows := count(*) FROM context_blocks;`)},
	{class: "pin", name: "a filtered count in a PL/pgSQL assignment",
		src: inDo(`    v_rows := count(*) FROM context_blocks WHERE created_at IS NULL;`)},
	{class: "pin", name: "a count stopped at the threshold in a PL/pgSQL assignment",
		src: inDo(`    v_rows := count(*) FROM (SELECT 1 FROM context_blocks LIMIT inline_rows) t;`)},
	{class: "never covered", refuse: true, name: "an unbounded count(*) in dynamic SQL",
		src: inDo(`    EXECUTE format('SELECT count(*) FROM %I', 'context_blocks') INTO v_rows;`)},
	{class: "never covered", refuse: true, name: "an unbounded count(*) of a schema-qualified format() table",
		src: inDo(`    EXECUTE format('SELECT count(*) FROM %I.%I', 'public', 'context_blocks') INTO v_rows;`)},
	{class: "never covered", refuse: true, name: "an unbounded count(*) with an alias",
		src: inDo(`    SELECT count(*) AS n INTO v_rows FROM context_blocks;`)},
	{class: "never covered", refuse: true, name: "an unbounded count(*) with INTO first",
		src: inDo(`    SELECT INTO v_rows count(*) FROM context_blocks;`)},
	{class: "pin", name: "the count of the largest group",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks GROUP BY scope ORDER BY 1 DESC LIMIT 1;`)},
	{class: "pin", name: "a count over a join",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks b JOIN context_block_grants g ON g.block_id = b.id;`)},
	{class: "pin", name: "a count stopped at the threshold, as ctx_migration_rows counts",
		src: inDo(`    SELECT count(*) INTO v_rows FROM (SELECT 1 FROM context_blocks LIMIT inline_rows) t;`)},
	{class: "pin", name: "a filtered count whose condition starts with a string",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks WHERE 'archived' = guard_status;`)},
	{class: "pin", name: "a count of the rows a constraint would reject",
		src: inDo(`    SELECT count(*) INTO v_rows FROM context_blocks WHERE created_at IS NULL;
    IF v_rows > 0 THEN
        RAISE EXCEPTION '% blocks without created_at', v_rows;
    END IF;`)},
	{class: "pin", name: "a count per group",
		src: "INSERT INTO context_block_counts (scope, category, is_archived, guard_status, n)\n" +
			"SELECT scope, category, is_archived, guard_status, count(*) FROM context_blocks GROUP BY 1, 2, 3, 4;\n"},
	{class: "pin", refuse: true, name: "pg_class.reltuples through a %ROWTYPE row",
		src: `DO $do$
DECLARE
    r pg_class%ROWTYPE;
BEGIN
    SELECT * INTO r FROM pg_class WHERE oid = 'public.context_blocks'::regclass;
    IF r.reltuples >= 500000 THEN
        RETURN;
    END IF;
END
$do$;
`},
	{class: "pin", refuse: true, name: "pg_class.reltuples through a record a SELECT INTO filled",
		src: `DO $do$
DECLARE
    r RECORD;
BEGIN
    SELECT c.* INTO STRICT r FROM pg_class c WHERE c.oid = 'public.context_blocks'::regclass;
    IF (r).reltuples >= 500000 THEN
        RETURN;
    END IF;
END
$do$;
`},
	{class: "pin", refuse: true, name: "pg_class.reltuples through the row of a FOR loop, a statement later",
		src: inDo(`    FOR r IN SELECT * FROM pg_class WHERE relname = 'context_blocks' LOOP
        RAISE NOTICE 'measuring %', r.relname;
        v_rows := r.reltuples;
    END LOOP;`)},
	{class: "pin", refuse: true, name: "pg_class.reltuples through a %ROWTYPE row a function filled",
		src: `DO $do$
DECLARE
    r pg_class%ROWTYPE;
BEGIN
    r := m193_catalog_row('public.context_blocks');
    IF r.reltuples >= 500000 THEN
        RETURN;
    END IF;
END
$do$;
`},
	{class: "pin", refuse: true, name: "pg_class.reltuples through a record := filled",
		src: `DO $do$
DECLARE
    r RECORD;
BEGIN
    r := (SELECT c FROM pg_class c WHERE c.oid = 'public.context_blocks'::regclass);
    IF (r).reltuples >= 500000 THEN
        RETURN;
    END IF;
END
$do$;
`},
	{class: "never covered", refuse: true, name: "a size through pg_stat_all_tables",
		src: inDo(`    SELECT n_tup_ins - n_tup_del INTO v_rows FROM pg_stat_all_tables WHERE relid = 'public.context_blocks'::regclass;`)},
	{class: "never covered", refuse: true, name: "a pg_class size through the statistics functions",
		src: inDo(`    SELECT pg_stat_get_tuples_inserted(c.oid) - pg_stat_get_tuples_deleted(c.oid) INTO v_rows
      FROM pg_class c WHERE c.oid = 'public.context_blocks'::regclass;`)},
	{class: "never covered", refuse: true, name: "pg_indexes_size decides",
		src: inDo(`    IF pg_indexes_size('public.context_blocks') > 1073741824 THEN
        RETURN;
    END IF;`)},
	{class: "never covered", refuse: true, name: "TimescaleDB's approximate_row_count decides",
		src: inDo(`    v_rows := approximate_row_count('public.context_llm_log');
    IF v_rows >= inline_rows THEN
        RETURN;
    END IF;`)},
	{class: "never covered", refuse: true, name: "hypertable_size decides",
		src: inDo(`    IF hypertable_size('public.context_llm_log') > 1073741824 THEN
        RETURN;
    END IF;`)},
	{class: "never covered", refuse: true, name: "the planner's estimate through EXPLAIN decides",
		src: inDo(`    EXECUTE 'EXPLAIN (FORMAT JSON) SELECT 1 FROM public.context_blocks' INTO v_plan;
    v_rows := (v_plan->0->'Plan'->>'Plan Rows')::bigint;`)},
	{class: "never covered", refuse: true, name: "EXPLAIN as the query of a FOR loop",
		src: inDo(`    FOR v_plan IN EXPLAIN SELECT 1 FROM public.context_blocks LOOP
        v_rows := v_rows + 1;
    END LOOP;`)},
	{class: "pin", name: "a column named explain",
		src: "CREATE TABLE migration_notes (id bigint PRIMARY KEY, explain text);\n" +
			inDo(`    SELECT count(explain) INTO v_rows FROM migration_notes WHERE id > 0;
    UPDATE migration_notes SET explain = 'checked' WHERE explain IS NULL;`)},

	// Limits: a name an author spells to get past the gate, and a read the
	// gate would have to follow through the file. These pass; the head of
	// this file names them.
	{class: "limit", name: "an E-string hex escape spells the name",
		src: inDo(`    EXECUTE E'SELECT rel\x74uples::bigint FROM pg_class WHERE oid = $1' INTO v_rows USING 'public.context_blocks'::regclass;`)},
	{class: "limit", name: "an E-string octal escape spells the name",
		src: inDo(`    EXECUTE E'SELECT rel\164uples::bigint FROM pg_class WHERE oid = $1' INTO v_rows USING 'public.context_blocks'::regclass;`)},
	{class: "limit", name: "a name concatenated from two strings",
		src: inDo(`    EXECUTE 'SELECT rel' || 'tuples::bigint FROM pg_class WHERE oid = $1' INTO v_rows USING 'public.context_blocks'::regclass;`)},
	{class: "limit", name: "an identifier with a unicode escape",
		src: inDo(`    SELECT U&"rel\0074uples"::bigint INTO v_rows FROM pg_class WHERE oid = 'public.context_blocks'::regclass;`)},
	{class: "limit", name: "a column alias list renames the catalog column",
		src: inDo(`    SELECT c.t::bigint INTO v_rows FROM pg_class AS c(o, n, ns, ty, oty, own, am, fn, ts, p, t)
     WHERE c.o = 'public.context_blocks'::regclass;`)},
	{class: "pin", refuse: true, name: "pg_class.reltuples through a row fetched from a cursor",
		src: `DO $do$
DECLARE
    cur refcursor;
    r   RECORD;
BEGIN
    OPEN cur FOR SELECT * FROM pg_class WHERE oid = 'public.context_blocks'::regclass;
    FETCH cur INTO r;
    IF r.reltuples >= 500000 THEN
        RETURN;
    END IF;
END
$do$;
`},
	{class: "limit", name: "a copy of pg_class in a table the file creates",
		src: "CREATE TEMP TABLE m193_catalog AS SELECT * FROM pg_class WHERE oid = 'public.context_blocks'::regclass;\n" +
			inDo("    SELECT reltuples::bigint INTO v_rows FROM m193_catalog;")},
	{class: "limit", name: "a column named raise in a CASE arm before a size read",
		src: inDo(`    SELECT CASE WHEN enabled THEN raise ELSE pg_relation_size('public.context_blocks') END INTO v_rows FROM settings;`)},
	{class: "limit", name: "a subquery alias named like the row variable it hides",
		src: `DO $do$
DECLARE
    r pg_class%ROWTYPE;
BEGIN
    r := m193_catalog_row('public.context_blocks');
    IF r.reltuples > 500000 AND EXISTS (SELECT 1 FROM settings r) THEN
        RETURN;
    END IF;
END
$do$;
`},
	{class: "limit", refuse: true, name: "a user table's relpages beside a join on pg_class",
		src: inDo(`    SELECT m.relpages INTO v_rows FROM context_page_marks m JOIN pg_class c ON c.oid = m.relid;`)},
	{class: "limit", name: "a count with a further column",
		src: inDo(`    SELECT count(*), max(created_at) INTO v_rows, v_heap FROM context_blocks;`)},

	// The lexer: quotes, comments and bodies end where PostgreSQL ends them.
	{class: "pin", refuse: true, name: "a line comment ends at a carriage return",
		src: "-- size check\rSELECT reltuples FROM pg_class WHERE relname = 'context_blocks';\n"},
	{class: "pin", refuse: true, name: "a dollar-quoted apostrophe inside the definition hides no read after it",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE plpgsql AS $fn$\nBEGIN\n    RAISE NOTICE $m$it's 191's function$m$;\n    RETURN p_threshold;\nEND\n$fn$;\n" +
			inDo(copiedQuery)},
	{class: "pin", refuse: true, name: "a dollar-quoted apostrophe hides no read after it",
		src: inDo("    v_heap := length($m$the block's size$m$);\n" + copiedQuery)},
	{class: "pin", refuse: true, name: "a body that holds its own closing tag in a string",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" +
			"RETURNS bigint LANGUAGE plpgsql AS $fn$\nBEGIN\n    RAISE NOTICE 'the body ends at $fn$ here';\n    RETURN p_threshold;\nEND\n$fn$;\n" +
			"SELECT reltuples FROM pg_class WHERE relname = 'context_blocks';\n"},
	{class: "pin", refuse: true, name: "a copied query in a DO block",
		src: inDo(copiedQuery)},
	{class: "pin", name: "a runbook in a comment",
		src: "-- Runbook: CREATE INDEX CONCURRENTLY idx_example ON context_blocks (col);\n" +
			"-- check SELECT reltuples FROM pg_class WHERE relname = 'context_blocks' first.\n" + inDo(asksAndBuilds)},
	{class: "pin", name: "the function redefined, reading in its body",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" + redefinitionBody + inDo(asksAndBuilds)},
	{class: "pin", refuse: true, name: "the function redefined, then the query copied",
		src: "CREATE OR REPLACE FUNCTION ctx_migration_rows(p_rel regclass, p_threshold bigint)\n" + redefinitionBody + inDo(copiedQuery)},
}

// TestSizeDecisionCheckDecidesTheReviewedForms holds checkSizeDecision to the
// decision of every form in reviewedForms.
func TestSizeDecisionCheckDecidesTheReviewedForms(t *testing.T) {
	for _, f := range reviewedForms {
		err := checkSizeDecision(f.src)
		switch {
		case f.refuse && err == nil:
			t.Errorf("%s (%s): passes the check, want it refused", f.name, f.class)
		case !f.refuse && err != nil:
			t.Errorf("%s (%s): refused (%v), want it passed", f.name, f.class, err)
		}
	}
}

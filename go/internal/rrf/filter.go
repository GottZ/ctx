package rrf

import "fmt"

// RequestFilter is the request-level narrowing of POST /api/query: the
// category, tags, categories_exclude and types_exclude that ctx_rrf applies to
// its candidates (p_category, p_tags, p_categories_exclude, p_types_exclude).
//
// The stages after the fusion that can ADD a block — the cluster injection,
// the graph expansion and the aggregate fold — apply the same filter to every
// block they would add (issue #225), so a block the request excluded cannot
// come back as a sibling, a neighbour or a parent. It is a relevance
// narrowing, not a visibility rule: in every statement it stands next to
// visibility.Predicate (scopes, grants, type allowlist), at the same position,
// and never replaces it.
//
// The zero value narrows nothing. The graph hydrates and the cluster injection
// given it run the statement they ran before the filter existed, text and
// arguments unchanged; the fold's parent hydrate reads a constant TRUE in the
// filter's column.
type RequestFilter struct {
	Category          *string
	Tags              []string
	CategoriesExclude []string
	TypesExclude      []string
}

// IsZero reports whether the filter narrows nothing.
func (f RequestFilter) IsZero() bool {
	return f.Category == nil && len(f.Tags) == 0 && len(f.CategoriesExclude) == 0 && len(f.TypesExclude) == 0
}

// args returns the four values the way ctx_rrf takes them: an empty list is
// NULL (the no-op, migration 048's convention), the category is passed as
// given (nil is NULL; an empty string is a filter that matches no block, as in
// ctx_rrf). rrfBaseArgs builds the fusion's arguments from this function, so
// the fusion and the stages after it cannot read one request two ways.
func (f RequestFilter) args() [4]any {
	var tags, categoriesExclude, typesExclude any
	if len(f.Tags) > 0 {
		tags = f.Tags
	}
	if len(f.CategoriesExclude) > 0 {
		categoriesExclude = f.CategoriesExclude
	}
	if len(f.TypesExclude) > 0 {
		typesExclude = f.TypesExclude
	}
	return [4]any{f.Category, tags, categoriesExclude, typesExclude}
}

// Predicate returns ctx_rrf's four filter conjuncts over the context_blocks
// alias, bound to the placeholders $first..$first+3, and their arguments. The
// zero filter returns "" and no arguments: the caller appends nothing and its
// statement stays the one it was.
//
// The conjuncts are ctx_rrf's own (migration 147), so a NULL argument is the
// no-op and a NULL column fails the conjunct, exactly as a WHERE clause of the
// fusion treats it. A caller that evaluates the predicate as a value rather
// than as a WHERE condition must therefore read NULL as "fails".
func (f RequestFilter) Predicate(alias string, first int) (string, []any) {
	if f.IsZero() {
		return "", nil
	}
	a := f.args()
	sql := fmt.Sprintf(
		"($%[2]d::text IS NULL OR %[1]s.category = $%[2]d::text)"+
			" AND ($%[3]d::text[] IS NULL OR %[1]s.tags && $%[3]d::text[])"+
			" AND ($%[4]d::text[] IS NULL OR %[1]s.category <> ALL($%[4]d::text[]))"+
			" AND ($%[5]d::text[] IS NULL OR %[1]s.type_name <> ALL($%[5]d::text[]))",
		alias, first, first+1, first+2, first+3)
	return sql, a[:]
}

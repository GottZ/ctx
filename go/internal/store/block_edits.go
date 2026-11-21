// block_edits.go — the text edits and the metadata merge of a block update
// (issue #406): the two additive fields that let a caller change part of a
// block without sending the whole of it back.
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxBlockContentBytes is the content cap of every client write path (50 KB,
// counted in bytes of UTF-8). The handlers check a sent content against it
// before the store runs (blockSizeLimit); the content an update's edits
// produce exists only under the row lock, so ApplyContentEdits checks it
// there, against the same constant.
const MaxBlockContentBytes = 50 * 1024

// MaxContentEdits is the most edits one update may carry. The edits apply
// while the update holds the block's row lock and a pooled connection, and
// each one scans and copies the content once, so the count bounds that hold
// the way MaxBlockContentBytes bounds each scan: a list at both caps costs
// 12–13 ms (BenchmarkApplyContentEditsAtTheCaps), where the 1 MB request body
// alone would admit tens of thousands of edits and seconds of lock time.
// A request-shape cap like the content cap, not a tunable policy: a caller
// that needs more rewrites the content whole.
const MaxContentEdits = 100

// ContentEdit is one text replacement of an update's edit list. Old must
// occur in the content exactly once — or, with ReplaceAll, at least once, and
// then every non-overlapping occurrence from left to right is replaced. New is
// a pointer so that a JSON edit without "new" decodes to nil and is refused
// (CheckUpdatePatch) instead of deleting the text; "new": "" deletes it.
//
// Matching compares bytes of the UTF-8 text: no Unicode normalisation, no case
// folding, no whitespace folding. A valid UTF-8 old can only match at
// character boundaries of valid UTF-8 content, so a match never splits a
// character.
type ContentEdit struct {
	Old        string  `json:"old"`
	New        *string `json:"new"`
	ReplaceAll bool    `json:"replace_all,omitempty"`
}

// ContentEditError is the refusal of an edit whose old does not match the
// content as the edits before it left it: Matches times where it needed
// exactly one (ReplaceAll false) or at least one (ReplaceAll true). Index is
// the edit's 0-based position in the list. The update it belongs to writes
// nothing.
type ContentEditError struct {
	Index      int
	Matches    int
	ReplaceAll bool
}

func (e *ContentEditError) Error() string {
	need := "exactly one match"
	if e.ReplaceAll {
		need = "at least one match with replace_all"
	}
	return fmt.Sprintf("edits[%d]: old matches %d times in the content (after the edits before it) — an edit needs %s; nothing was written",
		e.Index, e.Matches, need)
}

// ErrEditedContentTooLarge is the refusal of an edit list whose content would
// exceed MaxBlockContentBytes after one of its edits; the error wraps it
// behind the edit's index. The update writes nothing.
var ErrEditedContentTooLarge = errors.New("the content would exceed 50KB")

// CheckUpdatePatch refuses the field combinations an update cannot mean:
// content beside edits, metadata beside metadata_merge, an empty edit list or
// one longer than MaxContentEdits, an edit with an empty old (it would match
// at every position) or without new.
// nil = admissible. The update surfaces call it before they resolve the block
// and answer its message as a 400; UpdateBlock calls it again, so a surface
// cannot reach the statement past it.
func CheckUpdatePatch(d UpdateBlockData) error {
	if d.Content != nil && d.Edits != nil {
		return errors.New("content and edits are mutually exclusive — send the whole content or a list of edits, not both")
	}
	if d.Metadata != nil && d.MetadataMerge != nil {
		return errors.New("metadata and metadata_merge are mutually exclusive — send a replacement object or a merge, not both")
	}
	if d.Edits != nil && len(d.Edits) == 0 {
		return errors.New("edits: the list is empty — send at least one edit")
	}
	if len(d.Edits) > MaxContentEdits {
		return fmt.Errorf("edits: %d edits exceed the cap of %d per update — send the whole content instead", len(d.Edits), MaxContentEdits)
	}
	for i, e := range d.Edits {
		if err := checkContentEdit(i, e); err != nil {
			return err
		}
	}
	return nil
}

func checkContentEdit(i int, e ContentEdit) error {
	if e.Old == "" {
		return fmt.Errorf("edits[%d]: old is empty — an edit names the text it replaces; to write a whole new content, send content", i)
	}
	if e.New == nil {
		return fmt.Errorf("edits[%d]: new is missing — send \"new\": \"\" to delete the text", i)
	}
	return nil
}

// ApplyContentEdits applies edits in order to content and returns the result,
// or the refusal of the first edit that does not apply: *ContentEditError for
// a miss or an ambiguous match, ErrEditedContentTooLarge (wrapped with the
// edit's index) when the content would exceed MaxBlockContentBytes after it.
// The size is computed before the grown text is built, so a replace_all that
// would multiply the content allocates nothing.
//
// Without ReplaceAll an occurrence counts at every byte position where old
// starts, overlapping ones included ("aa" occurs twice in "aaa"): an edit that
// could land in two places is refused, never resolved to the first.
func ApplyContentEdits(content string, edits []ContentEdit) (string, error) {
	for i, e := range edits {
		if err := checkContentEdit(i, e); err != nil {
			return "", err
		}
		repl := *e.New
		if e.ReplaceAll {
			n := strings.Count(content, e.Old)
			if n == 0 {
				return "", &ContentEditError{Index: i, Matches: 0, ReplaceAll: true}
			}
			if len(content)+n*(len(repl)-len(e.Old)) > MaxBlockContentBytes {
				return "", fmt.Errorf("edits[%d]: %w", i, ErrEditedContentTooLarge)
			}
			content = strings.ReplaceAll(content, e.Old, repl)
			continue
		}
		if n := occurrences(content, e.Old); n != 1 {
			return "", &ContentEditError{Index: i, Matches: n}
		}
		if len(content)-len(e.Old)+len(repl) > MaxBlockContentBytes {
			return "", fmt.Errorf("edits[%d]: %w", i, ErrEditedContentTooLarge)
		}
		at := strings.Index(content, e.Old)
		content = content[:at] + repl + content[at+len(e.Old):]
	}
	return content, nil
}

// occurrences counts the positions at which sub (non-empty) starts in s,
// overlapping ones included. Knuth–Morris–Pratt: linear in len(s)+len(sub),
// so a long periodic old against a long periodic content costs what a plain
// scan does, where restarting a search one byte after each match would cost
// len(s)·len(sub).
func occurrences(s, sub string) int {
	if len(sub) > len(s) {
		return 0
	}
	fail := make([]int, len(sub))
	for i, k := 1, 0; i < len(sub); i++ {
		for k > 0 && sub[i] != sub[k] {
			k = fail[k-1]
		}
		if sub[i] == sub[k] {
			k++
		}
		fail[i] = k
	}
	n := 0
	for i, k := 0, 0; i < len(s); i++ {
		for k > 0 && s[i] != sub[k] {
			k = fail[k-1]
		}
		if s[i] == sub[k] {
			k++
		}
		if k == len(sub) {
			n++
			k = fail[k-1]
		}
	}
	return n
}

// contentEditPlan carries an update's edits into runBlockUpdate: the list, the
// index of the content argument it fills (the content_times argument, the
// content's dates, follows it), and the write scopes the row is read within.
type contentEditPlan struct {
	edits       []ContentEdit
	arg         int
	writeScopes []string
}

// editTargetWhere is the WHERE of the reads of an update's edits: UpdateBlock's,
// so a block outside the write scopes, an archived block and a derivative are
// a miss here as there — no match count is computed over text the caller may
// not write.
const editTargetWhere = `
	 WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
	   AND ` + notDerivedSQL

// readEditTargetSQL reads the content an update's edits apply to, and its
// version (content_hash), without a lock.
const readEditTargetSQL = `SELECT content, content_hash FROM context_blocks` + editTargetWhere

// lockEditTargetSQL locks the row with the strength the UPDATE that follows
// takes anyway (FOR NO KEY UPDATE: two editors queue on it, a link insert's
// FOR KEY SHARE does not) and answers whether it still holds the version
// readEditTargetSQL read ($3). Under READ COMMITTED a reader that waited for
// the lock gets the row version the holder committed.
const lockEditTargetSQL = `SELECT content_hash = $3 FROM context_blocks` + editTargetWhere + `
	 FOR NO KEY UPDATE`

// relockedEditTargetSQL reads the content of a row this transaction holds.
const relockedEditTargetSQL = `SELECT content FROM context_blocks WHERE id = $1`

// resolve locks the update's row, applies the edits to its content and puts
// the result into args. ok=false with a nil error is a miss (no row the update
// may write): the caller rolls back and UpdateBlock names the reason as for
// any 0-row update. ok=false with an error is a refused edit or a read fault.
// A nil plan (an update without edits) is ok and does nothing.
//
// The edits and the dates of their result (ExtractDates, which takes
// milliseconds on a large content, issue #421) are computed on the content
// read before the row is locked, so the lock is not held while they run; the
// lock then confirms the row still holds that version. A row another writer
// changed in between is edited again, under the lock, on the content it holds
// now: the match always runs on the text the write replaces.
func (p *contentEditPlan) resolve(ctx context.Context, tx pgx.Tx, id string, args []any) (bool, error) {
	if p == nil {
		return true, nil
	}
	var content, version string
	err := tx.QueryRow(ctx, readEditTargetSQL, id, p.writeScopes).Scan(&content, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update block: read edit target: %w", err)
	}
	edited, times, editErr := applyEditsAndDates(content, p.edits)
	var same bool
	err = tx.QueryRow(ctx, lockEditTargetSQL, id, p.writeScopes, version).Scan(&same)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: update block: lock edit target: %w", err)
	}
	if !same {
		if err := tx.QueryRow(ctx, relockedEditTargetSQL, id).Scan(&content); err != nil {
			return false, fmt.Errorf("store: update block: re-read edit target: %w", err)
		}
		edited, times, editErr = applyEditsAndDates(content, p.edits)
	}
	if editErr != nil {
		return false, editErr
	}
	args[p.arg] = edited
	args[p.arg+1] = times
	return true, nil
}

// applyEditsAndDates is ApplyContentEdits plus the dates of its result.
func applyEditsAndDates(content string, edits []ContentEdit) (string, []time.Time, error) {
	edited, err := ApplyContentEdits(content, edits)
	if err != nil {
		return "", nil, err
	}
	return edited, ExtractDates(edited), nil
}

// updateMetadataExpr builds the one assignment of the metadata column an
// update makes ("" = it makes none) and appends its arguments. Exactly one
// assignment per column is legal in SQL, so the replacement or the merge, the
// guard_checked_at strip of a content write and the sensitivity_audit of a
// confirmed downgrade compose into a single expression.
//
// The merge (issue #406) reads the column of the row the statement locked:
// (stored || set) - removed. A NULL metadata merges as {}.
func updateMetadataExpr(data UpdateBlockData, contentSent bool, args []any) (string, []any) {
	metaExpr := ""
	switch {
	case data.Metadata != nil:
		if contentSent {
			// Merge provided metadata AND strip guard_checked_at in one expression.
			metaExpr = fmt.Sprintf("$%d::jsonb - 'guard_checked_at'", len(args)+1)
		} else {
			metaExpr = fmt.Sprintf("$%d::jsonb", len(args)+1)
		}
		args = append(args, data.Metadata)
	case data.MetadataMerge != nil:
		set, drop := splitMetadataMerge(data.MetadataMerge)
		metaExpr = fmt.Sprintf("(COALESCE(metadata, '{}'::jsonb) || $%d::jsonb) - $%d::text[]", len(args)+1, len(args)+2)
		args = append(args, set, drop)
		if contentSent {
			metaExpr = "(" + metaExpr + ") - 'guard_checked_at'"
		}
	case contentSent:
		// No explicit metadata update — strip guard_checked_at from existing metadata.
		metaExpr = "metadata - 'guard_checked_at'"
	}
	if data.SensitivityAudit != nil {
		if metaExpr == "" {
			metaExpr = "COALESCE(metadata, '{}'::jsonb)"
		}
		metaExpr = fmt.Sprintf("(%s) || jsonb_build_object('sensitivity_audit', $%d::jsonb)", metaExpr, len(args)+1)
		args = append(args, data.SensitivityAudit)
	}
	return metaExpr, args
}

// splitMetadataMerge turns a merge object into the two arguments of its SQL
// expression (updateMetadataExpr): the keys it sets, with their values, and
// the keys it removes (a null value), sorted. Both are non-nil, because a NULL
// on either side of jsonb's || or - makes the whole metadata NULL.
func splitMetadataMerge(merge map[string]any) (map[string]any, []string) {
	set := make(map[string]any, len(merge))
	drop := []string{}
	for k, v := range merge {
		if v == nil {
			drop = append(drop, k)
			continue
		}
		set[k] = v
	}
	sort.Strings(drop)
	return set, drop
}

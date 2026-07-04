// Package forge — the canonical 3-way hash projection (design/02 §3.6, W16).
//
// base_hash / ctx_hash / forge_hash are sha256 over a CANONICAL JSON projection,
// NEVER over raw payloads: volatile fields (reactions, updated_at) would drift
// forever, and a TIMESTAMP comparison would fire on every re-fetch (the W16 trap
// the I-G idempotency gate ropes off — a re-run with unchanged content must be
// 0 writes / 0 conflicts).
//
//	Issue:   {title, body, state, labels: sorted, assignees: sorted, milestone}
//	Comment: {body}
//
// TITLE RULE (drift-killer, §3.6): `title` in the projection is the forge title
// WITHOUT the ctx "#<nr>"/"#L<seq>" prefix. The block title is a DERIVAT; the
// ctx-side projection strips the prefix deterministically before hashing. Without
// the strip, EVERY pulled issue would have ctxH != forgeH != base forever — 10k
// permanent conflict rows (push off) or a title-corrupting push storm (push on).
// The gate proves it: a projection over the RAW block title goes RED.
//
// STATE RULE (§3.6/§4.5.4): the forge side hashes IssueRemote.State (open/closed).
// The ctx side derives it from workflow_status via terminal-set membership when
// the registry is resolvable (a terminal status ⇒ "closed", else "open" — GitHub
// state is binary), and falls back to metadata.forge_state when it is not (the
// registry-less pull writes only metadata.forge_state, §4.5.4). Both paths make a
// freshly pulled issue project to exactly its forge state ⇒ ctxH == base.
package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"

	"github.com/GottZ/ctx/internal/store"
)

// issuePrefixRe / commentPrefixRe strip the ctx number prefix before hashing
// (§3.6). Issues: "#L?<n> "; comments: "#<n>.cL?<n> ". "#L?" matches both the
// forge "#12" and the local-draft "#L12" forms.
var (
	issuePrefixRe   = regexp.MustCompile(`^#L?\d+\s`)
	commentPrefixRe = regexp.MustCompile(`^#\d+\.cL?\d+\s`)
)

func stripIssuePrefix(title string) string {
	return issuePrefixRe.ReplaceAllString(title, "")
}

func stripCommentPrefix(title string) string {
	return commentPrefixRe.ReplaceAllString(title, "")
}

// issueProjection is the ordered, canonical shape hashed for an issue. Struct
// (not map) so Go's field-declaration order gives a deterministic byte layout;
// slices are sorted + non-nil so two equal sets hash identically.
type issueProjection struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
	Milestone string   `json:"milestone"`
}

func hashIssueProjection(p issueProjection) string {
	p.Labels = sortedNonNil(p.Labels)
	p.Assignees = sortedNonNil(p.Assignees)
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func hashCommentProjection(body string) string {
	raw, _ := json.Marshal(struct {
		Body string `json:"body"`
	}{Body: body})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ForgeIssueHash builds forge_hash from a fetched IssueRemote and returns the
// capped body (+truncated flag) so the caller stores EXACTLY the bytes that were
// hashed (§5.5 truncation runs before the hash, else a >50 KB issue drifts).
func ForgeIssueHash(iss IssueRemote) (hash, cappedBody string, truncated bool) {
	cappedBody, truncated = store.CapForgeBody(iss.Body)
	hash = hashIssueProjection(issueProjection{
		Title:     iss.Title,
		Body:      cappedBody,
		State:     iss.State,
		Labels:    iss.Labels,
		Assignees: iss.Assignees,
		Milestone: iss.Milestone,
	})
	return hash, cappedBody, truncated
}

// ForgeCommentHash builds forge_hash for a comment (projection {body}) and
// returns the capped body it hashed.
func ForgeCommentHash(body string) (hash, cappedBody string, truncated bool) {
	cappedBody, truncated = store.CapForgeBody(body)
	return hashCommentProjection(cappedBody), cappedBody, truncated
}

// CtxIssueHash builds ctx_hash from a stored issue block. terminalSet is the
// type's terminal status set and registryOK reports whether the registry resolved
// the issue workflow — together they pick the state-derivation rule (see the
// package doc). Title is prefix-stripped; body is (defensively) re-capped so a
// block that predates the cap still projects to the capped bytes.
func CtxIssueHash(b *store.Block, terminalSet []string, registryOK bool) string {
	body, _ := store.CapForgeBody(b.Content)
	return hashIssueProjection(issueProjection{
		Title:     stripIssuePrefix(b.Title),
		Body:      body,
		State:     ctxForgeState(b, terminalSet, registryOK),
		Labels:    metaStrings(b.Metadata, "labels"),
		Assignees: metaStrings(b.Metadata, "assignees"),
		Milestone: metaString(b.Metadata, "milestone"),
	})
}

// CtxCommentHash builds ctx_hash for a stored comment block ({body}).
func CtxCommentHash(b *store.Block) string {
	body, _ := store.CapForgeBody(b.Content)
	return hashCommentProjection(body)
}

// ctxForgeState resolves the projection state field from a stored block: registry
// resolvable + a set workflow_status ⇒ terminal-membership (binary open/closed);
// otherwise the last-synced metadata.forge_state (the §4.5.4 fallback).
func ctxForgeState(b *store.Block, terminalSet []string, registryOK bool) string {
	if registryOK && b.WorkflowStatus != "" {
		for _, t := range terminalSet {
			if t == b.WorkflowStatus {
				return "closed"
			}
		}
		return "open"
	}
	return metaString(b.Metadata, "forge_state")
}

// sortedNonNil returns a sorted copy of in, or an empty (non-nil) slice for a nil
// input, so equal sets hash identically regardless of source order/nilness.
func sortedNonNil(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

// metaStrings reads a []string from a JSONB metadata field, tolerating both the
// native []string and the pgx-decoded []any{string,…} form.
func metaStrings(meta map[string]any, key string) []string {
	if meta == nil {
		return nil
	}
	switch v := meta[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if s, ok := meta[key].(string); ok {
		return s
	}
	return ""
}

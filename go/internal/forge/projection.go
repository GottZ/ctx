// projection.go — the canonical 3-way hash projection (design/02 §3.6, W16).
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

// issuePrefixRe strips the ctx number prefix from an issue title before hashing
// (§3.6): "#L?<n> " — "#L?" matches both the forge "#12" and the local-draft
// "#L12" forms.
var issuePrefixRe = regexp.MustCompile(`^#L?\d+\s`)

func stripIssuePrefix(title string) string {
	return issuePrefixRe.ReplaceAllString(title, "")
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

// commentProjection is the ordered canonical shape hashed for a comment ({body}).
// A struct (mirroring issueProjection) so the base-field snapshot the push diff
// stores (§4.5.2) round-trips byte-identically through json.Unmarshal.
type commentProjection struct {
	Body string `json:"body"`
}

// issueProjectionJSON returns BOTH the canonical bytes and their sha256. The push
// path (I-H) stores these bytes as the mapping's base-field snapshot so a later
// field-diff compares VALUES, not just the opaque hash; the hash is what the 3-way
// matrix (§4.5.2) compares. Labels/assignees are sorted here (single source of
// truth) so the stored snapshot and every re-projection agree.
func issueProjectionJSON(p issueProjection) (json.RawMessage, string) {
	p.Labels = sortedNonNil(p.Labels)
	p.Assignees = sortedNonNil(p.Assignees)
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

func commentProjectionJSON(body string) (json.RawMessage, string) {
	raw, _ := json.Marshal(commentProjection{Body: body})
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:])
}

// ── base-field snapshots (Welle I-H push field-diff, §4.5.2) ─────────────────
// The 3-way matrix compares HASHES. A push, however, must send only the CHANGED
// fields (data-loss guard: a status flip must not carry the body; a truncated
// body must never be pushed). That needs the last-synced field VALUES, so every
// base write also persists the canonical projection JSON on the mapping row
// (context_project_sync_map.metadata.base_fields). These *Base helpers return
// (fields JSON, hash) together so the writer stores a snapshot whose hash is the
// base_hash by construction. issueBase/commentBase parse a stored snapshot back.

// ForgeIssueBase returns the forge issue's canonical field snapshot + hash (base
// after a pull) alongside the capped body (+truncated flag), so the caller stores
// EXACTLY the bytes that were hashed (§5.5: the cap runs BEFORE the hash, else a
// >50 KB issue drifts).
func ForgeIssueBase(iss IssueRemote) (fields []byte, hash, cappedBody string, truncated bool) {
	cappedBody, truncated = store.CapForgeBody(iss.Body)
	raw, h := issueProjectionJSON(issueProjection{
		Title: iss.Title, Body: cappedBody, State: iss.State,
		Labels: iss.Labels, Assignees: iss.Assignees, Milestone: iss.Milestone,
	})
	return raw, h, cappedBody, truncated
}

// CtxIssueBase returns the stored issue block's canonical field snapshot + hash
// (base after a push/convergence). terminalSet is the type's terminal status set
// and registryOK reports whether the registry resolved the issue workflow —
// together they pick the state-derivation rule (see the package doc). Title is
// prefix-stripped; body is (defensively) re-capped so a block that predates the
// cap still projects to the capped bytes.
func CtxIssueBase(b *store.Block, terminalSet []string, registryOK bool) (fields []byte, hash string) {
	body, _ := store.CapForgeBody(b.Content)
	return issueProjectionJSON(issueProjection{
		Title:     stripIssuePrefix(b.Title),
		Body:      body,
		State:     ctxForgeState(b, terminalSet, registryOK),
		Labels:    metaStrings(b.Metadata, "labels"),
		Assignees: metaStrings(b.Metadata, "assignees"),
		Milestone: metaString(b.Metadata, "milestone"),
	})
}

// ForgeCommentBase / CtxCommentBase are the comment ({body}) snapshots.
func ForgeCommentBase(body string) (fields []byte, hash, cappedBody string, truncated bool) {
	cappedBody, truncated = store.CapForgeBody(body)
	raw, h := commentProjectionJSON(cappedBody)
	return raw, h, cappedBody, truncated
}

func CtxCommentBase(b *store.Block) (fields []byte, hash string) {
	body, _ := store.CapForgeBody(b.Content)
	return commentProjectionJSON(body)
}

// parseIssueProjection decodes a stored base-field
// snapshot; a missing/garbled snapshot yields ok=false (the push then falls back
// to a full-projection push, and refuses to push a truncated body it cannot prove
// unchanged — the fail-closed side of §4.5.2).
func parseIssueProjection(fields []byte) (issueProjection, bool) {
	var p issueProjection
	if len(fields) == 0 {
		return p, false
	}
	if err := json.Unmarshal(fields, &p); err != nil {
		return p, false
	}
	return p, true
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

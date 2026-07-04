// Package forge is the pull-only forge abstraction for the ctx workflow engine
// (Achse 02, Welle I-F; design/02-issue-workflow.md §4.5). It fetches issues and
// comments from a git forge (GitHub first) with conditional requests, cursor
// pagination and rate-limit respect, and drives a per-project sync SHELL
// (run-state, fail-closed gates, offline-first backoff).
//
// WAVE BOUNDARY (I-F vs I-G): this package fetches and gates. It does NOT create
// blocks, write context_project_sync_map rows, or run the 3-way direction logic
// — that Pull-APPLY is Welle I-G (design/02 §7), wired through the IssueApplyFunc
// seam (nil ⇒ the I-F fetch-only no-op). The mapping row's block_id is NOT NULL,
// so a mapping-persist without block creation is structurally impossible; the
// im-Zweifel "I-F = Mapping-Persist" fallback therefore does not apply here.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package forge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrNotModified is returned by the List* methods on a 304 (a conditional
// request that hit the stored ETag): nothing changed since the last sync, so the
// run does 0 writes and leaves the cursor untouched (§4.5.1, GitHub best
// practice — a 304 does not count against the primary rate limit).
var ErrNotModified = errors.New("forge: not modified (304)")

// RateLimitError signals a GitHub primary OR secondary rate-limit hit (403 with
// Retry-After, or x-ratelimit-remaining:0). It is NEVER a conflict (§4.5.3): the
// sync engine stamps backoff_until (exponential, cap 1h) and aborts the run
// cleanly, then local work continues and the next tick retries.
type RateLimitError struct {
	RetryAfter time.Duration // from Retry-After; 0 = unknown (fall back to exponential)
	Reset      time.Time     // x-ratelimit-reset, if present (telemetry)
	Message    string
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("forge: rate limited (retry after %s)", e.RetryAfter)
	}
	return "forge: rate limited"
}

// RepoRef identifies a repo on a forge. APIBase is validated PATCH-time
// (handler.validateForge, §5.7) AND enforced dial-time against the SSRF deny-list
// (ssrf.go) — the two-layer guard closes DNS rebinding. "" → https://api.github.com.
type RepoRef struct {
	Owner   string
	Repo    string
	APIBase string
}

// IssueRemote is the forge projection of one issue as fetched (raw, pre-apply).
// The canonical hash projection (§3.6) is built by I-G from these fields; I-F
// only carries them.
type IssueRemote struct {
	Number    int64
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	Assignees []string
	Milestone string
	UpdatedAt time.Time
}

// IssuePage is one fetched page of issues. Pull requests are filtered out by the
// client BEFORE return (§4.5.1) — a caller never sees a PR; PRsSkipped counts the
// drops for the §6.1 budget telemetry. ETag is the listing response's ETag (store
// it for the next 304 probe). NextURL is the Link rel="next" target; "" = last.
type IssuePage struct {
	Issues     []IssueRemote
	ETag       string
	NextURL    string
	PRsSkipped int
}

// CommentRemote is the forge projection of one issue comment (pre-apply).
type CommentRemote struct {
	ID          int64
	IssueNumber int64
	Body        string
	UpdatedAt   time.Time
}

// CommentPage is one fetched page of comments.
type CommentPage struct {
	Comments []CommentRemote
	ETag     string
	NextURL  string
}

// Forge is the pull-only forge abstraction (I-F). The FIRST-page methods take a
// since cursor + ETag (conditional); the *Page follow methods take an absolute
// Link-header URL (strict pagination — GitHub docs: "use the link headers", never
// hand-built page queries). Push methods (CreateIssue/UpdateIssue/…) are Welle
// I-H and are deliberately absent — I-H widens this interface additively.
type Forge interface {
	ListIssuesSince(ctx context.Context, repo RepoRef, since time.Time, etag string) (IssuePage, error)
	ListIssuesPage(ctx context.Context, pageURL string) (IssuePage, error)
	ListCommentsSince(ctx context.Context, repo RepoRef, since time.Time, etag string) (CommentPage, error)
	ListCommentsPage(ctx context.Context, pageURL string) (CommentPage, error)
}

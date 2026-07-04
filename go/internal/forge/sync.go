// Forge sync SHELL (design/02 §4.5.5, S7/S8): per-project run-state (single-flight
// ⇒ 409 on double-start), fail-closed gates (tenant + issue-policy), the fetch
// loop with offline-first backoff, and the Pull-APPLY seam (I-G). It is the
// on-demand engine behind the forge-sync-start/-status manage actions and, later,
// the periodic scheduler loop — both drive THIS type (audit.go run-state pattern).
package forge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// backoffBase/backoffCap bound the exponential offline-first backoff
	// (§4.5.3): 1m, 2m, 4m … capped at 1h. A GitHub Retry-After overrides it.
	backoffBase = time.Minute
	backoffCap  = time.Hour
	// tokenSecretPrefix names the per-project sealed PAT secret in the PROJECT
	// scope (mirrors the webhook.github.<id> convention, §5.4). reveal-never.
	tokenSecretPrefix = "forge.token."
	// defaultMaxConcurrent bounds the daemon-wide count of in-flight syncs
	// (project.sync.max_concurrent, §4.4). NewSyncManager seeds this; SetMaxConcurrent
	// overrides it from config at boot.
	defaultMaxConcurrent = 3
)

// Sentinel errors surfaced to the manage handler (mapped to 409/422/…).
var (
	ErrSyncRunning     = errors.New("forge sync already running for this project")
	ErrSyncSaturated   = errors.New("forge sync concurrency limit reached")                                    // global semaphore full (§4.4 → 409 + retry_after_s)
	ErrNoTenant        = errors.New("scope has no owning tenant")                                              // found=false gate (S13)
	ErrTenantSuspended = errors.New("owning tenant is suspended")                                              // skip, not proceed
	ErrIssuePolicy     = errors.New("issue type policy not resolvable — deploy Achse-01 T3 + Welle I-C first") // §6.4
	ErrForgeKind       = errors.New("unsupported forge kind (github only)")
)

// ApplyResult is what a Pull-APPLY hook reports back to the run (I-G).
type ApplyResult struct {
	Applied   int
	Conflicts int
}

// IssueApplyFunc applies one fetched page of issues to the corpus — Welle I-G
// (block create/update + context_project_sync_map write + 3-way base_hash). I-F
// wires nil ⇒ the fetch-only no-op: the run fetches, counts and gates but writes
// no blocks and advances no durable fetch cursor (§7 boundary). I-G supplies this
// AND takes over etag/since cursor commit per page.
type IssueApplyFunc func(ctx context.Context, project store.ProjectRow, issues []IssueRemote) (ApplyResult, error)

// CommentApplyFunc applies one fetched page of comments — Welle I-G (comment
// block create/update + mapping + {body} 3-way, §3.6/§4.5.2). nil ⇒ the run does
// not pull comments (I-F fetch-only / dry-run). The comment leg runs AFTER the
// issue leg so a comment's parent issue mapping already exists (a comment whose
// parent is not yet pulled is skipped and picked up on a later run).
type CommentApplyFunc func(ctx context.Context, project store.ProjectRow, comments []CommentRemote) (ApplyResult, error)

// PushFunc drains ctx-ahead entities back onto the forge — Welle I-H
// (Pusher.PushProject). nil ⇒ no push (I-F/I-G / dry-run). allow() gates each
// content-POST against the token-scoped throttle. It runs AFTER both pull legs
// (§4.5.3: "on return the next run drains ctx-ahead via push").
type PushFunc func(ctx context.Context, project store.ProjectRow, client Forge, repo RepoRef, allow func() bool) (PushResult, error)

// SyncStatus is the in-memory state of the current/last run for one project
// (forge-sync-status surface). DB-side history (last run row, conflict count) is
// merged by the handler.
type SyncStatus struct {
	ProjectID  string    `json:"project_id"`
	Running    bool      `json:"running"`
	DryRun     bool      `json:"dry_run"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Fetched    int       `json:"fetched"`     // issues fetched (PRs excluded)
	PRsSkipped int       `json:"prs_skipped"` // pull_request items dropped (§6.1)
	Pages      int       `json:"pages"`
	Applied    int       `json:"applied"`   // I-G writes; 0 in I-F (no-op apply)
	Pushed     int       `json:"pushed"`    // I-H: entities written back to the forge
	Conflicts  int       `json:"conflicts"` // I-G pull + I-H push (truncated data-loss guard)
	Aborted    bool      `json:"aborted"`
	BackoffSet bool      `json:"backoff_set"`
	RunID      string    `json:"run_id,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

// TenantStatusFn resolves the owning tenant's status for a scope; found=false ⇒
// no tenant (the fail-closed skip, §4.5.5). Bound to store.TenantStatusForScope.
type TenantStatusFn func(ctx context.Context, scope string) (status string, found bool, err error)

// IssuePolicyFn reports whether the issue type policy is resolvable for a scope
// (with digest.include=false, §6.4). reason is a human message for the refusal.
// Bound in server.go over the block-type registry.
type IssuePolicyFn func(ctx context.Context, scope string) (ok bool, reason string)

// SyncManager owns run-state and the gates. It is the ForgeController the manage
// handler consumes.
type SyncManager struct {
	pool          *pgxpool.Pool
	openBox       func() (*sealbox.Box, error)
	newForge      func(token string) Forge
	tenantStatus  TenantStatusFn
	issuePolicy   IssuePolicyFn
	applyIssues   IssueApplyFunc   // nil ⇒ I-F fetch-only no-op (I-G wires it)
	applyComments CommentApplyFunc // nil ⇒ no comment pull (I-G wires it)
	push          PushFunc         // nil ⇒ no push (I-H wires it)
	throttle      *Throttle        // token-scoped content-POST limiter (§6.1)
	clock         func() time.Time

	mu   sync.Mutex
	runs map[string]*SyncStatus

	// sem is the process-global concurrency semaphore (project.sync.max_concurrent,
	// §4.4): a buffered channel whose capacity is the slot count. nil ⇒ unlimited
	// (test wiring built via struct literal). Sized once at boot (NewSyncManager /
	// SetMaxConcurrent), before any sync starts — no resize race.
	sem chan struct{}
}

// NewSyncManager builds the manager with the production GitHub client factory and
// the sealbox from env. applyIssues is nil (I-F fetch-only) until I-G calls
// SetApplyIssues. tenantStatus/issuePolicy are the fail-closed gates.
func NewSyncManager(pool *pgxpool.Pool, tenantStatus TenantStatusFn, issuePolicy IssuePolicyFn) *SyncManager {
	return &SyncManager{
		pool:         pool,
		openBox:      sealbox.FromEnv,
		newForge:     NewGitHubClient,
		tenantStatus: tenantStatus,
		issuePolicy:  issuePolicy,
		throttle:     NewThrottle(),
		clock:        time.Now,
		runs:         make(map[string]*SyncStatus),
		sem:          make(chan struct{}, defaultMaxConcurrent),
	}
}

// SetMaxConcurrent sizes the process-global concurrency semaphore from config
// (project.sync.max_concurrent, §4.4). Call once at boot BEFORE serving (the
// SetApplyIssues happens-before pattern) — it replaces the channel, so it is not
// safe concurrent with a running sync. n<=0 falls back to the default.
func (m *SyncManager) SetMaxConcurrent(n int) {
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	m.sem = make(chan struct{}, n)
}

// SetApplyIssues wires the I-G Pull-APPLY hook (idempotent; call once at boot).
func (m *SyncManager) SetApplyIssues(fn IssueApplyFunc) { m.applyIssues = fn }

// SetApplyComments wires the I-G comment Pull-APPLY hook (call once at boot).
func (m *SyncManager) SetApplyComments(fn CommentApplyFunc) { m.applyComments = fn }

// SetPush wires the I-H push pass (call once at boot).
func (m *SyncManager) SetPush(fn PushFunc) { m.push = fn }

// SetToken seals a PAT for the project and records its ref name (never the PAT,
// §5.4). The seal + the token_secret ref update commit in ONE transaction. The
// secret lives in the PROJECT scope so its AAD binds it to that scope.
func (m *SyncManager) SetToken(ctx context.Context, project store.ProjectRow, plaintext string) error {
	if plaintext == "" {
		return fmt.Errorf("token is required")
	}
	box, err := m.openBox()
	if err != nil {
		return fmt.Errorf("secrets unavailable: %w", err)
	}
	name := tokenSecretPrefix + project.ID
	nonce, ct, err := box.Seal(name, project.Scope, []byte(plaintext))
	if err != nil {
		return fmt.Errorf("seal token: %w", err)
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.PutSecret(ctx, tx, name, project.Scope, nonce, ct, 1, nzp(project.CreatedBy)); err != nil {
		return fmt.Errorf("persist token: %w", err)
	}
	if err := store.SetProjectToken(ctx, tx, project.ID, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nzp(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// Status returns the in-memory run status for a project (copy). Missing = zero.
func (m *SyncManager) Status(projectID string) SyncStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.runs[projectID]; ok {
		return *st
	}
	return SyncStatus{ProjectID: projectID}
}

// StartSync runs the fail-closed gates synchronously (so the caller gets the
// refusal), then launches the fetch loop in a goroutine. It returns ErrSyncRunning
// on a second concurrent run of the SAME project (S7 409), ErrNoTenant /
// ErrTenantSuspended (S13) or ErrIssuePolicy (§6.4) when a gate refuses — in every
// refusal case ZERO wire calls have been made and no goroutine is launched.
func (m *SyncManager) StartSync(ctx context.Context, project store.ProjectRow, dryRun bool) (SyncStatus, error) {
	m.mu.Lock()
	if st, ok := m.runs[project.ID]; ok && st.Running {
		m.mu.Unlock()
		return SyncStatus{}, ErrSyncRunning
	}

	// Gate A — tenant (fail-closed, §4.5.5/S13). found=false ⇒ disable sync +
	// stamp last_error, NEVER proceed (would re-materialise blocks into an
	// owner-less scope). Held under the run-state lock so the disable is atomic
	// with the refusal. NO forge client exists yet ⇒ 0 wire calls.
	status, found, err := m.tenantStatus(ctx, project.Scope)
	if err != nil {
		m.mu.Unlock()
		return SyncStatus{}, err
	}
	if !found {
		m.mu.Unlock()
		off := false
		msg := ErrNoTenant.Error()
		_ = store.SetProjectSyncState(ctx, m.pool, project.ID, store.SyncStatePatch{
			SyncEnabled: &off, LastError: &msg, SyncStatus: strptr("error"),
		})
		return SyncStatus{}, ErrNoTenant
	}
	if status != "active" {
		m.mu.Unlock()
		return SyncStatus{}, ErrTenantSuspended
	}

	// Gate B — issue policy resolvable with digest.include=false (§6.4). 0 wire calls.
	if ok, reason := m.issuePolicy(ctx, project.Scope); !ok {
		m.mu.Unlock()
		return SyncStatus{}, fmt.Errorf("%w (%s)", ErrIssuePolicy, reason)
	}

	// Gate C — global concurrency semaphore (§4.4). The per-project single-flight
	// above stops a double-start of the SAME project (ErrSyncRunning); THIS bounds
	// the daemon-wide count so one tenant's 10k import cannot serialise every other
	// project/tenant. A full semaphore ⇒ ErrSyncSaturated (the handler answers 409 +
	// retry_after_s — a queue would be hidden state, sync is idempotently retriable).
	// Held under mu (a non-blocking try is instant); released in finish() 1:1.
	if m.sem != nil {
		select {
		case m.sem <- struct{}{}:
		default:
			m.mu.Unlock()
			return SyncStatus{}, ErrSyncSaturated
		}
	}

	st := &SyncStatus{ProjectID: project.ID, Running: true, DryRun: dryRun, StartedAt: m.clock()}
	m.runs[project.ID] = st
	m.mu.Unlock()

	run, err := store.StartSyncRun(ctx, m.pool, project.ID)
	if err != nil {
		m.finish(project.ID, "", "start run: "+err.Error(), true, false)
		return SyncStatus{}, err
	}
	m.setRunID(project.ID, run.ID)

	go m.runSync(context.WithoutCancel(ctx), project, dryRun, run.ID)
	return m.Status(project.ID), nil
}

// runSync is the background fetch loop. It never panics the process (§4.5.5
// recover), records the run row on exit, and treats every wire/rate-limit error
// as backoff — never a conflict.
func (m *SyncManager) runSync(ctx context.Context, project store.ProjectRow, dryRun bool, runID string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("forge: sync run panicked", "project", project.ID, "panic", r, "stack", string(debug.Stack()))
			m.finish(project.ID, runID, "panic during sync", true, false)
		}
	}()

	token, err := m.resolveToken(ctx, project)
	if err != nil {
		m.finish(project.ID, runID, "token resolve failed", true, false)
		return
	}
	repo, err := repoRefFromForge(project.Forge)
	if err != nil {
		m.finish(project.ID, runID, err.Error(), true, false)
		return
	}
	client := m.newForge(token)

	etag, since, backoffN := readCursor(project.SyncCursor)

	// Issue leg: fetch + 3-way apply per page, committing the etag/since cursor
	// AFTER each successful page apply (I-G — I-F left the durable advance to us).
	// A wire OR apply error backs off with the cursor UN-advanced (resume re-fetches
	// from the last committed page, re-applies idempotently — no skip).
	if err := m.pullIssues(ctx, project, client, repo, dryRun, etag, since); err != nil {
		if errors.Is(err, ErrNotModified) {
			// 304 on the FIRST issue page: nothing changed ⇒ issue cursor untouched.
			// Comments may still have changed, so fall through to the comment leg.
		} else {
			m.handleWireError(ctx, project.ID, runID, backoffN, err)
			return
		}
	}

	// Comment leg (I-G): only when the hook is wired and this is not a dry run.
	if m.applyComments != nil && !dryRun {
		cEtag, cSince := readCommentCursor(project.SyncCursor)
		if err := m.pullComments(ctx, project, client, repo, cEtag, cSince); err != nil && !errors.Is(err, ErrNotModified) {
			m.handleWireError(ctx, project.ID, runID, backoffN, err)
			return
		}
	}

	// Push leg (I-H): drain ctx-ahead entities back onto the forge (§4.5.3). Gated
	// by push_enabled (§5.6, fail-closed) AND a resolved token (a write needs auth
	// — no token ⇒ 0 wire writes). The throttle is keyed on the CREDENTIAL (forge
	// kind + PAT hash), so repos sharing one PAT share the content-POST bucket
	// (§6.1). A push wire error backs off like a pull; a dry bucket just stops the
	// pass (batch-wise, the rest drain next run).
	if m.push != nil && !dryRun && project.PushEnabled && token != "" {
		key := pushThrottleKey(project.Forge, token)
		pres, err := m.push(ctx, project, client, repo, func() bool { return m.throttle.Allow(key) })
		if err != nil {
			m.handleWireError(ctx, project.ID, runID, backoffN, err)
			return
		}
		m.mu.Lock()
		if st := m.runs[project.ID]; st != nil {
			st.Pushed += pres.Pushed
			st.Conflicts += pres.Conflicts
		}
		m.mu.Unlock()
	}

	// SUCCESS. Backoff/error are cleared and last_sync_at stamped; backoff_n reset.
	// The etag/since fetch cursors were advanced per page inside the legs above (I-F
	// with applyIssues==nil advances nothing — no apply happened).
	_ = store.SetProjectSyncState(ctx, m.pool, project.ID, store.SyncStatePatch{
		SyncStatus: strptr("idle"), ClearError: true, ClearBackoff: true, SetLastSync: true,
	})
	_ = store.MergeProjectSyncCursor(ctx, m.pool, project.ID, json.RawMessage(`{"backoff_n":0}`))
	m.finish(project.ID, runID, "", false, false)
}

// pullIssues runs the issue fetch loop: each page is 3-way applied, then the
// etag/since cursor is durably committed (only when a real apply hook is wired and
// not a dry run — I-F's no-op advances nothing, §7 boundary). A 304 on the first
// page returns ErrNotModified (clean, cursor untouched); any other error aborts
// the leg with the cursor at its last committed page.
func (m *SyncManager) pullIssues(ctx context.Context, project store.ProjectRow, client Forge, repo RepoRef, dryRun bool, etag string, since time.Time) error {
	page, err := client.ListIssuesSince(ctx, repo, since, etag)
	if err != nil {
		return err
	}
	firstETag := page.ETag
	var maxSeen time.Time
	for {
		if err := m.applyPage(ctx, project, dryRun, page); err != nil {
			return err // cursor NOT advanced for this page (commit is below the apply)
		}
		m.addPageStats(project.ID, page)
		if m.applyIssues != nil && !dryRun {
			if mu := maxIssueUpdated(page); mu.After(maxSeen) {
				maxSeen = mu
			}
			if !maxSeen.IsZero() {
				m.commitFetchCursor(ctx, project.ID, "issues", firstETag, maxSeen)
			}
		}
		if page.NextURL == "" {
			return nil
		}
		if page, err = client.ListIssuesPage(ctx, page.NextURL); err != nil {
			return err
		}
	}
}

// pullComments runs the comment fetch loop (I-G): each page is 3-way applied over
// the {body} projection, then the comment etag/since cursor is committed per page.
func (m *SyncManager) pullComments(ctx context.Context, project store.ProjectRow, client Forge, repo RepoRef, etag string, since time.Time) error {
	page, err := client.ListCommentsSince(ctx, repo, since, etag)
	if err != nil {
		return err
	}
	firstETag := page.ETag
	var maxSeen time.Time
	for {
		res, err := m.applyComments(ctx, project, page.Comments)
		if err != nil {
			return err
		}
		m.mu.Lock()
		if st := m.runs[project.ID]; st != nil {
			st.Applied += res.Applied
			st.Conflicts += res.Conflicts
		}
		m.mu.Unlock()
		if mu := maxCommentUpdated(page); mu.After(maxSeen) {
			maxSeen = mu
		}
		if !maxSeen.IsZero() {
			m.commitFetchCursor(ctx, project.ID, "comments", firstETag, maxSeen)
		}
		if page.NextURL == "" {
			return nil
		}
		if page, err = client.ListCommentsPage(ctx, page.NextURL); err != nil {
			return err
		}
	}
}

// commitFetchCursor durably advances the etag/since fetch cursor for one leg
// (leg = "issues" | "comments") after a successful page apply. jsonb `||` replaces
// only that leg's sub-object; backoff_n and the other leg survive.
func (m *SyncManager) commitFetchCursor(ctx context.Context, projectID, leg, etag string, since time.Time) {
	patch, err := json.Marshal(map[string]any{leg: map[string]any{"etag": etag, "since": since}})
	if err != nil {
		return
	}
	_ = store.MergeProjectSyncCursor(ctx, m.pool, projectID, patch)
}

// maxIssueUpdated / maxCommentUpdated return the newest updated_at of a page — the
// since boundary to commit (GitHub returns items updated at/after `since`, so a
// resume re-fetches the boundary item, applied idempotently). Zero when empty.
func maxIssueUpdated(page IssuePage) time.Time {
	var mx time.Time
	for _, iss := range page.Issues {
		if iss.UpdatedAt.After(mx) {
			mx = iss.UpdatedAt
		}
	}
	return mx
}

func maxCommentUpdated(page CommentPage) time.Time {
	var mx time.Time
	for _, c := range page.Comments {
		if c.UpdatedAt.After(mx) {
			mx = c.UpdatedAt
		}
	}
	return mx
}

// applyPage streams one page to the Pull-APPLY hook (I-G). On I-F (nil hook) it is
// a no-op — the page is fetched and counted, nothing is written. dryRun also skips
// apply (fetch-only preview).
func (m *SyncManager) applyPage(ctx context.Context, project store.ProjectRow, dryRun bool, page IssuePage) error {
	if m.applyIssues == nil || dryRun {
		return nil
	}
	res, err := m.applyIssues(ctx, project, page.Issues)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if st := m.runs[project.ID]; st != nil {
		st.Applied += res.Applied
		st.Conflicts += res.Conflicts
	}
	m.mu.Unlock()
	return nil
}

// handleWireError stamps backoff_until (Retry-After if the forge gave one, else
// exponential cap 1h) + last_error, bumps backoff_n, and finishes the run as a
// clean abort (NEVER a conflict, §4.5.3).
func (m *SyncManager) handleWireError(ctx context.Context, projectID, runID string, backoffN int, err error) {
	var rl *RateLimitError
	var until time.Time
	switch {
	case errors.As(err, &rl) && rl.RetryAfter > 0:
		until = m.clock().Add(rl.RetryAfter)
	default:
		until = m.clock().Add(expBackoff(backoffN))
	}
	msg := sanitizeErr(err)
	_ = store.SetProjectSyncState(ctx, m.pool, projectID, store.SyncStatePatch{
		SyncStatus: strptr("error"), LastError: &msg, BackoffUntil: &until,
	})
	_ = store.MergeProjectSyncCursor(ctx, m.pool, projectID,
		json.RawMessage(fmt.Sprintf(`{"backoff_n":%d}`, backoffN+1)))
	m.finish(projectID, runID, msg, true, true)
}

// expBackoff = min(base * 2^n, cap). n is the count of prior consecutive failures.
func expBackoff(n int) time.Duration {
	d := backoffBase
	for i := 0; i < n && d < backoffCap; i++ {
		d *= 2
	}
	if d > backoffCap {
		d = backoffCap
	}
	return d
}

// ── run-state helpers ────────────────────────────────────────────────────────.

func (m *SyncManager) setRunID(projectID, runID string) {
	m.mu.Lock()
	if st := m.runs[projectID]; st != nil {
		st.RunID = runID
	}
	m.mu.Unlock()
}

func (m *SyncManager) addPageStats(projectID string, page IssuePage) {
	m.mu.Lock()
	if st := m.runs[projectID]; st != nil {
		st.Fetched += len(page.Issues)
		st.PRsSkipped += page.PRsSkipped
		st.Pages++
	}
	m.mu.Unlock()
}

// finish records the terminal run state in memory AND the DB run row.
func (m *SyncManager) finish(projectID, runID, errMsg string, aborted, backoffSet bool) {
	// Release the global concurrency slot acquired in StartSync (1:1 with a started
	// run; the gate-refusal paths never reach finish). Non-blocking so a defensive
	// stray finish can never deadlock.
	if m.sem != nil {
		select {
		case <-m.sem:
		default:
		}
	}
	m.mu.Lock()
	st := m.runs[projectID]
	if st != nil {
		st.Running = false
		st.FinishedAt = m.clock()
		st.Aborted = aborted
		st.BackoffSet = backoffSet
		st.LastError = errMsg
	}
	statsSnapshot := SyncStatus{}
	if st != nil {
		statsSnapshot = *st
	}
	m.mu.Unlock()

	if runID == "" {
		return
	}
	dbStatus := "done"
	if aborted {
		dbStatus = "error"
	}
	stats, _ := json.Marshal(map[string]int{
		"fetched": statsSnapshot.Fetched, "prs_skipped": statsSnapshot.PRsSkipped,
		"pages": statsSnapshot.Pages, "applied": statsSnapshot.Applied,
		"pushed": statsSnapshot.Pushed, "conflicts": statsSnapshot.Conflicts,
	})
	_ = store.FinishSyncRun(context.Background(), m.pool, runID, dbStatus, errMsg, stats)
}

// resolveToken opens the sealed PAT if the project carries a token ref; "" (no
// ref) = unauth pull. The plaintext is returned in-memory only, never logged.
func (m *SyncManager) resolveToken(ctx context.Context, project store.ProjectRow) (string, error) {
	if project.TokenSecret == nil || *project.TokenSecret == "" {
		return "", nil
	}
	box, err := m.openBox()
	if err != nil {
		return "", err
	}
	pt, err := store.ResolveSecret(ctx, m.pool, box, *project.TokenSecret, project.Scope)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func strptr(s string) *string { return &s }

// sanitizeErr reduces an error to a fixed short string per class — never the URL
// or token material (leak-scan line, §5.4).
func sanitizeErr(err error) string {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return "rate limited"
	}
	if errors.Is(err, ErrForgeKind) {
		return ErrForgeKind.Error()
	}
	return "sync wire error"
}

// forgeCfg is the {kind,owner,repo,api_base?} shape of context_projects.forge.
type forgeCfg struct {
	Kind    string `json:"kind"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	APIBase string `json:"api_base"`
}

// pushThrottleKey builds the token-scoped throttle key (§6.1). The design keys on
// (forge_kind, token_secret NAME, secret scope); under masterplan K14 each project
// seals its OWN secret ("forge.token.<project_id>"), so the NAME is per-repo and
// keying on it would give per-repo buckets — the very bug §6.1 warns of. To
// realise the per-CREDENTIAL intent (the GitHub secondary limit is per PAT), the
// key is the forge kind + a sha256 of the resolved PAT: two repos that sealed the
// SAME PAT hash to the same bucket. The hash is in-memory only, never logged.
func pushThrottleKey(forge json.RawMessage, token string) string {
	kind := "github"
	var f forgeCfg
	if len(forge) > 0 {
		if err := json.Unmarshal(forge, &f); err == nil && f.Kind != "" {
			kind = f.Kind
		}
	}
	sum := sha256.Sum256([]byte(token))
	return kind + "\x00" + hex.EncodeToString(sum[:])
}

func repoRefFromForge(raw json.RawMessage) (RepoRef, error) {
	var f forgeCfg
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &f); err != nil {
			return RepoRef{}, fmt.Errorf("forge: invalid forge config: %w", err)
		}
	}
	if f.Kind != "" && f.Kind != "github" {
		return RepoRef{}, ErrForgeKind
	}
	if f.Owner == "" || f.Repo == "" {
		return RepoRef{}, fmt.Errorf("forge: owner/repo required in forge config")
	}
	return RepoRef{Owner: f.Owner, Repo: f.Repo, APIBase: f.APIBase}, nil
}

// cursor is the sync_cursor JSONB shape this wave reads/writes: an independent
// etag/since leg per entity kind (issues, comments — a comment-only change does
// not bump the issue leg) plus the shared backoff counter.
type cursor struct {
	Issues struct {
		ETag  string    `json:"etag"`
		Since time.Time `json:"since"`
	} `json:"issues"`
	Comments struct {
		ETag  string    `json:"etag"`
		Since time.Time `json:"since"`
	} `json:"comments"`
	BackoffN int `json:"backoff_n"`
}

func readCursor(raw json.RawMessage) (etag string, since time.Time, backoffN int) {
	var c cursor
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c.Issues.ETag, c.Issues.Since, c.BackoffN
}

func readCommentCursor(raw json.RawMessage) (etag string, since time.Time) {
	var c cursor
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	return c.Comments.ETag, c.Comments.Since
}

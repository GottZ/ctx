// GitHub REST v3 pull-only client (design/02 §4.5.1). Conditional requests via
// If-None-Match (304 ⇒ ErrNotModified, 0 writes), a `since` updated-at fetch
// window, strict Link-header pagination, PR filtering (the issues listing returns
// pull requests too — only the pull_request field distinguishes them), and
// rate-limit respect (403 + Retry-After / x-ratelimit-remaining:0 ⇒ RateLimitError,
// never a conflict). Outbound dials pass the §5.7 SSRF guard (internal/ssrfguard).
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/ssrfguard"
)

const (
	defaultAPIBase = "https://api.github.com"
	perPage        = 100 // §6.1 budget: max page size = fewest requests for 10k+ issues
	// bodyCap bounds one response read (defense against a hostile/huge body; a
	// 100-issue page is well under this).
	bodyCap = 32 << 20
)

// githubClient implements Forge against the GitHub REST API. token "" = unauth
// (60 req/h, public repos only — a private repo then 404s, surfaced as a wire
// error → backoff). The PAT is sent as a Bearer header and NEVER logged (§5.4).
type githubClient struct {
	http  *http.Client
	token string
}

// NewGitHubClient builds the production pull client with the SSRF-guarded
// transport. token is the resolved PAT (from the sealbox) or "" for unauth.
func NewGitHubClient(token string) Forge {
	return &githubClient{
		token: token,
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{DialContext: ssrfguard.GuardedDialer().DialContext, ForceAttemptHTTP2: true},
		},
	}
}

func apiBase(repo RepoRef) string {
	if b := strings.TrimRight(repo.APIBase, "/"); b != "" {
		return b
	}
	return defaultAPIBase
}

// send issues req through a copy of c.http that follows a redirect only within
// the origin of req (checkRedirect, #459, #474). net/http alone keeps the
// PAT on a redirect to the same host name on another port or to a subdomain,
// and re-sends a write's body (the issue text) on 307/308 to any host.
// GitHub's own redirects — a renamed or transferred repository answers 301
// (GET) or 307 (write) with /repositories/<id>/... — stay on the origin of the
// api_base and are followed with token and body. A refused hop returns its
// 3xx, which do and doWrite turn into an error. The rule sits here, not on
// the client NewGitHubClient builds, so it holds for every client this type
// is given.
func (c *githubClient) send(req *http.Request) (*http.Response, error) {
	hc := *c.http
	hc.CheckRedirect = checkRedirect
	return hc.Do(req) //nolint:gosec // G704: url = base(api.github.com or PATCH-validated api_base) + owner/repo; SSRF enforced dial-time (internal/ssrfguard), every hop held to the origin of the request.
}

// checkRedirect is httpx.SameOriginRedirect without the http→https upgrade
// that rule lets through for the backend pools (#474). An api_base on http
// whose host upgrades to https has sent the token in clear text on the first
// hop already; following the upgrade hid that on every run, ended the pull
// one page later at a next-page link "on another origin", and turned a write
// answered with 301 or 302 into a GET of the target that reported success.
// The upgrade is refused before a byte of the hop is sent, and the error says
// which setting to change (the API takes no http api_base; such a row was
// written past handler.validateForge).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if err := httpx.SameOriginRedirect(req, via); err != nil {
		return err
	}
	if httpx.OriginOf(req.URL) != httpx.OriginOf(via[0].URL) {
		return errAPIBaseUpgrade // the one hop off the origin SameOriginRedirect follows
	}
	return nil
}

// configError is a forge error whose text says what the operator has to
// change. It is fixed text and at most a status code — never a URL, a token
// or text the remote chose — so sanitizeErr hands it to last_error as it is,
// where the operator reads it (§5.4 holds: nothing in it is token material).
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

var (
	errAPIBaseUpgrade error = &configError{"forge: the host of forge.api_base redirects http to https, which the forge client does not follow: set forge.api_base to its https URL (this request went out over plain http)"}
	errNextOffOrigin  error = &configError{"forge: the next-page link leaves the origin of the api_base; pagination stopped (set forge.api_base to the origin the forge names in its links)"}
)

// errRedirectNotFollowed is the answer of a redirect send did not follow: its
// Location leaves the origin of the request (or it has none). The status is
// named; the Location is not — it is the remote's choice and the URL/token
// never enter an error surface (§5.4).
func errRedirectNotFollowed(status int) error {
	return &configError{fmt.Sprintf("forge: redirect status %d not followed: the forge client follows a redirect only within the origin of the api_base", status)}
}

// withoutURL is err as an error of the forge client: net/http's *url.Error
// names the request URL — owner, repository, the since window, on a next page
// the query the forge chose — and the URL never enters an error surface
// (§5.4). The failure it wraps stays; a configError (a refused upgrade, which
// checkRedirect returns through it) comes back as itself.
func withoutURL(what string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ce *configError
	if errors.As(err, &ce) {
		return ce
	}
	return fmt.Errorf("%s: %w", what, err)
}

// do issues one GET with conditional/auth headers and maps the transport-level
// forge semantics (304, rate limit) onto errors. On a normal 2xx it returns the
// response for the caller to decode. The caller MUST close resp.Body.
func (c *githubClient) do(ctx context.Context, url, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G107: url is base(api.github.com or a PATCH-validated api_base) + owner/repo; SSRF is enforced dial-time by the §5.7 guard (internal/ssrfguard), not on this string.
	if err != nil {
		return nil, withoutURL("forge: build request", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.send(req)
	if err != nil {
		// Never wrap the URL/token into the error surface (leak-scan line, §5.4).
		return nil, withoutURL("forge: request failed", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotModified:
		_ = resp.Body.Close()
		return nil, ErrNotModified
	case isRateLimited(resp):
		rl := rateLimitFrom(resp)
		_ = resp.Body.Close()
		return nil, rl
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		_ = resp.Body.Close()
		return nil, errRedirectNotFollowed(resp.StatusCode)
	case resp.StatusCode >= 400:
		_ = resp.Body.Close()
		return nil, fmt.Errorf("forge: unexpected status %d", resp.StatusCode)
	}
	return resp, nil
}

// isRateLimited reports whether resp is a GitHub rate-limit rejection: a 429, or
// a 403 whose remaining budget is 0 or which carries a Retry-After (secondary
// limit). A plain 403 (permissions) is NOT rate limiting.
func isRateLimited(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return resp.Header.Get("X-RateLimit-Remaining") == "0"
}

func rateLimitFrom(resp *http.Response) *RateLimitError {
	rl := &RateLimitError{Message: "github rate limit"}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
			rl.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	if rs := resp.Header.Get("X-RateLimit-Reset"); rs != "" {
		if epoch, err := strconv.ParseInt(strings.TrimSpace(rs), 10, 64); err == nil {
			rl.Reset = time.Unix(epoch, 0)
		}
	}
	return rl
}

// doWrite issues one POST/PATCH with a JSON body + conditional/auth headers and
// maps the forge rate-limit semantics (§4.5.3) onto RateLimitError (never a
// conflict). It returns the decoded body for the caller (create needs the new
// number/id). The URL/token never enter an error surface (§5.4 leak-scan line).
func (c *githubClient) doWrite(ctx context.Context, method, url string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("forge: marshal write payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body)) //nolint:gosec // G107: url = base(api.github.com or PATCH-validated api_base) + owner/repo; SSRF enforced dial-time (internal/ssrfguard).
	if err != nil {
		return nil, withoutURL("forge: build write request", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.send(req)
	if err != nil {
		return nil, withoutURL("forge: write request failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case isRateLimited(resp):
		return nil, rateLimitFrom(resp)
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, errRedirectNotFollowed(resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, fmt.Errorf("forge: unexpected write status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, bodyCap))
}

// ── push (Welle I-H) ─────────────────────────────────────────────────────────.

func (c *githubClient) CreateIssue(ctx context.Context, repo RepoRef, in IssueCreate) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues", apiBase(repo), repo.Owner, repo.Repo)
	raw, err := c.doWrite(ctx, http.MethodPost, url, in)
	if err != nil {
		return 0, err
	}
	var out struct {
		Number int64 `json:"number"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("forge: decode created issue: %w", err)
	}
	if out.Number <= 0 {
		return 0, fmt.Errorf("forge: created issue has no number")
	}
	return out.Number, nil
}

func (c *githubClient) UpdateIssue(ctx context.Context, repo RepoRef, number int64, p IssuePatch) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d", apiBase(repo), repo.Owner, repo.Repo, number)
	_, err := c.doWrite(ctx, http.MethodPatch, url, p)
	return err
}

func (c *githubClient) CreateComment(ctx context.Context, repo RepoRef, issueNumber int64, body string) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", apiBase(repo), repo.Owner, repo.Repo, issueNumber)
	raw, err := c.doWrite(ctx, http.MethodPost, url, map[string]string{"body": body})
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("forge: decode created comment: %w", err)
	}
	if out.ID <= 0 {
		return 0, fmt.Errorf("forge: created comment has no id")
	}
	return out.ID, nil
}

func (c *githubClient) UpdateComment(ctx context.Context, repo RepoRef, commentID int64, body string) error {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", apiBase(repo), repo.Owner, repo.Repo, commentID)
	_, err := c.doWrite(ctx, http.MethodPatch, url, map[string]string{"body": body})
	return err
}

// ── issues ─────────────────────────────────────────────────────────────────.

// ghIssue is the subset of the GitHub issue JSON we project. PullRequest is a
// PRESENCE marker: the field exists (non-null) IFF the item is a pull request.
type ghIssue struct {
	Number      int64          `json:"number"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	State       string         `json:"state"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Labels      []ghLabel      `json:"labels"`
	Assignees   []ghUser       `json:"assignees"`
	Milestone   *ghMilestone   `json:"milestone"`
	PullRequest map[string]any `json:"pull_request"`
}

type ghLabel struct {
	Name string `json:"name"`
}
type ghUser struct {
	Login string `json:"login"`
}
type ghMilestone struct {
	Title string `json:"title"`
}

func (c *githubClient) ListIssuesSince(ctx context.Context, repo RepoRef, since time.Time, etag string) (IssuePage, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues?state=all&sort=updated&direction=asc&per_page=%d",
		apiBase(repo), repo.Owner, repo.Repo, perPage)
	if !since.IsZero() {
		url += "&since=" + since.UTC().Format(time.RFC3339)
	}
	return c.fetchIssues(ctx, url, etag)
}

func (c *githubClient) ListIssuesPage(ctx context.Context, pageURL string) (IssuePage, error) {
	return c.fetchIssues(ctx, pageURL, "")
}

func (c *githubClient) fetchIssues(ctx context.Context, url, etag string) (IssuePage, error) {
	resp, err := c.do(ctx, url, etag)
	if err != nil {
		return IssuePage{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
	if err != nil {
		return IssuePage{}, fmt.Errorf("forge: read issues body: %w", err)
	}
	var items []ghIssue
	if err := json.Unmarshal(raw, &items); err != nil {
		return IssuePage{}, fmt.Errorf("forge: decode issues: %w", err)
	}

	next, err := nextOnOrigin(url, resp)
	if err != nil {
		return IssuePage{}, err
	}
	page := IssuePage{
		ETag:    resp.Header.Get("ETag"),
		NextURL: next,
		Issues:  make([]IssueRemote, 0, len(items)),
	}
	for _, it := range items {
		if it.PullRequest != nil { // PR, not an issue — drop it (§4.5.1)
			page.PRsSkipped++
			continue
		}
		page.Issues = append(page.Issues, projectIssue(it))
	}
	return page, nil
}

func projectIssue(it ghIssue) IssueRemote {
	iss := IssueRemote{
		Number:    it.Number,
		Title:     it.Title,
		Body:      it.Body,
		State:     it.State,
		UpdatedAt: it.UpdatedAt,
	}
	for _, l := range it.Labels {
		iss.Labels = append(iss.Labels, l.Name)
	}
	for _, a := range it.Assignees {
		iss.Assignees = append(iss.Assignees, a.Login)
	}
	if it.Milestone != nil {
		iss.Milestone = it.Milestone.Title
	}
	return iss
}

// ── comments ───────────────────────────────────────────────────────────────.

type ghComment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
	IssueURL  string    `json:"issue_url"` // .../issues/<number>
}

func (c *githubClient) ListCommentsSince(ctx context.Context, repo RepoRef, since time.Time, etag string) (CommentPage, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/issues/comments?sort=updated&direction=asc&per_page=%d",
		apiBase(repo), repo.Owner, repo.Repo, perPage)
	if !since.IsZero() {
		url += "&since=" + since.UTC().Format(time.RFC3339)
	}
	return c.fetchComments(ctx, url, etag)
}

func (c *githubClient) ListCommentsPage(ctx context.Context, pageURL string) (CommentPage, error) {
	return c.fetchComments(ctx, pageURL, "")
}

func (c *githubClient) fetchComments(ctx context.Context, url, etag string) (CommentPage, error) {
	resp, err := c.do(ctx, url, etag)
	if err != nil {
		return CommentPage{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
	if err != nil {
		return CommentPage{}, fmt.Errorf("forge: read comments body: %w", err)
	}
	var items []ghComment
	if err := json.Unmarshal(raw, &items); err != nil {
		return CommentPage{}, fmt.Errorf("forge: decode comments: %w", err)
	}
	next, err := nextOnOrigin(url, resp)
	if err != nil {
		return CommentPage{}, err
	}
	page := CommentPage{
		ETag:     resp.Header.Get("ETag"),
		NextURL:  next,
		Comments: make([]CommentRemote, 0, len(items)),
	}
	for _, it := range items {
		page.Comments = append(page.Comments, CommentRemote{
			ID:          it.ID,
			IssueNumber: issueNumberFromURL(it.IssueURL),
			Body:        it.Body,
			UpdatedAt:   it.UpdatedAt,
		})
	}
	return page, nil
}

func issueNumberFromURL(u string) int64 {
	i := strings.LastIndex(u, "/")
	if i < 0 || i+1 >= len(u) {
		return 0
	}
	n, _ := strconv.ParseInt(u[i+1:], 10, 64)
	return n
}

// nextOnOrigin is the page resp's Link header names as next (nextTarget,
// RFC 8288), "" when it names none, held to the origin of pageURL — the URL
// the client asked for, by induction the origin of the api_base the first
// page was built from. The next page is requested with the PAT, and its URL
// is the response's choice, so a target on another origin is refused like a
// redirect there (#459): GitHub paginates on api.github.com. A relative
// target resolves against the URL that answered (RFC 8288 §3.1, RFC 3986
// §5.1.3: after a redirect within the origin, the last one); an absolute one
// is requested as the forge wrote it. GitHub docs: pagination MUST follow the
// Link header, never a hand-built ?page= query. The target is not named in an
// error (§5.4).
func nextOnOrigin(pageURL string, resp *http.Response) (string, error) {
	target, err := nextTarget(resp.Header.Values("Link"))
	if err != nil || target == "" {
		return "", err
	}
	from, err := url.Parse(pageURL)
	if err != nil {
		return "", errNextOffOrigin
	}
	ref, err := url.Parse(target)
	if err != nil {
		return "", errLinkUnparseable
	}
	base := from
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL
	}
	next := base.ResolveReference(ref)
	if httpx.OriginOf(from) != httpx.OriginOf(next) {
		return "", errNextOffOrigin
	}
	if ref.IsAbs() {
		return target, nil
	}
	return next.String(), nil
}

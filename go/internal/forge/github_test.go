package forge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a github client against an httptest server (its transport
// dials 127.0.0.1, bypassing the SSRF guard which would otherwise refuse the
// loopback fixture — the guard is exercised separately in ssrf_test.go).
func newTestClient(srv *httptest.Server, token string) *githubClient {
	return &githubClient{token: token, http: srv.Client()}
}

// TestListIssues_NotModified is the 304 gate (§4.5.1): a conditional request that
// hits the stored ETag ⇒ ErrNotModified ⇒ the run does 0 writes. RED-then-GREEN:
// the client sends If-None-Match ONLY because it USES the stored etag; drop that
// (pass etag="") and the server answers 200 with a body (asserted below), i.e.
// the run would process entities — the "rot ohne ETag-Nutzung" proof.
func TestListIssues_NotModified(t *testing.T) {
	var gotINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotINM = r.Header.Get("If-None-Match")
		if gotINM == `"etag-abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"etag-abc"`)
		_, _ = w.Write([]byte(`[{"number":1,"title":"x","state":"open"}]`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "tok")
	repo := RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}

	// WITH the stored etag ⇒ 304.
	_, err := c.ListIssuesSince(context.Background(), repo, time.Time{}, `"etag-abc"`)
	if !errors.Is(err, ErrNotModified) {
		t.Fatalf("with etag: want ErrNotModified, got %v", err)
	}
	if gotINM != `"etag-abc"` {
		t.Fatalf("client did not send If-None-Match (etag not used)")
	}

	// WITHOUT the etag (the red-proof) ⇒ 200 with entities, NOT ErrNotModified.
	page, err := c.ListIssuesSince(context.Background(), repo, time.Time{}, "")
	if err != nil {
		t.Fatalf("without etag: unexpected err %v", err)
	}
	if len(page.Issues) != 1 {
		t.Fatalf("without etag: want 1 issue processed, got %d (proves 304 gate is meaningful)", len(page.Issues))
	}
}

// TestListIssues_PRFilter is the PR gate (§4.5.1): the issues listing returns
// pull requests too (only the pull_request field distinguishes them). RED without
// the filter: the PR would become a phantom issue. GREEN: 0 issues for the PR,
// PRsSkipped counts it.
func TestListIssues_PRFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"number":10,"title":"real issue","state":"open"},
			{"number":11,"title":"a pull request","state":"open","pull_request":{"url":"https://api.github.com/repos/o/r/pulls/11"}},
			{"number":12,"title":"another issue","state":"closed"}
		]`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "")
	page, err := c.ListIssuesSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, time.Time{}, "")
	if err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	if len(page.Issues) != 2 {
		t.Fatalf("want 2 issues (PR filtered), got %d", len(page.Issues))
	}
	if page.PRsSkipped != 1 {
		t.Fatalf("want PRsSkipped=1, got %d", page.PRsSkipped)
	}
	for _, iss := range page.Issues {
		if iss.Number == 11 {
			t.Fatalf("PR #11 leaked into issues")
		}
	}
}

// TestListIssues_Pagination proves strict Link-header pagination: the first page
// exposes NextURL; ListIssuesPage follows it (never a hand-built ?page= query).
func TestListIssues_Pagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "page=2") {
			_, _ = w.Write([]byte(`[{"number":2,"title":"p2","state":"open"}]`))
			return
		}
		w.Header().Set("Link", `<`+srv.URL+`/repos/o/r/issues?page=2>; rel="next", <`+srv.URL+`/x?page=9>; rel="last"`)
		_, _ = w.Write([]byte(`[{"number":1,"title":"p1","state":"open"}]`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "")
	repo := RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}
	p1, err := c.ListIssuesSince(context.Background(), repo, time.Time{}, "")
	if err != nil || len(p1.Issues) != 1 || p1.NextURL == "" {
		t.Fatalf("page1: err=%v issues=%d next=%q", err, len(p1.Issues), p1.NextURL)
	}
	p2, err := c.ListIssuesPage(context.Background(), p1.NextURL)
	if err != nil || len(p2.Issues) != 1 || p2.Issues[0].Number != 2 {
		t.Fatalf("page2: err=%v issues=%d", err, len(p2.Issues))
	}
	if p2.NextURL != "" {
		t.Fatalf("page2 should be last, got next=%q", p2.NextURL)
	}
}

// TestListIssues_RateLimit is the rate-limit gate (§4.5.3): a 403 with
// x-ratelimit-remaining:0 (or Retry-After) ⇒ RateLimitError, NEVER a normal
// error/conflict. Retry-After is parsed for the backoff.
func TestListIssues_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1893456000")
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := newTestClient(srv, "tok")
	_, err := c.ListIssuesSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, time.Time{}, "")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("want RateLimitError, got %v", err)
	}
	if rl.RetryAfter != 42*time.Second {
		t.Fatalf("want RetryAfter=42s, got %s", rl.RetryAfter)
	}
}

// TestListIssues_AuthHeader confirms the PAT is sent as a Bearer header (and only
// when set). The leak-scan (sync_test) proves it never reaches a response/log.
func TestListIssues_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "ghp_secrettoken")
	if _, err := c.ListIssuesSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, time.Time{}, ""); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ghp_secrettoken" {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

// TestCreateIssue_PostsAndReturnsNumber (I-H): CreateIssue POSTs the prefix-free
// title/body and returns the forge number the finalise/rename needs.
func TestCreateIssue_PostsAndReturnsNumber(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"number":42}`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "tok")
	n, err := c.CreateIssue(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL},
		IssueCreate{Title: "Fix it", Body: "steps", Labels: []string{"bug"}})
	if err != nil || n != 42 {
		t.Fatalf("CreateIssue = %d err=%v, want 42", n, err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %s, want POST", gotMethod)
	}
	if !strings.Contains(gotBody, `"title":"Fix it"`) || !strings.Contains(gotBody, `"bug"`) {
		t.Fatalf("create body missing fields: %s", gotBody)
	}
}

// TestUpdateIssue_FieldDiffOmitsBody is the wire half of the truncated gate: an
// IssuePatch carrying only State PATCHes {"state":"closed"} — the body key is
// ABSENT (omitempty). RED if IssuePatch used value fields (a "" body would wire).
func TestUpdateIssue_FieldDiffOmitsBody(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"number":5}`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "tok")
	st := "closed"
	if err := c.UpdateIssue(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, 5,
		IssuePatch{State: &st}); err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %s, want PATCH", gotMethod)
	}
	if !strings.Contains(gotBody, `"state":"closed"`) {
		t.Fatalf("patch missing state: %s", gotBody)
	}
	if strings.Contains(gotBody, `"body"`) {
		t.Fatalf("field-diff PATCH leaked a body field (data-loss guard breached): %s", gotBody)
	}
}

// TestCreateComment_PostsAndReturnsID (I-H): CreateComment POSTs the body to the
// issue's comments endpoint and returns the new comment id.
func TestCreateComment_PostsAndReturnsID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"id":900}`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "tok")
	id, err := c.CreateComment(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, 42, "hi")
	if err != nil || id != 900 {
		t.Fatalf("CreateComment = %d err=%v, want 900", id, err)
	}
	if !strings.HasSuffix(gotPath, "/issues/42/comments") {
		t.Fatalf("comment path = %s, want .../issues/42/comments", gotPath)
	}
}

// TestUpdateIssue_RateLimit: a push PATCH hitting the secondary limit surfaces
// RateLimitError (never a normal error/conflict) so the run backs off (§4.5.3).
func TestUpdateIssue_RateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := newTestClient(srv, "tok")
	st := "open"
	err := c.UpdateIssue(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, 5, IssuePatch{State: &st})
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 30*time.Second {
		t.Fatalf("want RateLimitError RetryAfter=30s, got %v", err)
	}
}

// TestListComments_IssueNumber checks the comment→issue-number extraction from
// the issue_url (the parent link the I-G apply needs).
func TestListComments_IssueNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":555,"body":"hi","issue_url":"https://api.github.com/repos/o/r/issues/77"}]`))
	}))
	defer srv.Close()
	c := newTestClient(srv, "")
	page, err := c.ListCommentsSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, time.Time{}, "")
	if err != nil || len(page.Comments) != 1 {
		t.Fatalf("err=%v comments=%d", err, len(page.Comments))
	}
	if page.Comments[0].IssueNumber != 77 || page.Comments[0].ID != 555 {
		t.Fatalf("comment mapping wrong: %+v", page.Comments[0])
	}
}

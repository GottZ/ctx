package forge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeGitHub serves the issue listing of one repository the way GitHub does
// for the pull: items updated at or after `since`, ascending by updated_at,
// pageSize per page, and a Link header naming the next page. link renders
// the header field lines for a next-page URL, so a test chooses the spelling
// (#474: RFC 8288 allows more than the one GitHub uses). The comments
// listing answers an empty page.
type fakeGitHub struct {
	srv      *httptest.Server
	pageSize int
	link     func(next string) []string

	mu     sync.Mutex
	issues []fakeIssue
}

type fakeIssue struct {
	Number  int64
	Updated time.Time
}

func newFakeGitHub(t *testing.T, pageSize int, link func(next string) []string, issues ...fakeIssue) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{pageSize: pageSize, link: link, issues: issues}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// quotedNext is GitHub's own spelling.
func quotedNext(next string) []string { return []string{`<` + next + `>; rel="next"`} }

func (f *fakeGitHub) repo() RepoRef { return RepoRef{Owner: "o", Repo: "r", APIBase: f.srv.URL} }

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/repos/o/r/issues/comments" {
		_, _ = w.Write([]byte(`[]`))
		return
	}
	if r.URL.Path != "/repos/o/r/issues" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339, s)
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	f.mu.Lock()
	var match []fakeIssue
	for _, it := range f.issues {
		if !it.Updated.Before(since) {
			match = append(match, it)
		}
	}
	f.mu.Unlock()
	sort.SliceStable(match, func(i, j int) bool {
		if !match[i].Updated.Equal(match[j].Updated) {
			return match[i].Updated.Before(match[j].Updated)
		}
		return match[i].Number < match[j].Number
	})

	lo := (page - 1) * f.pageSize
	hi := lo + f.pageSize
	if lo > len(match) {
		lo = len(match)
	}
	if hi > len(match) {
		hi = len(match)
	}
	if hi < len(match) {
		nq := url.Values{}
		for k, v := range q {
			nq[k] = v
		}
		nq.Set("page", strconv.Itoa(page+1))
		for _, line := range f.link(f.srv.URL + r.URL.Path + "?" + nq.Encode()) {
			w.Header().Add("Link", line)
		}
	}
	items := make([]map[string]any, 0, hi-lo)
	for _, it := range match[lo:hi] {
		items = append(items, map[string]any{
			"number": it.Number, "title": "issue " + strconv.FormatInt(it.Number, 10),
			"body": "", "state": "open", "updated_at": it.Updated.UTC().Format(time.RFC3339),
		})
	}
	_ = json.NewEncoder(w).Encode(items)
}

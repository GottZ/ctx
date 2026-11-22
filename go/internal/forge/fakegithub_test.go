package forge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub serves the issue listing of one repository the way GitHub does
// for the pull: items updated at or after `since`, ascending by updated_at,
// pageSize per page, and a Link header naming the next page. link renders
// the header field lines for a next-page URL, so a test chooses the spelling
// (#474: RFC 8288 allows more than the one GitHub uses). The comments
// listing answers an empty page. With etags, a page carries an ETag
// derived from its body, as GitHub's do, and a matching If-None-Match is
// answered 304.
//
// It also takes the push's writes the way GitHub does (#491): a PATCH of an
// issue changes the fields it carries and moves the issue's updated_at, a
// POST creates an issue under the next number. A test can edit an issue on
// the forge (edit) and read what the forge holds (issue). seam installs
// functions that run inside the next issue PATCH: before while the request
// is on the wire and has not landed, after once it landed and before the
// response leaves — the window between a push's PATCH and its advance.
type fakeGitHub struct {
	srv      *httptest.Server
	pageSize int
	link     func(next string) []string
	etags    bool

	commentPages atomic.Int64 // comment-listing requests served

	mu      sync.Mutex
	issues  []fakeIssue
	patches []fakePatch // issue PATCHes that landed, in landing order
	before  func()      // one-shot seam of the next issue PATCH
	after   func()
}

// fakeIssue is one issue as the forge holds it. A zero Title, State is served
// as "issue <n>", "open".
type fakeIssue struct {
	Number    int64
	Updated   time.Time
	PR        bool // listed with a pull_request field, as GitHub lists pull requests
	Title     string
	Body      string
	State     string
	Labels    []string
	Assignees []string
}

// fakePatch is one issue PATCH as the forge received it.
type fakePatch struct {
	Number int64
	Fields map[string]json.RawMessage
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

// client is the production GitHub client pointed at the fake.
func (f *fakeGitHub) client() *githubClient { return &githubClient{http: f.srv.Client()} }

// seam arms the next issue PATCH (see fakeGitHub).
func (f *fakeGitHub) seam(before, after func()) {
	f.mu.Lock()
	f.before, f.after = before, after
	f.mu.Unlock()
}

// issue returns a copy of what the forge holds for issue n.
func (f *fakeGitHub) issue(t *testing.T, n int64) fakeIssue {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, it := range f.issues {
		if it.Number == n {
			it.Labels = append([]string(nil), it.Labels...)
			return it
		}
	}
	t.Fatalf("the fake forge holds no issue #%d", n)
	return fakeIssue{}
}

// edit changes issue n on the forge, as a remote user does, and moves its
// updated_at.
func (f *fakeGitHub) edit(t *testing.T, n int64, change func(*fakeIssue)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.issues {
		if f.issues[i].Number == n {
			change(&f.issues[i])
			f.issues[i].Updated = f.nextUpdated()
			return
		}
	}
	t.Fatalf("the fake forge holds no issue #%d", n)
}

// editInSecond changes issue n like edit but keeps its updated_at: a second
// write within the same second, which GitHub's updated_at (whole seconds)
// cannot tell from the first.
func (f *fakeGitHub) editInSecond(t *testing.T, n int64, change func(*fakeIssue)) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.issues {
		if f.issues[i].Number == n {
			at := f.issues[i].Updated
			change(&f.issues[i])
			f.issues[i].Updated = at
			return
		}
	}
	t.Fatalf("the fake forge holds no issue #%d", n)
}

// setETags makes the listing carry ETags and answer a matching If-None-Match
// with 304, as GitHub does.
func (f *fakeGitHub) setETags() {
	f.mu.Lock()
	f.etags = true
	f.mu.Unlock()
}

// landed returns the issue PATCHes that landed so far.
func (f *fakeGitHub) landed() []fakePatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePatch(nil), f.patches...)
}

// nextUpdated is one second past the newest updated_at the forge holds, so a
// write is always the newest item of the listing. Caller holds mu.
func (f *fakeGitHub) nextUpdated() time.Time {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, it := range f.issues {
		if it.Updated.After(at) {
			at = it.Updated
		}
	}
	return at.Add(time.Second)
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/comments":
		f.commentPages.Add(1)
		_, _ = w.Write([]byte(`[]`))
	case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
		f.serveListing(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
		f.serveCreate(w, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/"):
		f.servePatch(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) serveCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels"`
		Assignees []string `json:"assignees"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	var n int64
	for _, it := range f.issues {
		n = max(n, it.Number)
	}
	n++
	f.issues = append(f.issues, fakeIssue{Number: n, Updated: f.nextUpdated(), Title: in.Title, Body: in.Body,
		State: "open", Labels: in.Labels, Assignees: in.Assignees})
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"number": n})
}

// servePatch applies an issue PATCH. The seams run outside the lock: they
// may write to the forge themselves (another instance's PATCH).
func (f *fakeGitHub) servePatch(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/repos/o/r/issues/"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&fields); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if raw, ok := fields["state"]; ok {
		var st string
		if err := json.Unmarshal(raw, &st); err != nil || (st != "open" && st != "closed") {
			// GitHub answers a state outside open/closed with 422.
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
			return
		}
	}
	f.mu.Lock()
	before, after := f.before, f.after
	f.before, f.after = nil, nil
	f.mu.Unlock()
	if before != nil {
		before()
	}
	f.mu.Lock()
	found := false
	for i := range f.issues {
		if f.issues[i].Number != n {
			continue
		}
		found = true
		it := &f.issues[i]
		it.Title, it.State = it.servedTitle(), it.servedState()
		for k, v := range fields {
			switch k {
			case "title":
				_ = json.Unmarshal(v, &it.Title)
			case "body":
				_ = json.Unmarshal(v, &it.Body)
			case "state":
				_ = json.Unmarshal(v, &it.State)
			case "labels":
				_ = json.Unmarshal(v, &it.Labels)
			case "assignees":
				_ = json.Unmarshal(v, &it.Assignees)
			}
		}
		it.Updated = f.nextUpdated()
		f.patches = append(f.patches, fakePatch{Number: n, Fields: fields})
	}
	f.mu.Unlock()
	if !found {
		http.NotFound(w, r)
		return
	}
	if after != nil {
		after()
	}
	_, _ = w.Write([]byte(`{}`))
}

func (it fakeIssue) servedTitle() string {
	if it.Title == "" {
		return "issue " + strconv.FormatInt(it.Number, 10)
	}
	return it.Title
}

func (it fakeIssue) servedState() string {
	if it.State == "" {
		return "open"
	}
	return it.State
}

func (f *fakeGitHub) serveListing(w http.ResponseWriter, r *http.Request) {
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
		labels := make([]map[string]string, 0, len(it.Labels))
		for _, l := range it.Labels {
			labels = append(labels, map[string]string{"name": l})
		}
		assignees := make([]map[string]string, 0, len(it.Assignees))
		for _, a := range it.Assignees {
			assignees = append(assignees, map[string]string{"login": a})
		}
		item := map[string]any{
			"number": it.Number, "title": it.servedTitle(),
			"body": it.Body, "state": it.servedState(), "updated_at": it.Updated.UTC().Format(time.RFC3339),
			"labels": labels, "assignees": assignees,
		}
		if it.PR {
			item["pull_request"] = map[string]any{"url": "https://example.com/pulls/" + strconv.FormatInt(it.Number, 10)}
		}
		items = append(items, item)
	}
	body, _ := json.Marshal(items)
	if f.etags {
		sum := sha256.Sum256(body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
	}
	_, _ = w.Write(body)
}

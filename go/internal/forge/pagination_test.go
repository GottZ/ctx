package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// #474 point 2: the Link header is RFC 8288's, not only GitHub's spelling of
// it. A parser that knows one spelling ends the pull after the first page
// without an error — the run reports success, and issues beyond page one wait
// for later runs, or never arrive when more than a page of them share one
// updated_at (the since cursor then never moves past them).

// walkIssues follows NextURL from the first page and returns the issue
// numbers in order, or the first error.
func walkIssues(t *testing.T, gh *fakeGitHub) ([]int64, error) {
	t.Helper()
	c := &githubClient{http: gh.srv.Client()}
	page, err := c.ListIssuesSince(context.Background(), gh.repo(), time.Time{}, "")
	var got []int64
	for err == nil {
		for _, it := range page.Issues {
			got = append(got, it.Number)
		}
		if page.NextURL == "" || len(got) > 50 {
			break
		}
		page, err = c.ListIssuesPage(context.Background(), page.NextURL)
	}
	return got, err
}

func fiveIssues() []fakeIssue {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out []fakeIssue
	for i := int64(1); i <= 5; i++ {
		out = append(out, fakeIssue{Number: i, Updated: t0.Add(time.Duration(i) * time.Minute)})
	}
	return out
}

func lines(l ...string) []string { return l }

// TestPaginationFollowsEveryRFC8288Spelling: each spelling names the next page
// in a form RFC 8288 §3 allows; the walk reaches all five issues.
func TestPaginationFollowsEveryRFC8288Spelling(t *testing.T) {
	spellings := map[string]func(next string) []string{
		"github":            quotedNext,
		"gitea-no-space":    func(n string) []string { return lines(`<` + n + `>; rel="next",<` + n + `&x=1>; rel="last"`) },
		"unquoted-rel":      func(n string) []string { return lines(`<` + n + `>; rel=next`) },
		"two-relations":     func(n string) []string { return lines(`<` + n + `>; rel="next last"`) },
		"next-second":       func(n string) []string { return lines(`<` + n + `>; rel="last next"`) },
		"relation-case":     func(n string) []string { return lines(`<` + n + `>; rel="NEXT"`) },
		"comma-in-target":   func(n string) []string { return lines(`<` + n + `&labels=a,b>; rel="next"`) },
		"two-field-lines":   func(n string) []string { return lines(`<`+n+`&page=1>; rel="first"`, `<`+n+`>; rel="next"`) },
		"quoted-separators": func(n string) []string { return lines(`<` + n + `>; title="page 2, of; many"; rel="next"`) },
		"whitespace":        func(n string) []string { return lines(`<` + n + `> ;rel = "next" ; type=application/json`) },
		"empty-elements":    func(n string) []string { return lines(`, <` + n + `>; rel="next" ,, `) },
		"escaped-quote":     func(n string) []string { return lines(`<` + n + `>; title="a \"b\""; rel="next"`) },
		"relative-target": func(n string) []string {
			return lines(`<` + n[strings.Index(n, "/repos/"):] + `>; rel="next"`)
		},
		"first-rel-wins": func(n string) []string { return lines(`<` + n + `>; rel="next"; rel="prev"`) },
		// RFC 8288 §3.2: a link with an anchor is about another resource; a
		// client that does not apply the anchor ignores the link.
		"anchored-next-skipped": func(n string) []string {
			return lines(`</repos/o/r/issues?page=99>; rel=next; anchor="/other", <` + n + `>; rel=next`)
		},
	}
	for name, link := range spellings {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t, 2, link, fiveIssues()...)
			got, err := walkIssues(t, gh)
			if err != nil || fmt.Sprint(got) != "[1 2 3 4 5]" {
				t.Fatalf("walk = %v, err %v; want [1 2 3 4 5] (Link %q)", got, err, link(gh.srv.URL+"/repos/o/r/issues?page=2"))
			}
		})
	}
}

// TestPaginationEndsWhereNoNextIsNamed: a header without a next relation —
// another relation, a later rel parameter (RFC 8288 §3.3: occurrences after
// the first are ignored), an extension relation that only ends in "next" —
// is the last page, without an error.
func TestPaginationEndsWhereNoNextIsNamed(t *testing.T) {
	spellings := map[string]func(next string) []string{
		"prev-only":      func(n string) []string { return lines(`<` + n + `>; rel="prev"`) },
		"later-rel":      func(n string) []string { return lines(`<` + n + `>; rel="prev"; rel="next"`) },
		"extension-rel":  func(n string) []string { return lines(`<` + n + `>; rel="https://example.com/rel/next"`) },
		"next-in-title":  func(n string) []string { return lines(`<` + n + `>; rel="last"; title="next"`) },
		"next-in-target": func(n string) []string { return lines(`<` + n + `&next=1>; rel="last"`) },
		"anchored-only":  func(n string) []string { return lines(`<` + n + `>; rel="next"; anchor="#other"`) },
	}
	for name, link := range spellings {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t, 2, link, fiveIssues()...)
			got, err := walkIssues(t, gh)
			if err != nil || fmt.Sprint(got) != "[1 2]" {
				t.Fatalf("walk = %v, err %v; want only the first page [1 2] without an error", got, err)
			}
		})
	}
}

// TestPaginationUnparseableNextIsAnError: a header that does not parse as
// RFC 8288 but names a next page is an error that says so — not an early,
// silent end of the pull.
func TestPaginationUnparseableNextIsAnError(t *testing.T) {
	spellings := map[string]func(next string) []string{
		"no-closing-bracket": func(n string) []string { return lines(`<` + n + `; rel="next"`) },
		"no-brackets":        func(n string) []string { return lines(n + `; rel="next"`) },
		"open-quote":         func(n string) []string { return lines(`<` + n + `>; rel="next`) },
		"trailing-junk":      func(n string) []string { return lines(`<` + n + `>; rel="next" junk`) },
		"no-param-name":      func(n string) []string { return lines(`<` + n + `>; ="next"`) },
		"bad-second-element": func(n string) []string { return lines(`<` + n + `&page=9>; rel="last", next`) },
		// An unquoted rel value is a token (RFC 8288 §3.3); "=" and "/" are not tchars.
		"rel-not-a-token":   func(n string) []string { return lines(`<` + n + `>; rel=next=bad`) },
		"rel-not-a-token-2": func(n string) []string { return lines(`<` + n + `>; rel=next/page`) },
	}
	for name, link := range spellings {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t, 2, link, fiveIssues()...)
			got, err := walkIssues(t, gh)
			if err == nil {
				t.Fatalf("walk = %v without an error; an unparseable Link header naming next must fail the fetch", got)
			}
			if !strings.Contains(err.Error(), "Link header") {
				t.Errorf("error %q does not name the Link header", err)
			}
			if strings.Contains(err.Error(), gh.srv.URL) || strings.Contains(err.Error(), "/repos/") {
				t.Errorf("error %q carries the target URL", err)
			}
		})
	}
}

// TestPaginationUnparseableWithoutNextEnds: a header that does not parse and
// does not mention next — the word, not a part of one — names no next page;
// the walk ends there without an error, as before.
func TestPaginationUnparseableWithoutNextEnds(t *testing.T) {
	spellings := map[string]func(next string) []string{
		"no-closing-bracket": func(n string) []string { return lines(`<` + n + `; rel="last"`) },
		"word-inside-host":   func(n string) []string { return lines(`<` + n + `&host=nextcloud.example; rel="last"`) },
		"word-inside-path":   func(n string) []string { return lines(`<` + n + `&q=annexture; rel="last"`) },
	}
	for name, link := range spellings {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t, 2, link, fiveIssues()...)
			got, err := walkIssues(t, gh)
			if err != nil || fmt.Sprint(got) != "[1 2]" {
				t.Fatalf("walk = %v, err %v; want the first page [1 2] without an error", got, err)
			}
		})
	}
}

// TestPaginationResolvedTargetStaysOnOrigin: a relative target is resolved
// against the page (RFC 8288 §3.1, RFC 3986 §5) and then held to the origin
// rule of #459 like an absolute one — a network-path reference names another
// host.
func TestPaginationResolvedTargetStaysOnOrigin(t *testing.T) {
	gh := newFakeGitHub(t, 2, func(string) []string { return lines(`<//other.example/repos/o/r/issues?page=2>; rel="next"`) }, fiveIssues()...)
	got, err := walkIssues(t, gh)
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("walk = %v, err %v; want the next page on another origin refused", got, err)
	}
	if strings.Contains(err.Error(), "other.example") {
		t.Errorf("error %q names the target", err)
	}
}

// TestPaginationResolvesAgainstTheURLThatAnswered: after a redirect within
// the origin — GitHub's answer for a renamed repository — a relative target
// resolves against the URL that answered (RFC 3986 §5.1.3), not the one the
// client asked for: the next pages go to /repositories/1/…, and the old path
// sees the first request only.
func TestPaginationResolvesAgainstTheURLThatAnswered(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			http.Redirect(w, r, "/repositories/1/issues?"+r.URL.RawQuery, http.StatusMovedPermanently)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		if page < 3 {
			w.Header().Set("Link", `<issues?page=`+strconv.Itoa(page+1)+`>; rel="next"`)
		}
		_, _ = fmt.Fprintf(w, `[{"number":%d,"title":"t","state":"open"}]`, page)
	}))
	t.Cleanup(srv.Close)
	c := &githubClient{http: srv.Client()}
	page, err := c.ListIssuesSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}, time.Time{}, "")
	var got []int64
	for err == nil {
		for _, it := range page.Issues {
			got = append(got, it.Number)
		}
		if page.NextURL == "" || len(got) > 10 {
			break
		}
		page, err = c.ListIssuesPage(context.Background(), page.NextURL)
	}
	mu.Lock()
	defer mu.Unlock()
	old := 0
	for _, s := range seen {
		if strings.HasPrefix(s, "/repos/") {
			old++
		}
	}
	if err != nil || fmt.Sprint(got) != "[1 2 3]" || old != 1 {
		t.Fatalf("walk = %v, err %v, %d requests on the old path (want [1 2 3] and 1): %v", got, err, old, seen)
	}
}

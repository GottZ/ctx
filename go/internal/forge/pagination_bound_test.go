package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// #474 R1: the next page is the response's choice. A link that names the
// page carrying it — `<>`, `<#x>`, the page's own URL — or a ring of links
// over several pages kept a sync leg fetching the same pages until the
// process ended: the run has no deadline (context.WithoutCancel) and holds
// one of the daemon-wide sync slots meanwhile. A self link ends the leg
// with an error, so does a page the leg already requested, and so does the
// page bound, which stops a forge that names a new page forever.

// boundForge serves an endless listing: every page holds one issue and
// names a next page that link renders from the page number.
func boundForge(t *testing.T, link func(r *http.Request, page int) string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		if l := link(r, page); l != "" {
			w.Header().Set("Link", l)
		}
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = fmt.Fprintf(w, `[{"id":%d,"body":"c","issue_url":"https://x/issues/1"}]`, page)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"number":%d,"title":"t","state":"open"}]`, page)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// pullBoth runs each leg's fetch loop against srv the way runSync does,
// without a store (no apply hook commits nothing).
func pullBoth(t *testing.T, srv *httptest.Server) (issuesErr, commentsErr error) {
	t.Helper()
	m := &SyncManager{
		applyComments: func(context.Context, store.ProjectRow, []CommentRemote) (ApplyResult, error) {
			return ApplyResult{}, nil
		},
	}
	c := &githubClient{http: srv.Client()}
	repo := RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	issuesErr = m.pullIssues(ctx, store.ProjectRow{}, c, repo, false, "", time.Time{})
	commentsErr = m.pullComments(ctx, store.ProjectRow{}, c, repo, "", time.Time{})
	return issuesErr, commentsErr
}

func TestPaginationRefusesASelfLink(t *testing.T) {
	links := map[string]func(r *http.Request, page int) string{
		"empty-reference": func(*http.Request, int) string { return `<>; rel=next` },
		"fragment-only":   func(*http.Request, int) string { return `<#more>; rel="next"` },
		"own-url":         func(r *http.Request, _ int) string { return `<http://` + r.Host + r.URL.RequestURI() + `>; rel="next"` },
		"own-url-fragment": func(r *http.Request, _ int) string {
			return `<http://` + r.Host + r.URL.RequestURI() + `#x>; rel="next"`
		},
	}
	for name, link := range links {
		t.Run(name, func(t *testing.T) {
			srv, hits := boundForge(t, link)
			ierr, cerr := pullBoth(t, srv)
			for leg, err := range map[string]error{"issues": ierr, "comments": cerr} {
				if err == nil || !strings.Contains(err.Error(), "names the page that carries it") {
					t.Errorf("%s leg: err %v; want the self link refused", leg, err)
				}
			}
			if n := hits.Load(); n != 2 {
				t.Errorf("the forge saw %d requests; want 2 (one first page per leg)", n)
			}
		})
	}
}

func TestPaginationRefusesACycle(t *testing.T) {
	// 1 → 2 → 3 → 2: page 3 names page 2 again.
	srv, hits := boundForge(t, func(r *http.Request, page int) string {
		next := page + 1
		if page == 3 {
			next = 2
		}
		return `<http://` + r.Host + r.URL.Path + `?page=` + strconv.Itoa(next) + `>; rel="next"`
	})
	ierr, cerr := pullBoth(t, srv)
	for leg, err := range map[string]error{"issues": ierr, "comments": cerr} {
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Errorf("%s leg: err %v; want the cycle refused", leg, err)
		}
	}
	if n := hits.Load(); n != 6 {
		t.Errorf("the forge saw %d requests; want 6 (pages 1, 2, 3 per leg)", n)
	}
}

func TestPaginationStopsAtThePageBound(t *testing.T) {
	srv, hits := boundForge(t, func(r *http.Request, page int) string {
		return `<http://` + r.Host + r.URL.Path + `?page=` + strconv.Itoa(page+1) + `>; rel="next"`
	})
	ierr, cerr := pullBoth(t, srv)
	for leg, err := range map[string]error{"issues": ierr, "comments": cerr} {
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(maxPagesPerLeg)+" pages") {
			t.Errorf("%s leg: err %v; want the page bound", leg, err)
		}
		if got := sanitizeErr(err); got == "sync wire error" {
			t.Errorf("%s leg: last_error would read %q, not the bound", leg, got)
		}
	}
	if n := hits.Load(); n != 2*maxPagesPerLeg {
		t.Errorf("the forge saw %d requests; want %d (the bound per leg)", n, 2*maxPagesPerLeg)
	}
}

package forge

import (
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/httpx"
)

// linkSeeds: the reviewer's probes of #459, Link headers as GitHub, Gitea,
// Forgejo and GitLab sent them (anonymous GETs on public repositories,
// 2026-11-21), and the spellings of pagination_test.go.
var linkSeeds = []string{
	`<https://api.github.com/repos/o/r/issues?page=2>; rel="next"`,
	`<https://api.github.com@evil.example/x>; rel="next"`,
	`<https://evil.example/p>; rel="prev", <https://api.github.com/p2>; rel="next"`,
	`<https://api.github.com/p2>; rel="prev", <https://evil.example/p>; rel="next"`,
	`<https://evil.example/a,b>; rel="next"`,
	`<https://api.github.com/x;rel="next">; rel="last", <https://evil.example/>; rel="next"`,
	`<https://api.github.com/x>; rel="next"; foo=<https://evil.example/>`,
	`</repos/o/r/issues?page=2>; rel="next"`,
	`<//evil.example/x>; rel="next"`,
	`<HTTPS://API.GITHUB.COM/x>; rel="next"`,
	`<https://api.github.com:443/x>; rel="next"`,
	`<http://api.github.com/x>; rel="next"`,
	`<https://api.github.com./x>; rel="next"`,
	`<https://api.github.com\@evil.example/x>; rel="next"`,
	`<https://api.github.com%2F@evil.example/x>; rel="next"`,
	`<https://api.github.com:443@evil.example:443/x>; rel="next"`,
	`<https://api.github.com/x>; rel=next`,
	`<https://api.github.com/x>; rel="next last"`,
	`<https://api.github.com/x>;rel="next"`,
	`<ftp://api.github.com:443/x>; rel="next"`,
	// GitHub (cursor pagination of the issue listing with since; comments).
	`<https://api.github.com/repositories/23096959/issues?state=all&sort=updated&direction=asc&per_page=2&since=2024-01-01T00%3A00%3A00Z&after=Y3Vyc29yOnYyOpLPAAABjMNOWUjOUeLnuA%3D%3D&page=2>; rel="next"`,
	`<https://api.github.com/repositories/23096959/issues/comments?sort=updated&direction=asc&per_page=2&page=2>; rel="prev", <https://api.github.com/repositories/23096959/issues/comments?sort=updated&direction=asc&per_page=2&page=4>; rel="next", <https://api.github.com/repositories/23096959/issues/comments?sort=updated&direction=asc&per_page=2&page=15000>; rel="last", <https://api.github.com/repositories/23096959/issues/comments?sort=updated&direction=asc&per_page=2&page=1>; rel="first"`,
	// Gitea and Forgejo: no space after the comma.
	`<https://gitea.com/api/v1/repos/gitea/tea/issues?limit=2&page=3&state=all>; rel="next",<https://gitea.com/api/v1/repos/gitea/tea/issues?limit=2&page=576&state=all>; rel="last",<https://gitea.com/api/v1/repos/gitea/tea/issues?limit=2&page=1&state=all>; rel="first",<https://gitea.com/api/v1/repos/gitea/tea/issues?limit=2&page=1&state=all>; rel="prev"`,
	`<https://codeberg.org/api/v1/repos/forgejo/forgejo/issues?limit=2&page=3&state=all>; rel="next",<https://codeberg.org/api/v1/repos/forgejo/forgejo/issues?limit=2&page=7594&state=all>; rel="last"`,
	// GitLab.
	`<https://gitlab.com/api/v4/projects/gitlab-org%2Fgitlab-runner/issues?id=gitlab-org%2Fgitlab-runner&order_by=created_at&page=1&per_page=2&sort=desc&state=all&with_labels_details=false>; rel="prev", <https://gitlab.com/api/v4/projects/gitlab-org%2Fgitlab-runner/issues?id=gitlab-org%2Fgitlab-runner&order_by=created_at&page=3&per_page=2&sort=desc&state=all&with_labels_details=false>; rel="next", <https://gitlab.com/api/v4/projects/gitlab-org%2Fgitlab-runner/issues?id=gitlab-org%2Fgitlab-runner&order_by=created_at&page=1&per_page=2&sort=desc&state=all&with_labels_details=false>; rel="first"`,
	// RFC 8288 §3.5 examples and the spellings of pagination_test.go.
	`<http://example.com/TheBook/chapter2>; rel="previous"; title="previous chapter"`,
	`</TheBook/chapter2>; rel="previous"; title*=UTF-8'de'letztes%20Kapitel, </TheBook/chapter4>; rel="next"; title*=UTF-8'de'n%c3%a4chstes%20Kapitel`,
	`<http://example.org/>; rel="start http://example.net/relation/other"`,
	`<https://api.github.com/x> ;rel = "next" ; type=application/json`,
	`, <https://api.github.com/x>; rel="next" ,, `,
	`<https://api.github.com/x>; title="a \"b\""; rel="next"`,
	`<https://api.github.com/x>; title="page 2, of; many"; rel="next"`,
	`<https://api.github.com/x>; rel="prev"; rel="next"`,
	`<https://api.github.com/x; rel="next"`,
	`https://api.github.com/x; rel="next"`,
	`<https://api.github.com/x>; rel="next`,
	`<https://api.github.com/x>; rel="next" junk`,
	`<https://api.github.com/x>; ="next"`,
	`<https://api.github.com/x>; rel="last", next`,
	``,
}

const fuzzPage = "https://api.github.com/repos/o/r/issues?per_page=100"

// canonical writes links back as a Link field: every value quoted and
// escaped. Parsing it must give the same links.
func canonical(links []linkValue) string {
	var b strings.Builder
	for i, lv := range links {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("<" + lv.target + ">")
		for _, p := range lv.params {
			b.WriteString("; " + p.name + `="`)
			b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p.value))
			b.WriteString(`"`)
		}
	}
	return b.String()
}

// FuzzLinkHeader holds four properties for any field value: the parser does
// not panic; a field that parses reads back the same from its canonical
// form; nextTarget agrees with the parse — the first next target, or for a
// field that does not parse an error exactly when it mentions next; and a
// next page nextOnOrigin hands out lies on the origin of the page (#459).
func FuzzLinkHeader(f *testing.F) {
	for _, s := range linkSeeds {
		f.Add(s)
	}
	page, _ := url.Parse(fuzzPage)
	f.Fuzz(func(t *testing.T, field string) {
		links, perr := parseLinkField(field)
		target, terr := nextTarget([]string{field})
		if perr == nil {
			again, err := parseLinkField(canonical(links))
			if err != nil || !reflect.DeepEqual(again, links) {
				t.Fatalf("canonical form %q of %q reads back as %v, %v; want %v", canonical(links), field, again, err, links)
			}
			want, wantErr := "", error(nil)
			for _, lv := range links {
				if lv.hasRelation("next") && !lv.hasParam("anchor") {
					want = lv.target
					if want == "" {
						wantErr = errNextIsSelf
					}
					break
				}
			}
			if !errors.Is(terr, wantErr) || (wantErr == nil && target != want) {
				t.Fatalf("nextTarget(%q) = %q, %v; want %q, %v", field, target, terr, want, wantErr)
			}
		} else {
			if !errors.Is(perr, errLinkSyntax) {
				t.Fatalf("parse error %v is not errLinkSyntax", perr)
			}
			if (terr != nil) != mentionsNext(field) || target != "" {
				t.Fatalf("unparseable %q: nextTarget = %q, %v; mentionsNext %v", field, target, terr, mentionsNext(field))
			}
		}

		resp := &http.Response{Header: http.Header{"Link": []string{field}}, Request: &http.Request{URL: page}}
		next, err := nextOnOrigin(fuzzPage, resp)
		if err != nil {
			var ce *configError
			if !errors.As(err, &ce) || next != "" {
				t.Fatalf("nextOnOrigin(%q) = %q, %v; want a configError and no URL", field, next, err)
			}
			return
		}
		if next == "" {
			return
		}
		u, err := url.Parse(next)
		if err != nil || httpx.OriginOf(u) != httpx.OriginOf(page) {
			t.Fatalf("nextOnOrigin(%q) handed out %q, off the origin of %s", field, next, fuzzPage)
		}
	})
}

// TestNextOnOriginKeepsRealTargets: for the headers GitHub, Gitea, Forgejo
// and GitLab sent, the next page is requested byte for byte as the forge
// wrote it — the cursor and the encoded since of GitHub's issue listing
// included.
func TestNextOnOriginKeepsRealTargets(t *testing.T) {
	for _, field := range linkSeeds[20:25] {
		target, err := nextTarget([]string{field})
		if err != nil || target == "" {
			t.Fatalf("nextTarget(%q) = %q, %v", field, target, err)
		}
		page, _ := url.Parse(target)
		page.Path, page.RawPath, page.RawQuery = "/the-page-before", "", "" // same origin, another page
		resp := &http.Response{Header: http.Header{"Link": []string{field}}, Request: &http.Request{URL: page}}
		next, err := nextOnOrigin(page.String(), resp)
		if err != nil || next != target {
			t.Errorf("nextOnOrigin = %q, %v; want %q as written", next, err, target)
		}
	}
}

// TestMentionsNextIsLinear: the scan of an unparseable header for the word
// next runs over a header the forge chose — up to net/http's 10 MiB limit,
// and a tenant-admin may point forge.api_base at a host of their own. One
// "next" every five bytes, none of them a word: the scan must not copy the
// rest of the header per hit (a quadratic scan of 1 MiB took about a minute
// and held a sync slot meanwhile).
func TestMentionsNextIsLinear(t *testing.T) {
	field := strings.Repeat("anext", 1<<18) // 1.25 MiB
	if mentionsNext(field) {
		t.Fatal("no word next in the field")
	}
	if a := testing.AllocsPerRun(3, func() { mentionsNext(field) }); a > 2 {
		t.Fatalf("mentionsNext allocated %.0f times over one field; want at most 2 (one lowered copy)", a)
	}
}

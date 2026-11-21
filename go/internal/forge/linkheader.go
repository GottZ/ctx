// Link header parsing for the pull's pagination (RFC 8288 §3, #474).
//
// GitHub writes one spelling of the header — `<url>; rel="next", <url>;
// rel="last"` — and a parser that knows only that spelling reads every other
// one as "no next page": the pull ends after the first page and the run
// reports success. This parser follows the grammar instead, and a header
// that does not parse but names a next page is an error, not an end.

package forge

import (
	"bytes"
	"errors"
	"strings"
)

// linkValue is one link-value of a Link field: the target as written between
// "<" and ">", and the parameters in their order, names lowercased.
type linkValue struct {
	target string
	params []linkParam
}

type linkParam struct{ name, value string }

// errLinkSyntax marks a field value that does not follow the grammar. It
// never leaves this file: nextTarget turns it into errLinkUnparseable or
// into "no next page".
var errLinkSyntax = errors.New("link: not RFC 8288")

// errLinkUnparseable: the header names a next page in a form the parser
// cannot read. The pull stops with this error instead of ending early and
// silently; the header itself is the remote's text and is not quoted.
var errLinkUnparseable error = &configError{"forge: the Link header of a page names a next page but does not parse as RFC 8288; the pull stopped there"}

// nextTarget returns the target of the first link-value whose rel parameter
// holds the relation type "next", "" when no link-value does. fields are the
// response's Link field lines; RFC 9110 §5.3 combines several into one list.
// A link-value with an anchor parameter is about another resource (RFC 8288
// §3.2); this client does not apply anchors, so it ignores such a link.
func nextTarget(fields []string) (string, error) {
	field := strings.Join(fields, ", ")
	links, err := parseLinkField(field)
	if err != nil {
		if mentionsNext(field) {
			return "", errLinkUnparseable
		}
		return "", nil
	}
	for _, lv := range links {
		if lv.hasRelation("next") && !lv.hasParam("anchor") {
			if lv.target == "" {
				return "", errNextIsSelf // an empty reference is the page itself (RFC 3986 §4.4)
			}
			return lv.target, nil
		}
	}
	return "", nil
}

// hasRelation reports whether the first rel parameter of lv lists the
// relation type rt. RFC 8288 §3.3: rel MUST NOT appear more than once, and
// occurrences after the first MUST be ignored; its value is one or more
// relation types separated by whitespace (§3.3 relation-types), compared
// case-insensitively (§2.1.1).
func (lv linkValue) hasRelation(rt string) bool {
	for _, p := range lv.params {
		if p.name != "rel" {
			continue
		}
		for _, t := range strings.FieldsFunc(p.value, isWSP) {
			if strings.EqualFold(t, rt) {
				return true
			}
		}
		return false
	}
	return false
}

// hasParam reports whether lv carries a parameter named name (lowercase).
func (lv linkValue) hasParam(name string) bool {
	for _, p := range lv.params {
		if p.name == name {
			return true
		}
	}
	return false
}

// parseLinkField parses a Link field value:
//
//	Link       = #link-value
//	link-value = "<" URI-Reference ">" *( OWS ";" OWS link-param )
//	link-param = token BWS [ "=" BWS ( token / quoted-string ) ]
//
// with the list rule of RFC 9110 §5.6.1 (empty elements are skipped). The
// URI-Reference runs to the first ">", so a comma or semicolon inside it is
// part of the target. An unquoted value is read up to whitespace, ";", ","
// or a quote (RFC 8288 Appendix B.3 reads up to ";" or ","; a media type
// such as application/json is not a token, yet servers write it unquoted) —
// except rel, whose unquoted value has to be a token (RFC 8288 §3.3).
// Anything else — a link-value without "<…>", an unterminated quoted-string,
// a parameter without a name, text after a link-value that is neither ";"
// nor "," — is errLinkSyntax.
func parseLinkField(s string) ([]linkValue, error) {
	p := &linkParser{s: s}
	var out []linkValue
	for {
		p.skipWS()
		switch {
		case p.eof():
			return out, nil
		case p.s[p.i] == ',':
			p.i++ // an empty list element
			continue
		}
		lv, err := p.linkValue()
		if err != nil {
			return nil, err
		}
		out = append(out, lv)
		p.skipWS()
		if p.eof() {
			return out, nil
		}
		if p.s[p.i] != ',' {
			return nil, errLinkSyntax
		}
		p.i++
	}
}

type linkParser struct {
	s string
	i int
}

func (p *linkParser) eof() bool { return p.i >= len(p.s) }

// skipWS consumes OWS / BWS (spaces and horizontal tabs).
func (p *linkParser) skipWS() {
	for !p.eof() && isWSP(rune(p.s[p.i])) {
		p.i++
	}
}

func (p *linkParser) linkValue() (linkValue, error) {
	if p.s[p.i] != '<' {
		return linkValue{}, errLinkSyntax
	}
	end := strings.IndexByte(p.s[p.i+1:], '>')
	if end < 0 {
		return linkValue{}, errLinkSyntax
	}
	lv := linkValue{target: p.s[p.i+1 : p.i+1+end]}
	p.i += end + 2
	for {
		save := p.i
		p.skipWS()
		if p.eof() || p.s[p.i] != ';' {
			p.i = save
			return lv, nil
		}
		p.i++
		p.skipWS()
		param, err := p.param()
		if err != nil {
			return linkValue{}, err
		}
		lv.params = append(lv.params, param)
	}
}

func (p *linkParser) param() (linkParam, error) {
	start := p.i
	for !p.eof() && isTchar(p.s[p.i]) {
		p.i++
	}
	if p.i == start {
		return linkParam{}, errLinkSyntax
	}
	param := linkParam{name: strings.ToLower(p.s[start:p.i])}
	save := p.i
	p.skipWS()
	if p.eof() || p.s[p.i] != '=' {
		p.i = save // a parameter without a value
		return param, nil
	}
	p.i++
	p.skipWS()
	if !p.eof() && p.s[p.i] == '"' {
		v, err := p.quoted()
		if err != nil {
			return linkParam{}, err
		}
		param.value = v
		return param, nil
	}
	start = p.i
	for !p.eof() && !strings.ContainsRune(" \t;,\"", rune(p.s[p.i])) {
		p.i++
	}
	if p.i == start {
		return linkParam{}, errLinkSyntax
	}
	param.value = p.s[start:p.i]
	if param.name == "rel" && !isToken(param.value) {
		return linkParam{}, errLinkSyntax // an unquoted rel is a token (RFC 8288 §3.3)
	}
	return param, nil
}

// quoted reads a quoted-string (RFC 9110 §5.6.4) and returns its content
// with the quoted-pairs resolved.
func (p *linkParser) quoted() (string, error) {
	p.i++ // the opening DQUOTE
	var b strings.Builder
	for !p.eof() {
		c := p.s[p.i]
		p.i++
		switch c {
		case '"':
			return b.String(), nil
		case '\\':
			if p.eof() {
				return "", errLinkSyntax
			}
			b.WriteByte(p.s[p.i])
			p.i++
		default:
			b.WriteByte(c)
		}
	}
	return "", errLinkSyntax
}

func isWSP(r rune) bool { return r == ' ' || r == '\t' }

// isToken reports whether s is a token (RFC 9110 §5.6.2).
func isToken(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isTchar(s[i]) {
			return false
		}
	}
	return s != ""
}

// isTchar is RFC 9110 §5.6.2's tchar.
func isTchar(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// mentionsNext reports whether s holds the word "next" in any ASCII case,
// bounded by characters that are not letters or digits: the sign that an
// unparseable header named a next page. It errs towards yes — "next" in a
// URL path counts — since a false yes stops the pull with an error and a
// false no would end it silently.
//
// The field is the remote's and may be megabytes long: one lowered copy,
// then a linear scan over it (no copy per hit).
func mentionsNext(s string) bool {
	low := []byte(s)
	for i, c := range low {
		if 'A' <= c && c <= 'Z' {
			low[i] = c + ('a' - 'A')
		}
	}
	for from := 0; ; {
		i := bytes.Index(low[from:], nextWord)
		if i < 0 {
			return false
		}
		i += from
		before := i == 0 || !isAlnum(low[i-1])
		after := i+4 == len(low) || !isAlnum(low[i+4])
		if before && after {
			return true
		}
		from = i + 1
	}
}

// nextWord is what mentionsNext looks for.
var nextWord = []byte("next")

func isAlnum(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

package fedhop

// Gates of F-W3 on the header grammar (design/04 §4.1, table "Header-Grammatik"):
//
//   - TestParseInboundGrammar: valid and invalid inputs of every request
//     header — error class (400 InvalidHeaderError, 508 DuplicateEntryError)
//     and field value per case: 16 vs 17 entries, upper case in Ctx-Path,
//     duplicates, garbage, len(Path) > len(Hop), several header lines = one
//     list, an unreadable Ctx-Embed-Space-Require = 400, and the fail-closed
//     value of every header that is only mapped (unknown Ctx-Sensitivity ⇒
//     credentials, Ctx-Egress ⇒ local, Ctx-Class ⇒ background) or ignored
//     (Ctx-Max-Wait-Ms out of 0…120 000, invalid Ctx-Pipeline).
//   - FuzzParseInbound: no panic, and Format∘Parse is the identity on the
//     parsed value for every accepted input (Parse(Format(Parse(h))) =
//     Parse(h), Format idempotent); a canonical input is reproduced byte
//     for byte (table cases marked canonical).
//
//	go test -count=1 -run 'TestParseInbound|TestLoopFor|TestHeader|TestReason' ./internal/fedhop/
//	go test -run NONE -fuzz FuzzParseInbound -fuzztime 60s ./internal/fedhop/

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tok returns the i-th grammar-valid hop token of no identity (1-based).
func tok(i int) string { return fmt.Sprintf("%032x", i) }

// uid returns the i-th canonical lowercase UUID (1-based).
func uid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", i) }

// toks returns tokens from..to as a list.
func toks(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, tok(i))
	}
	return out
}

// hdr builds a header from name/value pairs; a repeated name adds a line.
func hdr(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

// formatInbound is the Format half of the round trip: the canonical header
// form of a parsed Inbound, written with the same list and millisecond
// writers Stamp uses.
func formatInbound(in Inbound) http.Header {
	h := http.Header{}
	if len(in.Hop) > 0 {
		h.Set(HeaderHop, formatList(in.Hop))
	}
	if len(in.Path) > 0 {
		h.Set(HeaderPath, formatList(in.Path))
	}
	if in.Sensitivity != "" {
		h.Set(HeaderSensitivity, in.Sensitivity)
	}
	if in.EgressLocal {
		h.Set(HeaderEgress, "local")
	}
	if in.Class != "" {
		h.Set(HeaderClass, in.Class)
	}
	if in.HasMaxWait {
		h.Set(HeaderMaxWait, formatMillis(in.MaxWait))
	}
	if in.Pipeline != "" {
		h.Set(HeaderPipeline, in.Pipeline)
	}
	if in.EmbedSpaceRequire != "" {
		h.Set(HeaderEmbedSpaceRequire, in.EmbedSpaceRequire)
	}
	return h
}

// sameInbound compares two Inbounds without their receive time.
func sameInbound(a, b Inbound) bool {
	a.ReceivedAt, b.ReceivedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

const (
	errNone = ""
	err400  = "400"
	err508  = "508"
)

type parseCase struct {
	name      string
	h         http.Header
	wantErr   string // errNone, err400 (InvalidHeaderError) or err508 (DuplicateEntryError)
	errHeader string // the header the error names
	want      Inbound
	canonical bool // formatInbound(ParseInbound(h)) == h byte for byte
}

func grammarCases() []parseCase {
	long := strings.Repeat("a", MaxPipelineLen)
	return []parseCase{
		{name: "no federation header", h: hdr("Authorization", "Bearer x"), canonical: false},
		{name: "empty header map", h: http.Header{}, canonical: true},
		{name: "one hop", h: hdr(HeaderHop, tok(1)), want: Inbound{Hop: []string{tok(1)}}, canonical: true},
		{name: "hop list canonical", h: hdr(HeaderHop, tok(1)+", "+tok(2)+", "+tok(3)), want: Inbound{Hop: toks(1, 3)}, canonical: true},
		{name: "hop list OWS variants", h: hdr(HeaderHop, " "+tok(1)+","+tok(2)+" \t,\t "+tok(3)+" "), want: Inbound{Hop: toks(1, 3)}},
		{name: "several lines are one list", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderHop, tok(3)), want: Inbound{Hop: toks(1, 3)}},
		{name: "16 hops", h: hdr(HeaderHop, strings.Join(toks(1, 16), ", ")), want: Inbound{Hop: toks(1, 16)}, canonical: true},
		{name: "17 hops", h: hdr(HeaderHop, strings.Join(toks(1, 17), ", ")), wantErr: err400, errHeader: HeaderHop},
		{name: "17 hops over two lines", h: hdr(HeaderHop, strings.Join(toks(1, 9), ", "), HeaderHop, strings.Join(toks(10, 17), ",")), wantErr: err400, errHeader: HeaderHop},
		{name: "17 hops, the 17th garbage", h: hdr(HeaderHop, strings.Join(toks(1, 16), ", ")+", x"), wantErr: err400, errHeader: HeaderHop},
		{name: "upper-case token", h: hdr(HeaderHop, strings.ToUpper(strings.Repeat("ab", 16))), wantErr: err400, errHeader: HeaderHop},
		{name: "mixed-case token", h: hdr(HeaderHop, "A"+tok(1)[1:]), wantErr: err400, errHeader: HeaderHop},
		{name: "token 31 chars", h: hdr(HeaderHop, tok(1)[1:]), wantErr: err400, errHeader: HeaderHop},
		{name: "token 33 chars", h: hdr(HeaderHop, tok(1)+"0"), wantErr: err400, errHeader: HeaderHop},
		{name: "token non-hex", h: hdr(HeaderHop, "g"+tok(1)[1:]), wantErr: err400, errHeader: HeaderHop},
		{name: "token with inner space", h: hdr(HeaderHop, tok(1)[:16]+" "+tok(1)[16:]), wantErr: err400, errHeader: HeaderHop},
		{name: "token with vertical tab", h: hdr(HeaderHop, tok(1)+"\v"), wantErr: err400, errHeader: HeaderHop},
		{name: "garbage", h: hdr(HeaderHop, "x"), wantErr: err400, errHeader: HeaderHop},
		{name: "empty element", h: hdr(HeaderHop, tok(1)+",,"+tok(2)), wantErr: err400, errHeader: HeaderHop},
		{name: "trailing comma", h: hdr(HeaderHop, tok(1)+","), wantErr: err400, errHeader: HeaderHop},
		{name: "empty hop line", h: hdr(HeaderHop, tok(1), HeaderHop, ""), wantErr: err400, errHeader: HeaderHop},
		{name: "duplicate hop", h: hdr(HeaderHop, tok(1)+", "+tok(1)), wantErr: err508, errHeader: HeaderHop, want: Inbound{Hop: []string{tok(1), tok(1)}}},
		{name: "duplicate hop over lines", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderHop, tok(1)), wantErr: err508, errHeader: HeaderHop, want: Inbound{Hop: []string{tok(1), tok(2), tok(1)}}},
		{name: "path", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderPath, uid(1)+", "+uid(2)), want: Inbound{Hop: toks(1, 2), Path: []string{uid(1), uid(2)}}, canonical: true},
		{name: "path shorter than hop", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderPath, uid(2)), want: Inbound{Hop: toks(1, 2), Path: []string{uid(2)}}, canonical: true},
		{name: "path over two lines", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderPath, uid(1), HeaderPath, uid(2)), want: Inbound{Hop: toks(1, 2), Path: []string{uid(1), uid(2)}}},
		{name: "path upper case", h: hdr(HeaderHop, tok(1), HeaderPath, strings.ToUpper(uid(10))), wantErr: err400, errHeader: HeaderPath},
		{name: "path one upper-case letter", h: hdr(HeaderHop, tok(1), HeaderPath, "6F1c2c9e-4d0b-4a52-9a43-0c3f5e1d2b7a"), wantErr: err400, errHeader: HeaderPath},
		{name: "path longer than hop", h: hdr(HeaderHop, tok(1), HeaderPath, uid(1)+", "+uid(2)), wantErr: err400, errHeader: HeaderPath},
		{name: "path without hop", h: hdr(HeaderPath, uid(1)), wantErr: err400, errHeader: HeaderPath},
		{name: "path 16 entries", h: hdr(HeaderHop, strings.Join(toks(1, 16), ", "), HeaderPath, strings.Join(uids(1, 16), ", ")), want: Inbound{Hop: toks(1, 16), Path: uids(1, 16)}, canonical: true},
		{name: "path 17 entries", h: hdr(HeaderHop, strings.Join(toks(1, 16), ", "), HeaderPath, strings.Join(uids(1, 17), ", ")), wantErr: err400, errHeader: HeaderPath},
		{name: "path not a uuid", h: hdr(HeaderHop, tok(1), HeaderPath, "6f1c2c9e4d0b4a529a430c3f5e1d2b7a"), wantErr: err400, errHeader: HeaderPath},
		{name: "path braced uuid", h: hdr(HeaderHop, tok(1), HeaderPath, "{"+uid(1)+"}"), wantErr: err400, errHeader: HeaderPath},
		{name: "path urn uuid", h: hdr(HeaderHop, tok(1), HeaderPath, "urn:uuid:"+uid(1)), wantErr: err400, errHeader: HeaderPath},
		{name: "path hyphen misplaced", h: hdr(HeaderHop, tok(1), HeaderPath, "6f1c2c9e4-d0b-4a52-9a43-0c3f5e1d2b7a"), wantErr: err400, errHeader: HeaderPath},
		{name: "path empty element", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderPath, uid(1)+", "), wantErr: err400, errHeader: HeaderPath},
		{name: "duplicate path", h: hdr(HeaderHop, tok(1)+", "+tok(2), HeaderPath, uid(1)+", "+uid(1)), wantErr: err508, errHeader: HeaderPath, want: Inbound{Hop: toks(1, 2), Path: []string{uid(1), uid(1)}}},
		{name: "garbage beats duplicate", h: hdr(HeaderHop, tok(1)+", "+tok(1), HeaderEmbedSpaceRequire, "xyz"), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},
		{name: "non-canonical key", h: http.Header{"ctx-hop": {tok(1)}}, wantErr: err400, errHeader: HeaderHop},
		{name: "non-canonical key next to canonical", h: http.Header{HeaderHop: {tok(1)}, "CTX-HOP": {tok(2)}}, wantErr: err400, errHeader: HeaderHop},
		{name: "unknown ctx header ignored", h: hdr("Ctx-Future", "whatever"), want: Inbound{}},

		{name: "sensitivity credentials", h: hdr(HeaderSensitivity, "credentials"), want: Inbound{Sensitivity: "credentials"}, canonical: true},
		{name: "sensitivity personal", h: hdr(HeaderSensitivity, "personal"), want: Inbound{Sensitivity: "personal"}, canonical: true},
		{name: "sensitivity internal", h: hdr(HeaderSensitivity, "internal"), want: Inbound{Sensitivity: "internal"}, canonical: true},
		{name: "sensitivity public", h: hdr(HeaderSensitivity, "public"), want: Inbound{Sensitivity: "public"}, canonical: true},
		{name: "sensitivity unknown", h: hdr(HeaderSensitivity, "secret"), want: Inbound{Sensitivity: "credentials"}},
		{name: "sensitivity upper case", h: hdr(HeaderSensitivity, "PUBLIC"), want: Inbound{Sensitivity: "credentials"}},
		{name: "sensitivity empty", h: hdr(HeaderSensitivity, ""), want: Inbound{Sensitivity: "credentials"}},
		{name: "sensitivity list", h: hdr(HeaderSensitivity, "public, internal"), want: Inbound{Sensitivity: "credentials"}},
		{name: "sensitivity two lines", h: hdr(HeaderSensitivity, "public", HeaderSensitivity, "public"), want: Inbound{Sensitivity: "credentials"}},
		{name: "sensitivity OWS", h: hdr(HeaderSensitivity, " public\t"), want: Inbound{Sensitivity: "public"}},

		{name: "egress local", h: hdr(HeaderEgress, "local"), want: Inbound{EgressLocal: true}, canonical: true},
		{name: "egress any", h: hdr(HeaderEgress, "any"), want: Inbound{}},
		{name: "egress unknown", h: hdr(HeaderEgress, "none"), want: Inbound{EgressLocal: true}},
		{name: "egress upper case any", h: hdr(HeaderEgress, "ANY"), want: Inbound{EgressLocal: true}},
		{name: "egress empty", h: hdr(HeaderEgress, ""), want: Inbound{EgressLocal: true}},
		{name: "egress two lines", h: hdr(HeaderEgress, "any", HeaderEgress, "any"), want: Inbound{EgressLocal: true}},

		{name: "class interactive", h: hdr(HeaderClass, "interactive"), want: Inbound{Class: "interactive"}, canonical: true},
		{name: "class external", h: hdr(HeaderClass, "external"), want: Inbound{Class: "external"}, canonical: true},
		{name: "class background", h: hdr(HeaderClass, "background"), want: Inbound{Class: "background"}, canonical: true},
		{name: "class external_bulk", h: hdr(HeaderClass, "external_bulk"), want: Inbound{Class: "external_bulk"}, canonical: true},
		{name: "class unknown", h: hdr(HeaderClass, "urgent"), want: Inbound{Class: "background"}},
		{name: "class capitalised", h: hdr(HeaderClass, "Interactive"), want: Inbound{Class: "background"}},
		{name: "class invalid name", h: hdr(HeaderClass, "invalid"), want: Inbound{Class: "background"}},
		{name: "class two lines", h: hdr(HeaderClass, "interactive", HeaderClass, "interactive"), want: Inbound{Class: "background"}},

		{name: "max wait 0", h: hdr(HeaderMaxWait, "0"), want: Inbound{HasMaxWait: true}, canonical: true},
		{name: "max wait 5000", h: hdr(HeaderMaxWait, "5000"), want: Inbound{MaxWait: 5 * time.Second, HasMaxWait: true}, canonical: true},
		{name: "max wait 120000", h: hdr(HeaderMaxWait, "120000"), want: Inbound{MaxWait: 120 * time.Second, HasMaxWait: true}, canonical: true},
		{name: "max wait 120001", h: hdr(HeaderMaxWait, "120001"), want: Inbound{}},
		{name: "max wait 999999", h: hdr(HeaderMaxWait, "999999"), want: Inbound{}},
		{name: "max wait 7 digits", h: hdr(HeaderMaxWait, "1000000"), want: Inbound{}},
		{name: "max wait overflow", h: hdr(HeaderMaxWait, "99999999999999999999999"), want: Inbound{}},
		{name: "max wait 2^64+5000 (wraps to 5000 without the digit limit)", h: hdr(HeaderMaxWait, "18446744073709556616"), want: Inbound{}},
		{name: "max wait 00", h: hdr(HeaderMaxWait, "00"), want: Inbound{}},
		{name: "max wait negative", h: hdr(HeaderMaxWait, "-1"), want: Inbound{}},
		{name: "max wait plus sign", h: hdr(HeaderMaxWait, "+5"), want: Inbound{}},
		{name: "max wait leading zero", h: hdr(HeaderMaxWait, "05"), want: Inbound{}},
		{name: "max wait exponent", h: hdr(HeaderMaxWait, "1e3"), want: Inbound{}},
		{name: "max wait fraction", h: hdr(HeaderMaxWait, "1.5"), want: Inbound{}},
		{name: "max wait empty", h: hdr(HeaderMaxWait, ""), want: Inbound{}},
		{name: "max wait two lines", h: hdr(HeaderMaxWait, "100", HeaderMaxWait, "200"), want: Inbound{}},
		{name: "max wait OWS", h: hdr(HeaderMaxWait, " 500 "), want: Inbound{MaxWait: 500 * time.Millisecond, HasMaxWait: true}},

		{name: "pipeline", h: hdr(HeaderPipeline, "dream-eval"), want: Inbound{Pipeline: "dream-eval"}, canonical: true},
		{name: "pipeline one char", h: hdr(HeaderPipeline, "a"), want: Inbound{Pipeline: "a"}, canonical: true},
		{name: "pipeline all classes", h: hdr(HeaderPipeline, "0a:b_c-d"), want: Inbound{Pipeline: "0a:b_c-d"}, canonical: true},
		{name: "pipeline 64 chars", h: hdr(HeaderPipeline, long), want: Inbound{Pipeline: long}, canonical: true},
		{name: "pipeline 65 chars", h: hdr(HeaderPipeline, long+"a"), want: Inbound{}},
		{name: "pipeline upper case", h: hdr(HeaderPipeline, "Dream-eval"), want: Inbound{}},
		{name: "pipeline leading hyphen", h: hdr(HeaderPipeline, "-dream"), want: Inbound{}},
		{name: "pipeline leading colon", h: hdr(HeaderPipeline, ":dream"), want: Inbound{}},
		{name: "pipeline dot", h: hdr(HeaderPipeline, "a.b"), want: Inbound{}},
		{name: "pipeline space", h: hdr(HeaderPipeline, "a b"), want: Inbound{}},
		{name: "pipeline empty", h: hdr(HeaderPipeline, ""), want: Inbound{}},
		{name: "pipeline two lines", h: hdr(HeaderPipeline, "a", HeaderPipeline, "b"), want: Inbound{}},

		{name: "embed space", h: hdr(HeaderEmbedSpaceRequire, "0123456789abcdef"), want: Inbound{EmbedSpaceRequire: "0123456789abcdef"}, canonical: true},
		{name: "embed space 15", h: hdr(HeaderEmbedSpaceRequire, "0123456789abcde"), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},
		{name: "embed space 17", h: hdr(HeaderEmbedSpaceRequire, "0123456789abcdef0"), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},
		{name: "embed space upper case", h: hdr(HeaderEmbedSpaceRequire, "0123456789ABCDEF"), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},
		{name: "embed space non-hex", h: hdr(HeaderEmbedSpaceRequire, "0123456789abcdeg"), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},
		{name: "embed space empty", h: hdr(HeaderEmbedSpaceRequire, ""), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},
		{name: "embed space two lines", h: hdr(HeaderEmbedSpaceRequire, "0123456789abcdef", HeaderEmbedSpaceRequire, "0123456789abcdef"), wantErr: err400, errHeader: HeaderEmbedSpaceRequire},

		{name: "everything", h: hdr(
			HeaderHop, tok(1)+", "+tok(2),
			HeaderPath, uid(1)+", "+uid(2),
			HeaderSensitivity, "personal",
			HeaderEgress, "local",
			HeaderClass, "external_bulk",
			HeaderMaxWait, "20000",
			HeaderPipeline, "web-chat",
			HeaderEmbedSpaceRequire, "fedcba9876543210",
		), want: Inbound{
			Hop: toks(1, 2), Path: []string{uid(1), uid(2)}, Sensitivity: "personal", EgressLocal: true,
			Class: "external_bulk", MaxWait: 20 * time.Second, HasMaxWait: true, Pipeline: "web-chat",
			EmbedSpaceRequire: "fedcba9876543210",
		}, canonical: true},
		{name: "X-Request-ID is not part of the inbound", h: hdr("X-Request-ID", "abc"), want: Inbound{}},
	}
}

// nonCanonicalCases: every request header under a lower-case and an
// upper-case key of a hand-built map is refused, with a value that would be
// valid under the canonical key — reading only h[canonical] would skip it.
func nonCanonicalCases() []parseCase {
	valid := map[string]string{
		HeaderHop: tok(1), HeaderPath: uid(1), HeaderSensitivity: "public", HeaderEgress: "any",
		HeaderClass: "interactive", HeaderMaxWait: "5", HeaderPipeline: "a", HeaderEmbedSpaceRequire: "0123456789abcdef",
	}
	var out []parseCase
	for _, name := range requestHeaders {
		for _, key := range []string{strings.ToLower(name), strings.ToUpper(name)} {
			h := http.Header{HeaderHop: {tok(9)}, key: {valid[name]}}
			out = append(out, parseCase{name: "non-canonical key " + key, h: h, wantErr: err400, errHeader: name})
		}
	}
	return out
}

func uids(from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, uid(i))
	}
	return out
}

func TestParseInboundGrammar(t *testing.T) {
	cases := append(grammarCases(), nonCanonicalCases()...)
	if len(nonCanonicalCases()) != 2*len(requestHeaders) || len(requestHeaders) != 8 {
		t.Fatalf("%d request headers, want the 8 of the grammar table", len(requestHeaders))
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := time.Now()
			in, err := ParseInbound(c.h)
			var inv *InvalidHeaderError
			var dup *DuplicateEntryError
			switch c.wantErr {
			case errNone:
				if err != nil {
					t.Fatalf("ParseInbound: %v, want no error", err)
				}
			case err400:
				if !errors.As(err, &inv) || errors.As(err, &dup) {
					t.Fatalf("ParseInbound error %v (%T), want *InvalidHeaderError (400)", err, err)
				}
				if inv.Header != c.errHeader {
					t.Errorf("error names %q, want %q", inv.Header, c.errHeader)
				}
				if !sameInbound(in, Inbound{}) {
					t.Errorf("a 400 returns the zero Inbound, got %+v", in)
				}
				return
			case err508:
				if !errors.As(err, &dup) || errors.As(err, &inv) {
					t.Fatalf("ParseInbound error %v (%T), want *DuplicateEntryError (508)", err, err)
				}
				if dup.Header != c.errHeader {
					t.Errorf("error names %q, want %q", dup.Header, c.errHeader)
				}
				if _, loop := in.LoopFor(Identity{}); !loop {
					t.Error("LoopFor on the Inbound of a duplicate reports no loop")
				}
			}
			if !sameInbound(in, c.want) {
				t.Errorf("ParseInbound = %+v\nwant          %+v", in, c.want)
			}
			if in.ReceivedAt.Before(before) || in.ReceivedAt.After(time.Now()) {
				t.Errorf("ReceivedAt %v is not the parse time", in.ReceivedAt)
			}
			if c.wantErr == errNone && c.canonical {
				if got := formatInbound(in); !reflect.DeepEqual(got, c.h) {
					t.Errorf("Format∘Parse of a canonical input = %v, want %v", got, c.h)
				}
			}
		})
	}
}

// TestParseInboundErrorsCarryNoValue: neither error names the header value —
// garbage may be anything a client sent (a pasted key included), and the
// message reaches logs.
func TestParseInboundErrorsCarryNoValue(t *testing.T) {
	const marker = "sk-live-0123456789abcdefSECRET"
	for _, h := range []http.Header{
		hdr(HeaderHop, marker),
		hdr(HeaderHop, tok(1)+", "+marker),
		hdr(HeaderHop, tok(1), HeaderPath, marker),
		hdr(HeaderEmbedSpaceRequire, marker),
		{"ctx-hop": {marker}},
	} {
		_, err := ParseInbound(h)
		if err == nil {
			t.Fatalf("%v: no error", h)
		}
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "SECRET") {
			t.Errorf("error %q carries the header value", err)
		}
	}
	h := hdr(HeaderHop, tok(7)+", "+tok(7))
	_, err := ParseInbound(h)
	if err == nil || strings.Contains(err.Error(), tok(7)) {
		t.Errorf("duplicate error %v names the entry", err)
	}
}

// TestParseInboundCountsIgnoredValues: every value ParseInbound does not take
// as sent counts federation_header_ignored once; taken values count nothing.
func TestParseInboundCountsIgnoredValues(t *testing.T) {
	for _, c := range []struct {
		h    http.Header
		want uint64
	}{
		{hdr(HeaderSensitivity, "public", HeaderEgress, "any", HeaderClass, "background", HeaderMaxWait, "1", HeaderPipeline, "a"), 0},
		{hdr(HeaderMaxWait, "120001"), 1},
		{hdr(HeaderPipeline, "A"), 1},
		{hdr(HeaderSensitivity, "secret"), 1},
		{hdr(HeaderEgress, "none"), 1},
		{hdr(HeaderClass, "urgent"), 1},
		{hdr(HeaderSensitivity, "x", HeaderEgress, "x", HeaderClass, "x", HeaderMaxWait, "x", HeaderPipeline, "X"), 5},
	} {
		before := CounterSnapshot().HeaderIgnored
		if _, err := ParseInbound(c.h); err != nil {
			t.Fatalf("%v: %v", c.h, err)
		}
		if got := CounterSnapshot().HeaderIgnored - before; got != c.want {
			t.Errorf("%v: federation_header_ignored +%d, want +%d", c.h, got, c.want)
		}
	}
}

func TestLoopFor(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	other := NewIdentity(uid(99), []byte("another instance key, 32 bytes!!"))
	own := self.MintToken()
	foreign := other.MintToken()
	for _, c := range []struct {
		name    string
		in      Inbound
		wantTok string
		loop    bool
	}{
		{"empty", Inbound{}, "", false},
		{"foreign chain", Inbound{Hop: []string{foreign, tok(1)}, Path: []string{uid(99)}}, "", false},
		{"own token first", Inbound{Hop: []string{own, foreign}}, own, true},
		{"own token last", Inbound{Hop: []string{tok(1), foreign, own}}, own, true},
		{"own id in path", Inbound{Hop: []string{foreign, tok(1)}, Path: []string{uid(99), testID}}, "", true},
		{"duplicate hop", Inbound{Hop: []string{tok(1), tok(1)}}, "", true},
		{"duplicate path", Inbound{Hop: []string{tok(1), tok(2)}, Path: []string{uid(1), uid(1)}}, "", true},
		{"own token and duplicate", Inbound{Hop: []string{tok(1), own, tok(1)}}, own, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			gotTok, loop := c.in.LoopFor(self)
			if gotTok != c.wantTok || loop != c.loop {
				t.Errorf("LoopFor = (%q, %v), want (%q, %v)", gotTok, loop, c.wantTok, c.loop)
			}
		})
	}
	// The zero Identity recognises nothing of its own, but a duplicate stays a loop.
	if tk, loop := (Inbound{Hop: []string{own}, Path: []string{testID}}).LoopFor(Identity{}); tk != "" || loop {
		t.Errorf("zero Identity: LoopFor = (%q, %v), want (\"\", false)", tk, loop)
	}
	if _, loop := (Inbound{Hop: []string{own, own}}).LoopFor(Identity{}); !loop {
		t.Error("zero Identity: a duplicate is no loop")
	}
	// The parsed chain of an own outgoing stamp comes back as a loop.
	h := http.Header{}
	Stamp(WithIdentity(t.Context(), self), h, Target{Tier: TierFederation})
	in, err := ParseInbound(h)
	if err != nil {
		t.Fatal(err)
	}
	if tk, loop := in.LoopFor(self); !loop || tk != in.Hop[0] {
		t.Errorf("own stamp parsed back: LoopFor = (%q, %v), want (%q, true)", tk, loop, in.Hop[0])
	}
	if _, loop := in.LoopFor(other); loop {
		t.Error("another instance sees a loop in a chain it is not part of")
	}
}

// TestHeaderNamesAreCanonical: the header constants are their own canonical
// form — the parser reads h[name] directly.
func TestHeaderNamesAreCanonical(t *testing.T) {
	for _, n := range []string{
		HeaderHop, HeaderPath, HeaderSensitivity, HeaderEgress, HeaderClass, HeaderMaxWait,
		HeaderPipeline, HeaderEmbedSpaceRequire, HeaderRequestID, HeaderInstance, HeaderReason,
		HeaderLoopToken, HeaderEmbedSpace,
	} {
		if c := http.CanonicalHeaderKey(n); c != n {
			t.Errorf("%q is not canonical (%q)", n, c)
		}
	}
}

func TestReason(t *testing.T) {
	for _, r := range []Reason{ReasonBusy, ReasonDraining, ReasonLoop, ReasonDepth, ReasonHopRequired, ReasonPeerMismatch, ReasonEmbedSpace} {
		h := http.Header{}
		SetReason(h, r)
		if got := h.Values(HeaderReason); len(got) != 1 || got[0] != string(r) {
			t.Errorf("SetReason(%q) wrote %q", r, got)
		}
		if got := ReasonFrom(h); got != r {
			t.Errorf("ReasonFrom = %q, want %q", got, r)
		}
	}
	h := http.Header{}
	SetReason(h, Reason("overloaded"))
	if _, ok := h[HeaderReason]; ok {
		t.Errorf("SetReason with a reason outside the vocabulary wrote %q", h.Get(HeaderReason))
	}
	for _, h := range []http.Header{
		{}, hdr(HeaderReason, "overloaded"), hdr(HeaderReason, "BUSY"), hdr(HeaderReason, "busy", HeaderReason, "busy"), hdr(HeaderReason, "busy, loop"),
	} {
		if got := ReasonFrom(h); got != "" {
			t.Errorf("ReasonFrom(%v) = %q, want \"\"", h, got)
		}
	}
	if got := ReasonFrom(hdr(HeaderReason, " loop ")); got != ReasonLoop {
		t.Errorf("ReasonFrom with OWS = %q", got)
	}
}

// addLines adds one header line per "\n"-separated part of v; "" adds none.
func addLines(h http.Header, name, v string) {
	if v == "" {
		return
	}
	for _, line := range strings.Split(v, "\n") {
		h.Add(name, line)
	}
}

func FuzzParseInbound(f *testing.F) {
	f.Add(tok(1)+", "+tok(2), uid(1), "personal", "local", "interactive", "5000", "dream-eval", "0123456789abcdef")
	f.Add(tok(1)+"\n"+tok(2), uid(1)+"\n"+uid(2), "secret", "none", "urgent", "120001", "A", "")
	f.Add(tok(1)+", "+tok(1), "", "", "", "", "", "", "")
	f.Add(strings.Join(toks(1, 16), ","), strings.Join(uids(1, 16), ","), "public", "any", "external_bulk", "0", "a", "")
	f.Add(strings.Join(toks(1, 17), ","), "", "", "", "", "", "", "")
	f.Add(tok(1), strings.ToUpper(uid(1)), "", "", "", "", "", "")
	f.Add("x", "y", "z", "w", "v", "-1", "-", "g")
	f.Add(tok(1)+",,", "", " internal ", " local ", "background", " 7 ", "a:b", "")
	f.Fuzz(func(t *testing.T, hop, path, sens, egress, class, wait, pipeline, embed string) {
		h := http.Header{}
		addLines(h, HeaderHop, hop)
		addLines(h, HeaderPath, path)
		addLines(h, HeaderSensitivity, sens)
		addLines(h, HeaderEgress, egress)
		addLines(h, HeaderClass, class)
		addLines(h, HeaderMaxWait, wait)
		addLines(h, HeaderPipeline, pipeline)
		addLines(h, HeaderEmbedSpaceRequire, embed)
		in, err := ParseInbound(h)
		if err != nil {
			var inv *InvalidHeaderError
			var dup *DuplicateEntryError
			switch {
			case errors.As(err, &inv):
				if !sameInbound(in, Inbound{}) {
					t.Fatalf("400 with a non-zero Inbound %+v", in)
				}
			case errors.As(err, &dup):
				if _, loop := in.LoopFor(Identity{}); !loop {
					t.Fatalf("508 Inbound %+v is no loop", in)
				}
			default:
				t.Fatalf("error of unknown class %T: %v", err, err)
			}
			return
		}
		checkInboundInvariants(t, in)
		h2 := formatInbound(in)
		in2, err := ParseInbound(h2)
		if err != nil {
			t.Fatalf("Parse(Format(%+v)) = %v", in, err)
		}
		if !sameInbound(in, in2) {
			t.Fatalf("Parse∘Format∘Parse ≠ Parse:\n%+v\n%+v", in, in2)
		}
		if h3 := formatInbound(in2); !reflect.DeepEqual(h2, h3) {
			t.Fatalf("Format not idempotent: %v vs %v", h2, h3)
		}
	})
}

// checkInboundInvariants: what every accepted Inbound satisfies.
func checkInboundInvariants(t *testing.T, in Inbound) {
	t.Helper()
	if len(in.Hop) > MaxHopEntries || len(in.Path) > len(in.Hop) {
		t.Fatalf("list bounds: hop %d, path %d", len(in.Hop), len(in.Path))
	}
	for _, e := range in.Hop {
		if !validToken(e) {
			t.Fatalf("accepted hop entry %q", e)
		}
	}
	for _, e := range in.Path {
		if !validUUID(e) {
			t.Fatalf("accepted path entry %q", e)
		}
	}
	if hasDuplicate(in.Hop) || hasDuplicate(in.Path) {
		t.Fatalf("accepted a duplicate: %+v", in)
	}
	if in.Sensitivity != "" && !knownSensitivity(in.Sensitivity) {
		t.Fatalf("sensitivity %q", in.Sensitivity)
	}
	if in.Class != "" && !knownClass(in.Class) {
		t.Fatalf("class %q", in.Class)
	}
	if in.MaxWait < 0 || in.MaxWait > MaxWaitCeiling || (!in.HasMaxWait && in.MaxWait != 0) || in.MaxWait%time.Millisecond != 0 {
		t.Fatalf("max wait %v (has %v)", in.MaxWait, in.HasMaxWait)
	}
	if in.Pipeline != "" && !validPipeline(in.Pipeline) {
		t.Fatalf("pipeline %q", in.Pipeline)
	}
	if in.EmbedSpaceRequire != "" && !validEmbedSpace(in.EmbedSpaceRequire) {
		t.Fatalf("embed space %q", in.EmbedSpaceRequire)
	}
}

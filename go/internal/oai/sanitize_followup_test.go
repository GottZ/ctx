package oai_test

// Pins of the follow-up hardening (Copilot review on PR #144, B-1/B-2): a
// URL or a host-prefixed path whose host is a row identity loses its path
// and query, and an absolute path behind any quoting or bracket delimiter is
// removed. Over-replacement is allowed, under-replacement is not: every pin
// only asserts what must be gone.

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// sanHostRows is a pool whose host is the Copilot example's: a scheme, a
// dotted host, a port; a second row on a host without port; a third named
// like a path segment.
func sanHostRows() []backends.Backend {
	return []backends.Backend{
		{Name: "gpu1", Host: "http://gpu.internal:8000", Model: "m1"},
		{Name: "edge", Host: "https://edge.internal", Model: "m2"},
		{Name: "spark", Host: "http://10.0.0.9:30000", Model: "m3"},
	}
}

// TestSanitizeURLOfAnIdentityHostLosesPathAndQuery (B-1): identities used
// to be replaced before the URL scrubber ran, which then no longer saw a
// scheme — "http://gpu.internal:8000/private/X?key=Y" became
// "<alias>/private/X?key=Y". Every form below keeps neither the path nor
// the query value.
func TestSanitizeURLOfAnIdentityHostLosesPathAndQuery(t *testing.T) {
	rows := sanHostRows()
	for _, tc := range []struct{ name, in string }{
		{"copilot case", "fetch http://gpu.internal:8000/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"host:port without scheme", "fetch gpu.internal:8000/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"host without port, path and query", "GET edge.internal/v1/SECRET_CANARY?token=TOKEN_CANARY refused"},
		{"upper-case host with scheme", "fetch HTTP://GPU.INTERNAL:8000/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"upper-case host without scheme", "fetch GPU.INTERNAL:8000/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"two URLs", "a http://gpu.internal:8000/p/SECRET_CANARY?k=TOKEN_CANARY and https://edge.internal/q/SECRET_CANARY2?t=TOKEN_CANARY2 b"},
		{"row name as path prefix", "load spark/models/SECRET_CANARY.gguf?rev=TOKEN_CANARY failed"},
		{"fragment", "see gpu.internal:8000/doc#SECRET_CANARY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sanLeaks(t, rows, tc.in, "SECRET_CANARY", "TOKEN_CANARY", "private", "gpu.internal", "edge.internal", "/v1", "models/")
		})
	}
}

// TestSanitizePathBehindEveryDelimiter (B-2): an absolute path is removed
// behind every quoting or bracket delimiter — the space, quotes and "(=,:"
// of before, and now "[", "{", "<", "`", "|", ">", ";" — and a Windows path
// likewise, also behind "_".
func TestSanitizePathBehindEveryDelimiter(t *testing.T) {
	rows := sanHostRows()
	for _, d := range []struct{ name, in string }{
		{"bracket", "cannot open [/srv/cache/PATH_CANARY.db]"},
		{"brace", "cannot open {/srv/cache/PATH_CANARY.db}"},
		{"angle", "cannot open </srv/cache/PATH_CANARY.db>"},
		{"backtick", "cannot open `/srv/cache/PATH_CANARY.db`"},
		{"pipe", "cannot open |/srv/cache/PATH_CANARY.db|"},
		{"greater", "redirect >/srv/cache/PATH_CANARY.db"},
		{"semicolon", "files a;/srv/cache/PATH_CANARY.db"},
		{"colon without space", "file:/srv/cache/PATH_CANARY.db"},
		{"windows bracket", `cannot open [C:\data\PATH_CANARY.db]`},
		{"windows backtick", "cannot open `C:\\data\\PATH_CANARY.db`"},
		{"windows pipe", `cannot open |C:\data\PATH_CANARY.db|`},
		{"windows underscore", `cannot open _C:\data\PATH_CANARY.db`},
	} {
		t.Run(d.name, func(t *testing.T) {
			sanLeaks(t, rows, d.in, "PATH_CANARY", "srv/cache", `data\`)
		})
	}
}

// TestSanitizeIPAddressLosesPathAndQuery (B-1, the rest of the order): the
// path scrubber runs before the IP scrubbers and sees no delimiter in
// "10.1.2.3:80/x?key=y" — an IP address that is no identity takes its path,
// query and fragment with it, IPv4 and IPv6 alike.
func TestSanitizeIPAddressLosesPathAndQuery(t *testing.T) {
	rows := sanHostRows()
	for _, tc := range []struct{ name, in string }{
		{"ipv4 with port", "fetch 192.0.2.7:8080/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"ipv4 without port", "fetch 192.0.2.7/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"ipv6 bracketed with port", "fetch [2001:db8::7]:8080/private/SECRET_CANARY?key=TOKEN_CANARY failed"},
		{"identity ipv4", "fetch 10.0.0.9:30000/private/SECRET_CANARY#TOKEN_CANARY failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sanLeaks(t, rows, tc.in, "SECRET_CANARY", "TOKEN_CANARY", "private")
		})
	}
}

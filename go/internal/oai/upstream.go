package oai

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
)

// The attempt-error mapping of the chat route (design/02 §4.10.2, masterplan
// K5/K29): what a failed walk tells the client, by the class of its last
// attempt. Admission failures never come here — they are the planner's
// (WriteAdmissionError); this is the route's own half (A03-W8 handover:
// "attempt errors are the route's mapping").

// relayTextMax bounds a relayed upstream 4xx text (design/02 §4.10.1, E-02-5:
// "sanitisierter Upstream-Text ≤ 512 B"): a short diagnostic, not a body.
const relayTextMax = 512

// The ctx texts of the attempt answers: generic, nothing of the row, the
// host or the upstream body in them (B4).
const (
	upstreamUnavailableMessage = "The model backends are temporarily unavailable; retry later."
	upstreamErrorMessage       = "The model backend failed to complete the request."
	upstreamTimeoutMessage     = "The model backend did not answer in time."
	upstreamRejectedMessage    = "The model backend rejected the request."
	contextLengthMessage       = "This model's maximum context length is %d tokens; reduce the length of the messages."
	contextLengthNoNumber      = "This model's maximum context length was exceeded; reduce the length of the messages."
	reapedMessage              = "The request was ended by the server's admission control; retry."
)

// failedAttempt is one failed wire attempt as the route saw it: the row and
// the raw error (the walker returns only the last, wrapped).
type failedAttempt struct {
	b   *backends.Backend
	err error
}

// attemptAnswer is the client answer of a failed walk.
type attemptAnswer struct {
	code        string
	message     string
	retryAfter  time.Duration
	shouldRetry bool
	// class is the backend class of the last attempt (llmlog RedactedErr).
	class string
	// status is the upstream status of the last attempt (0 = none).
	status int
}

// answerContext is what the mapping reads besides the error: the alias (its
// name replaces identities, its context_length is the number of the
// context-length text), the chain (the fallback window), whether upstream
// 4xx texts are relayed and the pool snapshot the sanitizer replaces.
type answerContext struct {
	alias    modelalias.Alias
	chain    []backends.Backend
	relay4xx bool
	snapshot func() []backends.Backend
}

// mapAttemptFailure maps the failed walk to its answer. last is the walk's
// last attempt; failed every failed attempt in order (the smallest upstream
// Retry-After of an exhausted chain comes from all of them).
func mapAttemptFailure(last failedAttempt, failed []failedAttempt, lastClass string, ac answerContext) attemptAnswer {
	err := last.err
	if lastClass == llmlog.AbortReaped || lastClass == llmlog.AbortPreempted {
		// The dispatcher ended the lease (reaper, design/03 §4.5): an
		// A-internal outcome a retry can fix.
		return attemptAnswer{code: CodeInternalError, message: reapedMessage, shouldRetry: true, class: lastClass}
	}
	if errors.Is(err, llm.ErrPassthroughProtocol) {
		// A row the chain build should have kept off — a code bug (K29).
		return attemptAnswer{code: CodeInternalError, message: "The server could not complete the request.", class: "server_fault"}
	}
	providerClass := ""
	if last.b != nil {
		providerClass = last.b.ProviderClass
	}
	class := backends.Classify(err, providerClass)
	var se *httpx.StatusError
	status := 0
	if errors.As(err, &se) {
		status = se.Code
	}
	a := attemptAnswer{class: class.String(), status: status}
	switch {
	case class == backends.ClassTimeout:
		a.code, a.message = CodeUpstreamTimeout, upstreamTimeoutMessage
	case class == backends.ClassServerFault:
		a.code, a.message = CodeUpstreamError, upstreamErrorMessage
	case class == backends.ClassBadRequest && se != nil && se.Code != http.StatusNotFound && se.Code >= 400:
		a.code, a.message = rejectedAnswer(se, ac)
	case class == backends.ClassBadRequest && se == nil:
		// A request-specific provider error event without a status: the
		// client's request, not the row (window rule, §4.11.1).
		a.code, a.message = CodeUpstreamRejected, upstreamRejectedMessage
	default:
		// Auth (ctx's own row credentials — never a 401 to the client,
		// B5), transport, gateway, rate limit, billing, no providers, a
		// last 404: the chain is exhausted for reasons of its rows.
		a.code, a.message, a.shouldRetry = CodeUpstreamUnavailable, upstreamUnavailableMessage, true
		a.retryAfter = smallestRetryAfter(failed)
	}
	return a
}

// rejectedAnswer is a request-specific upstream 4xx after the window rule:
// context_length_exceeded when the detector recognises the engine's
// wording, else upstream_rejected — with ctx's own text, or with the
// sanitized upstream text when gateway.relay_upstream_4xx_messages is on.
func rejectedAnswer(se *httpx.StatusError, ac answerContext) (string, string) {
	if contextLengthExceeded(se.Body) {
		if n := contextLength(ac.alias, ac.chain); n > 0 {
			return CodeContextLengthExceeded, fmt.Sprintf(contextLengthMessage, n)
		}
		return CodeContextLengthExceeded, contextLengthNoNumber
	}
	if ac.relay4xx {
		var snap []backends.Backend
		if ac.snapshot != nil {
			snap = ac.snapshot()
		}
		if t := sanitizeUpstreamText(upstreamMessage(se.Body), ac.alias.Name, snap); t != "" {
			return CodeUpstreamRejected, t
		}
	}
	return CodeUpstreamRejected, upstreamRejectedMessage
}

// contextLengthPatterns is the whitelist of the context-length detector
// (design/02 §4.10.1): the wordings of vLLM and OpenAI-shaped providers
// ("maximum context length is N tokens"), SGLang ("longer than the model's
// context length"), llama.cpp ("exceeds the available context size",
// type exceed_context_size_error) and the OpenAI code itself. Pinned per
// engine against fixtures (TestContextLengthDetectorFixtures). A text that
// matches none is upstream_rejected — the detector never guesses.
var contextLengthPatterns = regexp.MustCompile(`(?i)maximum context length|longer than the model'?s context length|exceeds? the available context size|exceed_context_size_error|context_length_exceeded`)

func contextLengthExceeded(body string) bool { return contextLengthPatterns.MatchString(body) }

// contextLength is the number of the context-length text: the alias's
// context_length, else the smallest known window of the chain's rows (the
// design/02 §3.2 default; rows of unknown window left out) — never a number
// from the upstream text.
func contextLength(a modelalias.Alias, chain []backends.Backend) int {
	if a.Policy.ContextLength.Set {
		return a.Policy.ContextLength.V
	}
	n := 0
	for i := range chain {
		if w := chain[i].NumCtx; w > 0 && (n == 0 || w < n) {
			n = w
		}
	}
	return n
}

// smallestRetryAfter is the smallest positive upstream Retry-After of the
// failed attempts (design/02 §4.10.2 "kleinste Upstream-Retry-After").
func smallestRetryAfter(failed []failedAttempt) time.Duration {
	var best time.Duration
	for _, f := range failed {
		var se *httpx.StatusError
		if errors.As(f.err, &se) && se.RetryAfter > 0 && (best == 0 || se.RetryAfter < best) {
			best = se.RetryAfter
		}
	}
	return best
}

// upstreamMessage is the error text of an upstream 4xx body: error.message,
// message or detail of a JSON body, else the body itself.
func upstreamMessage(body string) string {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  string          `json:"detail"`
	}
	if json.Unmarshal([]byte(body), &v) != nil {
		return body
	}
	var inner struct {
		Message string `json:"message"`
	}
	switch {
	case len(v.Error) > 0 && json.Unmarshal(v.Error, &inner) == nil && inner.Message != "":
		return inner.Message
	case len(v.Error) > 0:
		var s string
		if json.Unmarshal(v.Error, &s) == nil && s != "" {
			return s
		}
	case v.Message != "":
		return v.Message
	case v.Detail != "":
		return v.Detail
	}
	return body
}

// The scrubbers of a relayed text (§4.10.1): URLs, userinfo authorities,
// IP addresses and absolute paths go before the text reaches a client.
// pathPattern and winPattern take a path behind every quoting or bracket
// delimiter — whitespace, quotes, "(=,:" and "[{<>`|;" (Copilot review on
// PR #144: "[/srv/cache/x.db]" and a backtick-quoted path kept theirs) —
// and stop before the closing one; winPattern also behind "_". An IP
// address takes its path, query or fragment with it (identitySuffix): the
// path scrubber runs before the IP scrubbers and sees no delimiter in
// "10.1.2.3:80/x?key=y".
// userinfoPattern is user[:password]@host[:port] without a scheme, taken as
// a whole — the host behind the @ too, a pool identity already replaced by
// the alias or not (Fremd-Review 2 F2-6 H-7 2). ipv6Pattern starts at a
// hex group or at a leading :: (::1, and the ::ffff: an IPv4-mapped address
// leaves once ipv4Pattern took its dotted tail, H-7 3) and takes the zone
// index (%eth0, %25eth0 in the URI form, H-7 1), the brackets and the port.
var (
	urlPattern      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	userinfoPattern = regexp.MustCompile(`[^\s"'<>()\[\]{},;=@]+@[^\s"'<>(){},;]*`)
	ipv4Pattern     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b` + identitySuffix)
	ipv6Pattern     = regexp.MustCompile(`(?i)\[?(?:\b[0-9a-f]{1,4}|:)(?::[0-9a-f]{0,4}){1,8}(?:%[0-9a-z_.~-]+)?\]?(?::\d+)?` + identitySuffix)
	pathPattern     = regexp.MustCompile("(?:^|[\\s\"'(=,:\\[{<>`|;])(/[^\\s\"'),\\]}>`|;]+)")
	winPattern      = regexp.MustCompile("(^|[^A-Za-z0-9])[A-Za-z]:\\\\[^\\s\"'\\]}>`|;]+")
	spacePattern    = regexp.MustCompile(`\s+`)
	zonePattern     = regexp.MustCompile(`(?i)%(?:25)?[0-9a-z_.~-]+`)
)

// sanitizeUpstreamText makes an upstream 4xx text relayable (§4.10.1,
// E-02-5): every identity string of the CURRENT pool snapshot (identities)
// is replaced by the alias name in ONE pass — case-insensitive, longest
// first, so the inserted alias is never rewritten again and a longer name
// wins over its prefix — then URLs, userinfo authorities, IP addresses and
// absolute paths are removed, whitespace is folded and the text is cut to
// relayTextMax bytes on a rune boundary. Over-replacement is accepted,
// under-replacement is not (H-7): an empty result falls back to ctx's own
// text. Matching is on bytes after Unicode case folding; homoglyphs are not
// folded (docs/security.md).
func sanitizeUpstreamText(text, alias string, snap []backends.Backend) string {
	// Whole URLs and userinfo authorities go FIRST: replacing an identity
	// inside one first leaves "<alias>/private/x?key=…", which neither the
	// URL scrubber (no scheme left) nor the path scrubber (no delimiter)
	// recognizes (Copilot review on PR #144).
	text = urlPattern.ReplaceAllString(text, "")
	text = userinfoPattern.ReplaceAllString(text, "")
	if re := identityPattern(snap); re != nil {
		text = re.ReplaceAllLiteralString(text, alias)
	}
	// A zone index outlives the replacement of an address that is itself an
	// identity of a row ("fd00::17%25en0" ⇒ "<alias>%25en0"): every zone
	// suffix goes (Fremd-Review 3 R3-3; over-replacement is accepted).
	text = zonePattern.ReplaceAllString(text, "")
	text = winPattern.ReplaceAllString(text, "${1}")
	text = pathPattern.ReplaceAllStringFunc(text, func(m string) string {
		if i := strings.IndexByte(m, '/'); i >= 0 {
			return m[:i]
		}
		return m
	})
	text = ipv4Pattern.ReplaceAllString(text, "")
	text = ipv6Pattern.ReplaceAllStringFunc(text, func(m string) string {
		if strings.Count(m, ":") >= 2 {
			return ""
		}
		return m
	})
	text = strings.TrimSpace(spacePattern.ReplaceAllString(text, " "))
	return cutRunes(text, relayTextMax)
}

// shortIdentity is the length below which an identity string is replaced
// only as a whole word: a row named "a" must not rewrite every "a" of the
// text. A mechanism bound of the replacement, not a policy.
const shortIdentity = 3

// modelCuts are the separators a model id is cut at for its prefixes
// (Fremd-Review 2 F2-6 H-7 5): the - of a family, version or size segment,
// the : of an Ollama or OpenRouter tag, the / of a namespace.
const modelCuts = "-:/"

// identities are the identity strings of a pool snapshot the relayed text
// must not carry (§4.10.1): every row's name, its host with and without
// scheme, with and without port, its model and every model_map model — each
// also by its last path segment (meta-llama/Llama-3.3 ⇒ Llama-3.3) — and its
// provider class. A model also counts by every prefix of the full id and of
// the basename that ends at a modelCuts separator and still holds one
// (H-7 5: meta-llama, meta-llama/Llama-3.3-70B, Llama-3.3-70B, Llama-3.3); a
// one-segment prefix (Llama, meta, the MODEL of MODEL-CANARY) is a word of
// ordinary error text, not an identity of the row.
func identities(snap []backends.Backend) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if k := strings.ToLower(s); s != "" && !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	addPrefixes := func(m string) {
		for i := 0; i < len(m); i++ {
			if p := m[:i]; strings.IndexByte(modelCuts, m[i]) >= 0 && strings.ContainsAny(p, modelCuts) {
				add(p)
			}
		}
	}
	addModel := func(m string) {
		m = strings.TrimSpace(m)
		add(m)
		addPrefixes(m)
		if i := strings.LastIndexByte(m, '/'); i >= 0 {
			add(m[i+1:])
			addPrefixes(m[i+1:])
		}
	}
	for i := range snap {
		b := &snap[i]
		add(b.Name)
		add(b.Host)
		if _, rest, ok := strings.Cut(b.Host, "://"); ok {
			add(rest)
		}
		if u, err := url.Parse(b.Host); err == nil && u.Host != "" {
			add(u.Host)
			add(u.Hostname())
		}
		addModel(b.Model)
		for _, spec := range b.ModelMap {
			addModel(spec.Model)
		}
		add(b.ProviderClass)
	}
	return out
}

// foldKey is the canonical case fold of s for the sort of identityPattern:
// every rune replaced by the smallest member of its unicode.SimpleFold
// orbit — the folding (?i) matches by. strings.ToLower is not that fold
// ("ſ" stays "ſ" but matches "s"), and a sort by it could put a prefix
// before the identity it is a prefix of (Fremd-Review 3 R3-2: row "ſpark"
// next to host "spark-gpu" left "ALIAS-gpu").
func foldKey(s string) string {
	return strings.Map(func(r rune) rune {
		m := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			m = min(m, f)
		}
		return m
	}, s)
}

// identityPattern is one case-insensitive alternation over the identities,
// longest first; short ones only as whole words. nil = nothing to replace.
// A match takes what is glued to the identity with it — a port and a path,
// query or fragment (identitySuffix): "gpu.internal:8000/private/x?key=y"
// without a scheme, or a row name used as a path prefix, goes as a whole
// (Copilot review on PR #144: the path and the query value stayed).
// The order is the reverse lexical order of the lower-cased strings: at one
// position of the text only identities that are prefixes of one another
// compete, and each comes before its own prefixes, so the longest still
// wins — and the prefixes of one model id stay adjacent, so regexp/syntax
// factors them into one branch. Sorted by length, the model prefixes of a
// 20-row pool made the match about 50 times slower (Fremd-Review 2 F2-6
// H-7 cost, BenchmarkSanitizeUpstreamText512).
func identityPattern(snap []backends.Backend) *regexp.Regexp {
	ids := identities(snap)
	if len(ids) == 0 {
		return nil
	}
	keys := make(map[string]string, len(ids))
	for _, id := range ids {
		keys[id] = foldKey(id)
	}
	sort.SliceStable(ids, func(i, j int) bool { return keys[ids[i]] > keys[ids[j]] })
	alts := make([]string, len(ids))
	for i, id := range ids {
		alts[i] = regexp.QuoteMeta(id)
		if len(id) < shortIdentity {
			alts[i] = `\b` + alts[i] + `\b`
		}
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)` + identitySuffix)
}

// identitySuffix is the port and the path, query or fragment glued to an
// identity, up to the next whitespace, quote or angle bracket.
const identitySuffix = "(?::[0-9]+)?(?:[/?#][^\\s\"'<>`]*)?"

// cutRunes cuts s to at most n bytes on a rune boundary.
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// writeRetryAfter sets Retry-After in whole seconds, rounded up (0 = none).
func writeRetryAfter(h http.Header, d time.Duration) {
	if d <= 0 {
		return
	}
	h.Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
}

// writeRetryAfterMs sets retry-after-ms in whole milliseconds, rounded up
// (0 = none): the OpenAI SDKs read it before Retry-After (extern-recherche
// §1.7), so it carries the exact wait the seconds header rounds away.
func writeRetryAfterMs(h http.Header, d time.Duration) {
	if d <= 0 {
		return
	}
	h.Set("retry-after-ms", strconv.FormatInt(int64((d+time.Millisecond-1)/time.Millisecond), 10))
}

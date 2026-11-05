package fedhop

// The hop header grammar (design/04 §4.1, table "Header-Grammatik"): the
// header names, the protocol constants, ParseInbound with its two error
// classes and the loop test of a parsed chain.

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Header names of the hop contract, each its own canonical form. The request
// headers are Tier L (Ctx-Hop, to every target but openrouter) and Tier F
// (the rest, to ctx rows only); the last four are response headers. No X-
// prefix (RFC 6648), and deliberately not RFC 8586 CDN-Loop: intermediaries
// in front of ctx append CDN-Loop entries of their own, so a strict parser
// would refuse legitimate traffic and a tolerant one would lose the
// fail-closed property (docs/architecture.md, "Federation hop headers").
const (
	HeaderHop               = "Ctx-Hop"
	HeaderPath              = "Ctx-Path"
	HeaderSensitivity       = "Ctx-Sensitivity"
	HeaderEgress            = "Ctx-Egress"
	HeaderClass             = "Ctx-Class"
	HeaderMaxWait           = "Ctx-Max-Wait-Ms"
	HeaderPipeline          = "Ctx-Pipeline"
	HeaderEmbedSpaceRequire = "Ctx-Embed-Space-Require"
	HeaderRequestID         = "X-Request-Id" // canonical spelling of X-Request-ID

	HeaderInstance   = "Ctx-Instance"
	HeaderReason     = "Ctx-Reason"
	HeaderLoopToken  = "Ctx-Loop-Token" //nolint:gosec // G101 false positive: a header NAME; the hop token it carries is public by design (unlinkable, design/04 §4.1)
	HeaderEmbedSpace = "Ctx-Embed-Space"
)

// Protocol constants of the grammar. They are part of the wire contract that
// every ctx instance parses alike — an instance with another value would
// refuse what its peers accept or accept what they refuse — so they are
// named constants, not settings (A-E2 "hardcoded-legitim"). The policy values
// around them are settings of their own waves: federation.max_depth (F-W8,
// at most MaxHopEntries) and federation.peer_max_wait_* (F-W6, through
// SetPeerMaxWait; clamped to MaxWaitCeiling).
const (
	// MaxHopEntries is the hard parse limit of Ctx-Hop and Ctx-Path: a list
	// with more entries is refused (400). It bounds the parse work of one
	// request and the header a chain can grow to (16 × 34 bytes).
	MaxHopEntries = 16
	// MaxWaitCeiling is the largest Ctx-Max-Wait-Ms a receiver takes (120 000);
	// a larger value is ignored and the receiver keeps its own budget.
	MaxWaitCeiling = 120 * time.Second
	// MaxPipelineLen is the length limit of Ctx-Pipeline.
	MaxPipelineLen = 64
	// MaxRequestIDLen is the length limit of the X-Request-ID Stamp sends —
	// the limit of the RequestID middleware (handler/middleware.go).
	MaxRequestIDLen = 64
	// TokenLen is the length of a hop token: hex of 8 nonce and 8 MAC bytes.
	TokenLen = 2 * (nonceLen + macLen)
	// EmbedSpaceLen is the length of an embed space id (16 hex digits).
	EmbedSpaceLen = 16
)

// The two values of Ctx-Egress.
const (
	egressLocal = "local"
	egressAny   = "any"
)

// Inbound is the parsed, validated federation state of an incoming request.
// Absent headers are zero values; a present value ParseInbound could not
// take as sent is either its fail-closed value (Sensitivity credentials,
// EgressLocal true, Class background) or dropped (MaxWait, Pipeline).
type Inbound struct {
	// Hop is the Ctx-Hop chain in order, Path the Ctx-Path instance ids.
	Hop, Path []string
	// Sensitivity is "" when the header was absent.
	Sensitivity string
	// EgressLocal is Ctx-Egress: local (or an unreadable value).
	EgressLocal bool
	// Class is "" when the header was absent (or the ingress removed it, F-W9).
	Class string
	// MaxWait is the Ctx-Max-Wait-Ms budget; HasMaxWait tells a sent 0 (do not
	// wait at all — the most restrictive budget) from an absent or ignored one.
	// Read both through Wait: it also keeps a hand-built MaxWait > 0 whose
	// HasMaxWait was forgotten (design/04 §4.1 had "0 = absent").
	MaxWait    time.Duration
	HasMaxWait bool
	// Pipeline is "" when absent or invalid.
	Pipeline string
	// EmbedSpaceRequire is the embed space the sender pinned, "" when absent.
	EmbedSpaceRequire string
	// ReceivedAt is the parse time; Stamp charges the time since against MaxWait.
	ReceivedAt time.Time
}

// InvalidHeaderError is the 400 class of ParseInbound (federation_header_invalid):
// a hop list or a restriction that cannot be read — without a readable chain
// there is no loop protection, and an unreadable embed space must not count
// as no condition. Reason never carries the header value.
type InvalidHeaderError struct {
	Header string // canonical header name
	Reason string // fixed text, never the value
}

func (e *InvalidHeaderError) Error() string {
	return "fedhop: invalid " + e.Header + " header: " + e.Reason
}

// DuplicateEntryError is the 508 class of ParseInbound (federation_loop): a
// token or instance id twice in one chain — the request has passed a hop
// before. ParseInbound returns the complete Inbound with it, so LoopFor on
// that value reports the loop as well.
type DuplicateEntryError struct {
	Header string // canonical header name
}

func (e *DuplicateEntryError) Error() string {
	return "fedhop: " + e.Header + " repeats an entry"
}

func invalid(header, reason string) error {
	return &InvalidHeaderError{Header: header, Reason: reason}
}

// ParseInbound parses the federation request headers of h (design/04 §4.1).
// A 400 error (*InvalidHeaderError) returns the zero Inbound: an unreadable
// Ctx-Hop or Ctx-Path (more than MaxHopEntries entries, an empty element, an
// entry outside its grammar, len(Path) > len(Hop)), an unreadable
// Ctx-Embed-Space-Require, or a federation header under a non-canonical key
// (only a hand-built map has one; reading the canonical key alone would
// miss it). A 508 error (*DuplicateEntryError) returns the parsed Inbound.
// Every other header never fails: an unknown value takes its fail-closed
// value or is dropped, counted as federation_header_ignored. Several lines
// of a list header are one list (RFC 9110 §5.3); several lines of a
// one-value header are an unreadable value. Values are compared exactly —
// lower case is the grammar, upper case is unknown.
func ParseInbound(h http.Header) (Inbound, error) {
	if err := checkCanonicalKeys(h); err != nil {
		return Inbound{}, err
	}
	hop, err := parseList(h, HeaderHop, validToken, "is not a hop token (32 lower-case hex digits)")
	if err != nil {
		return Inbound{}, err
	}
	path, err := parseList(h, HeaderPath, validUUID, "is not a lower-case UUID")
	if err != nil {
		return Inbound{}, err
	}
	if len(path) > len(hop) {
		return Inbound{}, invalid(HeaderPath, "more entries than "+HeaderHop)
	}
	space, err := parseEmbedSpaceRequire(h)
	if err != nil {
		return Inbound{}, err
	}
	in := Inbound{Hop: hop, Path: path, EmbedSpaceRequire: space, ReceivedAt: time.Now()}
	in.Sensitivity = parseSensitivity(h)
	in.EgressLocal = parseEgress(h)
	in.Class = parseClass(h)
	in.MaxWait, in.HasMaxWait = parseMaxWait(h)
	in.Pipeline = parsePipeline(h)
	switch {
	case hasDuplicate(hop):
		return in, &DuplicateEntryError{Header: HeaderHop}
	case hasDuplicate(path):
		return in, &DuplicateEntryError{Header: HeaderPath}
	}
	return in, nil
}

// LoopFor reports whether in is a loop for the instance id: one of its own
// hop tokens is in the chain (ownToken is the first of them — the echo of a
// 508), its instance id is in the path, or the chain repeats an entry. The
// zero Identity recognises nothing of its own; a repeated entry is a loop
// for every caller.
func (in Inbound) LoopFor(id Identity) (ownToken string, loop bool) {
	for _, t := range in.Hop {
		if id.IsOwnToken(t) {
			return t, true
		}
	}
	if id.ID != "" && slices.Contains(in.Path, id.ID) {
		return "", true
	}
	return "", hasDuplicate(in.Hop) || hasDuplicate(in.Path)
}

// Wait returns the inbound wait budget and whether there is one: a sent
// Ctx-Max-Wait-Ms (0 included — do not wait at all) or a non-zero MaxWait of
// a hand-built Inbound; a negative MaxWait is a budget of 0. The receiver's
// MaxWait_eff = min(own budget, Wait) and the sender's Stamp both read it
// here, so a forgotten HasMaxWait narrows instead of dropping the budget.
func (in Inbound) Wait() (time.Duration, bool) {
	if !in.HasMaxWait && in.MaxWait == 0 {
		return 0, false
	}
	return max(in.MaxWait, 0), true
}

// requestHeaders are the headers ParseInbound reads.
var requestHeaders = []string{
	HeaderHop, HeaderPath, HeaderSensitivity, HeaderEgress, HeaderClass,
	HeaderMaxWait, HeaderPipeline, HeaderEmbedSpaceRequire,
}

// checkCanonicalKeys refuses a federation request header under a key that
// is not its canonical form. net/http canonicalises every key of a parsed
// request; a hand-built map may not, and h[canonical] would not see it.
func checkCanonicalKeys(h http.Header) error {
	for k := range h {
		if !hasPrefixFold(k, "ctx-") {
			continue
		}
		if c := http.CanonicalHeaderKey(k); c != k && slices.Contains(requestHeaders, c) {
			return invalid(c, "non-canonical header name")
		}
	}
	return nil
}

// parseList reads a list header over all its lines: elements separated by
// commas with optional whitespace (SP/HTAB) around them, no empty element,
// at most MaxHopEntries, each valid. Scanning stops at the first error, so
// a megabyte of commas costs one pass and no allocation per element.
func parseList(h http.Header, name string, valid func(string) bool, what string) ([]string, error) {
	lines := h[name]
	if len(lines) == 0 {
		return nil, nil
	}
	var out []string
	for _, line := range lines {
		rest, more := line, true
		for more {
			var elem string
			elem, rest, more = strings.Cut(rest, ",")
			elem = trimOWS(elem)
			switch {
			case elem == "":
				return nil, invalid(name, "empty list element")
			case len(out) == MaxHopEntries:
				return nil, invalid(name, "more than "+strconv.Itoa(MaxHopEntries)+" entries")
			case !valid(elem):
				return nil, invalid(name, "entry "+strconv.Itoa(len(out)+1)+" "+what)
			}
			out = append(out, elem)
		}
	}
	return out, nil
}

// single returns the value of a one-value header, trimmed of SP/HTAB:
// present false when absent; several lines yield "" (never a valid value).
func single(h http.Header, name string) (value string, present bool) {
	switch lines := h[name]; len(lines) {
	case 0:
		return "", false
	case 1:
		return trimOWS(lines[0]), true
	default:
		return "", true
	}
}

func parseSensitivity(h http.Header) string {
	v, ok := single(h, HeaderSensitivity)
	switch {
	case !ok:
		return ""
	case knownSensitivity(v):
		return v
	}
	headerIgnored.Add(1)
	return sensCredentials
}

func parseEgress(h http.Header) bool {
	v, ok := single(h, HeaderEgress)
	switch {
	case !ok, v == egressAny:
		return false
	case v == egressLocal:
		return true
	}
	headerIgnored.Add(1)
	return true
}

func parseClass(h http.Header) string {
	v, ok := single(h, HeaderClass)
	switch {
	case !ok:
		return ""
	case knownClass(v):
		return v
	}
	headerIgnored.Add(1)
	return classBackground
}

func parseMaxWait(h http.Header) (time.Duration, bool) {
	v, ok := single(h, HeaderMaxWait)
	if !ok {
		return 0, false
	}
	if d, ok := parseMillis(v); ok {
		return d, true
	}
	headerIgnored.Add(1)
	return 0, false
}

// parseMillis reads a canonical decimal 0 … 120 000: digits only, no sign, no
// leading zero, at most six digits (no overflow before the range check).
func parseMillis(v string) (time.Duration, bool) {
	if v == "" || len(v) > 6 || (len(v) > 1 && v[0] == '0') {
		return 0, false
	}
	n := 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	d := time.Duration(n) * time.Millisecond
	if d > MaxWaitCeiling {
		return 0, false
	}
	return d, true
}

func parsePipeline(h http.Header) string {
	v, ok := single(h, HeaderPipeline)
	switch {
	case !ok:
		return ""
	case validPipeline(v):
		return v
	}
	headerIgnored.Add(1)
	return ""
}

func parseEmbedSpaceRequire(h http.Header) (string, error) {
	v, ok := single(h, HeaderEmbedSpaceRequire)
	switch {
	case !ok:
		return "", nil
	case validEmbedSpace(v):
		return v, nil
	}
	return "", invalid(HeaderEmbedSpaceRequire, "is not "+strconv.Itoa(EmbedSpaceLen)+" lower-case hex digits")
}

// formatList writes a list header value: elements joined by ", ".
func formatList(list []string) string { return strings.Join(list, ", ") }

// appendList writes list plus last as a list header value, without touching
// list's backing array (an Inbound in a request context is shared).
func appendList(list []string, last string) string {
	if len(list) == 0 {
		return last
	}
	return formatList(list) + ", " + last
}

// formatMillis writes a duration as Ctx-Max-Wait-Ms (whole milliseconds).
func formatMillis(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func hasDuplicate(list []string) bool {
	for i := range list {
		if slices.Contains(list[i+1:], list[i]) {
			return true
		}
	}
	return false
}

func trimOWS(s string) string { return strings.Trim(s, " \t") }

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// validToken: 32 lower-case hex digits.
func validToken(s string) bool { return len(s) == TokenLen && isLowerHex(s) }

// validUUID: the canonical lower-case 8-4-4-4-12 form (any version — the
// id is compared, not interpreted).
func validUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func validEmbedSpace(s string) bool { return len(s) == EmbedSpaceLen && isLowerHex(s) }

// validPipeline: [a-z0-9][a-z0-9:_-]{0,63}.
func validPipeline(s string) bool {
	if s == "" || len(s) > MaxPipelineLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case i > 0 && (c == ':' || c == '_' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// validRequestID: [0-9A-Fa-f-]{1,64} — exactly what the receiver's
// RequestID middleware accepts (handler/middleware.go isValidRequestID); an
// id it would replace is not sent. A wider server-side id format (K28) widens
// both sides together.
func validRequestID(s string) bool {
	if s == "" || len(s) > MaxRequestIDLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// Reason is the machine-readable refusal reason of a ctx answer (Ctx-Reason,
// design/04 §4.1 and §4.13): A sets it through SetReason in its error
// writers, B reads it through ReasonFrom to classify the answer of a ctx row.
type Reason string

// The Ctx-Reason vocabulary.
const (
	ReasonBusy         Reason = "busy"          // admission refused on A: capacity, not health
	ReasonDraining     Reason = "draining"      // A is shutting down
	ReasonLoop         Reason = "loop"          // 508: the request has been here before
	ReasonDepth        Reason = "depth"         // 508: the chain is longer than federation.max_depth
	ReasonHopRequired  Reason = "hop_required"  // a peer request without Ctx-Path
	ReasonPeerMismatch Reason = "peer_mismatch" // the peer key belongs to another instance
	ReasonEmbedSpace   Reason = "embed_space"   // the pinned embed space is not served
)

func (r Reason) known() bool {
	switch r {
	case ReasonBusy, ReasonDraining, ReasonLoop, ReasonDepth, ReasonHopRequired, ReasonPeerMismatch, ReasonEmbedSpace:
		return true
	}
	return false
}

// SetReason sets Ctx-Reason on an answer. A reason outside the vocabulary
// sets nothing: B would ignore it, and an error writer must not invent one.
func SetReason(h http.Header, r Reason) {
	if r.known() {
		h.Set(HeaderReason, string(r))
	}
}

// ReasonFrom reads Ctx-Reason of an answer: "" when absent, outside the
// vocabulary or on several lines (B then classifies by status alone).
func ReasonFrom(h http.Header) Reason {
	if v, ok := single(h, HeaderReason); ok && Reason(v).known() {
		return Reason(v)
	}
	return ""
}

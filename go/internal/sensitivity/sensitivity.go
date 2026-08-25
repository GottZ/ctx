package sensitivity

import (
	"regexp"
	"strings"
)

// Match describes why content was flagged as credentials. Reason is a short,
// log-/metadata-safe label — it NEVER echoes the matched secret itself (the
// detector must not become the leak it prevents).
type Match struct {
	Kind   string `json:"kind"`   // structured rule that fired (aws-key, pem-private-key, …)
	Reason string `json:"reason"` // human-readable, secret-free explanation
}

// Detection thresholds (design 03 §2.3c). Tunable after the dry-run corpus
// measurement — the first live classify run is gated on the dry-run FP rate.
const (
	// base64MinLen / base64MinEntropy gate a raw high-entropy blob. Prose and
	// identifiers sit below 4.5 bits/char; uniform base64 keys sit near 6.0.
	base64MinLen     = 32
	base64MinEntropy = 4.5

	// assignMinEntropy gates the VALUE of a secret-looking assignment
	// (password=…, token:…). A real secret is high-entropy; "password=changeme"
	// or "token: <your-token>" are not.
	assignMinLen     = 8
	assignMinEntropy = 3.0
)

var (
	// Structured, high-precision signals — a hit here is credentials with
	// negligible false-positive risk.

	// reAWSKey: AWS access key IDs (AKIA/ASIA/AROA/AIDA + 16 base32 chars).
	reAWSKey = regexp.MustCompile(`\b(?:AKIA|ASIA|AROA|AIDA)[0-9A-Z]{16}\b`)

	// rePEMPrivate: PEM private-key headers ONLY. Public certs (-----BEGIN
	// CERTIFICATE-----) and PGP messages are deliberately NOT matched — they
	// are not secrets, and this corpus documents PEM formats in prose.
	rePEMPrivate = regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----`)

	// reJWT: three base64url segments. The leading eyJ ( = {" base64url) makes
	// this near-zero FP.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)

	// reTokenPrefix: vendor token formats with distinctive prefixes.
	reTokenPrefix = regexp.MustCompile(`\b(?:` +
		`sk-[A-Za-z0-9]{20,}` + // OpenAI / Anthropic-style
		`|ghp_[A-Za-z0-9]{36}|gho_[A-Za-z0-9]{36}|ghu_[A-Za-z0-9]{36}|ghs_[A-Za-z0-9]{36}` + // GitHub
		`|github_pat_[A-Za-z0-9_]{50,}` + // GitHub fine-grained
		`|xox[baprs]-[A-Za-z0-9-]{10,}` + // Slack
		`|AIza[A-Za-z0-9_\-]{35}` + // Google API key
		`|glpat-[A-Za-z0-9_\-]{20,}` + // GitLab PAT
		`)\b`)

	// reAssign: secret-semantic assignment. The captured VALUE (group 1) is
	// entropy- and placeholder-gated in code so that "password = changeme" /
	// "api_key: <redacted>" do not fire.
	reAssign = regexp.MustCompile(`(?i)(?:password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|private[_-]?key)["']?\s*[:=]\s*["']?([^\s"',;]+)`)

	// reBase64Blob / reHexBlob: generic high-entropy blobs, the lowest-precision
	// rules. Base64 is entropy-gated. Hex is LENGTH-gated at 64+ (git SHAs are
	// 40, abbreviated 7-12); a run whose immediately preceding token is a hash
	// label (SHA256:, sha512=, checksum, digest, fingerprint — reHashLabel) is
	// an integrity hash, not a secret, and is skipped (#22). Fail-closed: ONE
	// unlabelled 64+ hex run still flags the whole content.
	reBase64Blob = regexp.MustCompile(`\b[A-Za-z0-9+/]{32,}={0,2}\b`)
	reHexBlob    = regexp.MustCompile(`\b[0-9a-fA-F]{64,}\b`)

	// reHashLabel: hash-label token anchored to the END of the prefix window
	// before a hex run — the word (plus optional -sum suffix, separator and
	// opening quote/bracket) must directly precede the hex. Deliberate
	// trade-off (#22 option 1): a real 64-hex secret written directly behind a
	// hash label escapes this rule; the label context is still the strongest
	// available discriminator (entropy cannot split SHA-256 from hex keys —
	// both sit at ~4.0 bits/char).
	reHashLabel = regexp.MustCompile(
		`(?i)(?:sha-?(?:224|256|384|512)|sha3-\d{3}|blake[23][bs]?(?:-\d{3})?|checksum|digest|fingerprint|hash)` +
			"(?:sum)?\\s*[:=]?\\s*[\"'`(<\\[]*\\s*$")
)

// The two GENERIC kinds. They are named because a caller has to be able to
// tell them apart from the structured ones without re-listing every rule
// above: a structured hit is a credential, a generic hit is a high-entropy run
// that MAY be one. Only the payload layer makes anything of that distinction
// (handler/blob_core.go, W02-9) — on a block, both still mean credentials.
const (
	KindBase64Blob = "base64-blob"
	KindHexBlob    = "hex-blob"
)

// EntropyOnly reports whether a Match came from one of the two generic
// high-entropy rules rather than from a structured signal.
//
// The list is CLOSED and positive on purpose: a rule added to Scan above
// without a thought about this method reads as structured, which is the
// direction that keeps a caller's gate shut. The inverse spelling (list the
// structured kinds, default to generic) would turn every future rule into a
// silent downgrade at whatever call site trusts this answer.
func (m Match) EntropyOnly() bool {
	return m.Kind == KindBase64Blob || m.Kind == KindHexBlob
}

// hashLabelWindow is how many bytes before a hex run reHashLabel may search —
// generous enough for `fingerprint: "` plus whitespace, small enough to keep
// the label ADJACENT (a mention three sentences earlier must not whitelist).
const hashLabelWindow = 32

// hexBlobUnlabelled reports whether content carries a 64+ hex run WITHOUT a
// hash label directly in front of it (#22). Byte-window slicing may cut into
// a multi-byte rune at the window start — harmless, reHashLabel anchors at $.
func hexBlobUnlabelled(content string) bool {
	for _, loc := range reHexBlob.FindAllStringIndex(content, -1) {
		start := max(loc[0]-hashLabelWindow, 0)
		if !reHashLabel.MatchString(content[start:loc[0]]) {
			return true
		}
	}
	return false
}

// placeholderValue reports whether v is an obvious non-secret placeholder, so
// an entropy-passing but clearly-templated assignment value does not fire.
func placeholderValue(v string) bool {
	lv := strings.ToLower(strings.Trim(v, "<>{}[]()"))
	switch lv {
	case "changeme", "password", "secret", "example", "redacted", "your_token",
		"your-token", "yourtoken", "todo", "none", "null", "xxxxxxxx":
		return true
	}
	// Placeholder shapes: ${VAR}, $VAR, {{ var }}, <template>, all-x, your-*,
	// *_here. A leading "<" is template shape even when the closing ">" falls
	// outside the captured token (`<descriptive placeholder>` captures only
	// `<descriptive` — the real value was never present, #22).
	if strings.HasPrefix(v, "$") || strings.HasPrefix(v, "{{") || strings.HasPrefix(v, "<") ||
		strings.HasPrefix(lv, "your") || strings.HasSuffix(lv, "_here") ||
		strings.Trim(lv, "x") == "" || strings.Trim(lv, "*") == "" ||
		strings.Trim(lv, ".") == "" {
		return true
	}
	return false
}

// Scan reports whether content carries a credentials signal and, if so, why.
// Deterministic and side-effect-free. The first matching rule wins; rules are
// ordered most-precise first so the Reason names the strongest signal.
func Scan(content string) (Match, bool) {
	if loc := reAWSKey.FindString(content); loc != "" {
		return Match{Kind: "aws-key", Reason: "AWS access key id pattern"}, true
	}
	if rePEMPrivate.MatchString(content) {
		return Match{Kind: "pem-private-key", Reason: "PEM private key header"}, true
	}
	if reJWT.MatchString(content) {
		return Match{Kind: "jwt", Reason: "JWT (three base64url segments)"}, true
	}
	if reTokenPrefix.MatchString(content) {
		return Match{Kind: "token-prefix", Reason: "vendor API token prefix"}, true
	}
	for _, m := range reAssign.FindAllStringSubmatch(content, -1) {
		v := m[1]
		if len(v) >= assignMinLen && !placeholderValue(v) && shannonEntropy(v) >= assignMinEntropy {
			return Match{Kind: "secret-assignment", Reason: "high-entropy secret assignment"}, true
		}
	}
	for _, blob := range reBase64Blob.FindAllString(content, -1) {
		if len(blob) >= base64MinLen && shannonEntropy(blob) >= base64MinEntropy {
			return Match{Kind: KindBase64Blob, Reason: "high-entropy base64 blob"}, true
		}
	}
	if hexBlobUnlabelled(content) {
		return Match{Kind: KindHexBlob, Reason: "long hex blob (>=64 chars)"}, true
	}
	return Match{}, false
}

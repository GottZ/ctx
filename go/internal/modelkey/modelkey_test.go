package modelkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// The golden vector was recomputed outside this package (Python zlib.crc32
// and hashlib.sha256, reports/bau/W01-2.md). Its body is the first 43
// characters of the alphabet, so it also pins the order [0-9A-Za-z]; its
// CRC32 (299355605) is below 62^5, so the checksum also pins the '0' padding.
// It is the only key plaintext these tests ever print.
const (
	goldenBody = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefg"
	goldenSum  = "0KG44D"
	goldenRaw  = Prefix + goldenBody + goldenSum
	goldenHash = "22c9cd33159155bcf567457718df5a8b29177cb5ccffe4dbbf36a448414b9833"
	goldenHint = "ctxm_…G44D"
	// goldenAliasSum spells CRC32+2^32 in six Base62 digits: the same checksum
	// modulo 2^32, i.e. the second spelling a uint32 decoder would accept.
	goldenAliasSum = "50vJGH"
)

// keyShape is the canonical form spelled independently of alphabet and
// isBase62, so a mutant of either cannot bend the oracle along with it.
var keyShape = regexp.MustCompile(`^ctxm_[0-9A-Za-z]{49}$`)

// withSum appends the checksum recomputed over s — an input built this way
// passes the CRC comparison, so only the shape checks can refuse it.
func withSum(s string) string {
	sum := checksum([]byte(s))
	return s + string(sum[:])
}

func mustParse(t *testing.T, raw string) Key {
	t.Helper()
	k, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(golden) = %v", err)
	}
	return k
}

// goldenEntropy maps onto goldenBody through generate: rejected bytes
// (248…255, 250) in front of and between accepted ones, and accepted bytes
// above 61 that only reduce correctly modulo 62.
func goldenEntropy() []byte {
	in := []byte{248, 249, 250, 251, 252, 253, 254, 255}
	for i := 0; i < 21; i++ {
		in = append(in, byte(i))
	}
	in = append(in, 250)
	for i := 21; i < bodyLen; i++ {
		in = append(in, byte(i+62*(i%4)))
	}
	return in
}

func TestGoldenVector(t *testing.T) {
	if Len != 54 || len(goldenRaw) != Len {
		t.Fatalf("Len = %d, len(golden) = %d, want 54", Len, len(goldenRaw))
	}
	if got := checksum([]byte(Prefix + goldenBody)); string(got[:]) != goldenSum {
		t.Fatalf("checksum = %q, want %q", got, goldenSum)
	}
	k := mustParse(t, goldenRaw)
	if k.Plaintext() != goldenRaw {
		t.Fatalf("Plaintext = %q, want the golden key", k.Plaintext())
	}
	if k.Hash() != goldenHash {
		t.Fatalf("Hash = %s, want %s", k.Hash(), goldenHash)
	}
	want, _ := hex.DecodeString(goldenHash[:16])
	if hp := k.HashPrefix(); !bytes.Equal(hp[:], want) {
		t.Fatalf("HashPrefix = %x, want %x", hp, want)
	}
	if k.Hint() != goldenHint || len([]rune(k.Hint())) > 16 {
		t.Fatalf("Hint = %q (%d runes), want %q (≤ 16)", k.Hint(), len([]rune(k.Hint())), goldenHint)
	}
	g, err := generate(bytes.NewReader(goldenEntropy()))
	if err != nil || g.Plaintext() != goldenRaw {
		t.Fatalf("generate(goldenEntropy) = %q, %v; want the golden key", g.Plaintext(), err)
	}
}

// Every position × every other character of the alphabet valid there
// (Base62, plus "_" on the prefix positions) must be refused — 3299 flips.
func TestParseRejectsEverySingleCharFlip(t *testing.T) {
	total, accepted := 0, 0
	for pos := 0; pos < Len; pos++ {
		chars := alphabet
		if pos < len(Prefix) {
			chars += "_"
		}
		for i := 0; i < len(chars); i++ {
			c := chars[i]
			if c == goldenRaw[pos] {
				continue
			}
			total++
			flipped := goldenRaw[:pos] + string(c) + goldenRaw[pos+1:]
			k, err := Parse(flipped)
			if err != ErrShape || k.Plaintext() != "" { //nolint:errorlint // identity is the contract
				accepted++
				if accepted <= 5 {
					t.Errorf("flip at %d to %q: err = %v, non-zero Key = %v", pos, c, err, k.Plaintext() != "")
				}
			}
		}
	}
	if total != 5*62+49*61 {
		t.Fatalf("flip count = %d, want %d", total, 5*62+49*61)
	}
	if accepted != 0 {
		t.Fatalf("accepted %d of %d single-character flips", accepted, total)
	}
}

func TestParseRejectsAliasedChecksum(t *testing.T) {
	sumValue := func(s string) uint64 {
		var v uint64
		for i := 0; i < len(s); i++ {
			v = v*62 + uint64(strings.IndexByte(alphabet, s[i]))
		}
		return v
	}
	if a, g := sumValue(goldenAliasSum), sumValue(goldenSum); a != g+1<<32 || uint32(a) != uint32(g) {
		t.Fatalf("test vector broken: alias %d, sum %d", a, g)
	}
	if _, err := Parse(Prefix + goldenBody + goldenAliasSum); err != ErrShape { //nolint:errorlint // identity is the contract
		t.Fatalf("aliased checksum: err = %v, want ErrShape", err)
	}
}

func TestParseRejectsAdjacentTransposition(t *testing.T) {
	swaps := 0
	for i := len(Prefix); i < Len-1; i++ {
		if goldenRaw[i] == goldenRaw[i+1] {
			continue
		}
		swaps++
		s := goldenRaw[:i] + string(goldenRaw[i+1]) + string(goldenRaw[i]) + goldenRaw[i+2:]
		if _, err := Parse(s); err != ErrShape { //nolint:errorlint // identity is the contract
			t.Errorf("swap at %d: err = %v", i, err)
		}
	}
	if swaps == 0 {
		t.Fatal("no transposition tried")
	}
}

// One error form: every class of malformed input yields the identical
// sentinel with the identical text, and a zero Key.
func TestParseOneErrorForm(t *testing.T) {
	body2 := goldenBody[:10] + "é" + goldenBody[12:] // 2-byte rune, length stays 54 bytes
	cases := map[string]string{
		"empty":             "",
		"prefix only":       Prefix,
		"short":             goldenRaw[:Len-1],
		"long":              goldenRaw + "0",
		"trailing newline":  goldenRaw + "\n",
		"leading space":     " " + goldenRaw[:Len-1],
		"padded":            " " + goldenRaw + " ",
		"wrong prefix":      "ctxt_" + goldenBody + goldenSum,
		"upper prefix":      "CTXM_" + goldenBody + goldenSum,
		"no separator":      "ctxm0" + goldenBody + goldenSum,
		"char in body":      Prefix + goldenBody[:20] + "-" + goldenBody[21:] + goldenSum,
		"char in checksum":  Prefix + goldenBody + goldenSum[:3] + "_" + goldenSum[4:],
		"NUL":               Prefix + goldenBody[:20] + "\x00" + goldenBody[21:] + goldenSum,
		"non-ASCII":         Prefix + body2 + goldenSum,
		"invalid UTF-8":     Prefix + goldenBody[:20] + "\xff" + goldenBody[21:] + goldenSum,
		"checksum mismatch": Prefix + goldenBody + "0KG44E",
		"aliased checksum":  Prefix + goldenBody + goldenAliasSum,
		"knowledge key":     strings.Repeat("ab", 32),
		"access token":      "ctxt_" + strings.Repeat("a", 49),
	}
	if len(cases["non-ASCII"]) != Len {
		t.Fatalf("non-ASCII case is %d bytes, want %d", len(cases["non-ASCII"]), Len)
	}
	for name, raw := range cases {
		k, err := Parse(raw)
		if err != ErrShape { //nolint:errorlint // identity is the contract
			t.Errorf("%s: err = %v, want ErrShape itself", name, err)
			continue
		}
		if err.Error() != "modelkey: malformed key" {
			t.Errorf("%s: text = %q", name, err.Error())
		}
		if k.Plaintext() != "" || k.Hash() != "" || k.Hint() != "" {
			t.Errorf("%s: returned a non-zero Key", name)
		}
	}
}

// The cases above all carry a checksum that no longer matches, so the CRC
// comparison alone refuses them. Here every input carries the checksum
// recomputed over its own first 48 bytes: only the prefix check, the
// character check or the length check stands between it and a Key.
func TestParseRejectsShapeWithValidChecksum(t *testing.T) {
	at20 := func(c string) string { return goldenBody[:20] + c + goldenBody[21:] }
	cases := map[string]string{
		"access-token prefix": withSum("ctxt_" + goldenBody),
		"refresh prefix":      withSum("ctxr_" + goldenBody),
		"upper prefix":        withSum("CTXM_" + goldenBody),
		"no separator":        withSum("ctxm0" + goldenBody),
		"dash in body":        withSum(Prefix + at20("-")),
		"underscore in body":  withSum(Prefix + at20("_")),
		"space in body":       withSum(Prefix + at20(" ")),
		"NUL in body":         withSum(Prefix + at20("\x00")),
		"0xFF in body":        withSum(Prefix + at20("\xff")),
		"non-ASCII in body":   withSum(Prefix + goldenBody[:10] + "é" + goldenBody[12:]),
		"short body":          withSum(Prefix + goldenBody[:bodyLen-1]),
		"long body":           withSum(Prefix + goldenBody + "h"),
	}
	for name, raw := range cases {
		if k, err := Parse(raw); err != ErrShape || k.Plaintext() != "" { //nolint:errorlint // identity is the contract
			t.Errorf("%s: err = %v, non-zero Key = %v", name, err, k.Plaintext() != "")
		}
	}
}

// Every body position 5…47 × every byte outside [0-9A-Za-z], each with a
// recomputed checksum (43 × 194 = 8342 inputs): the refusal can only come
// from the character check, so a loop that skips its first or last body
// position is red here.
func TestParseRejectsInvalidCharAtEveryBodyPosition(t *testing.T) {
	total, accepted := 0, 0
	for pos := len(Prefix); pos < len(Prefix)+bodyLen; pos++ {
		for b := 0; b < 256; b++ {
			c := string([]byte{byte(b)})
			if keyShape.MatchString("ctxm_" + strings.Repeat("0", 48) + c) {
				continue
			}
			total++
			raw := withSum(Prefix + goldenBody[:pos-len(Prefix)] + c + goldenBody[pos-len(Prefix)+1:])
			if k, err := Parse(raw); err != ErrShape || k.Plaintext() != "" { //nolint:errorlint // identity is the contract
				accepted++
				if accepted <= 5 {
					t.Errorf("byte 0x%02x at %d with a valid checksum: err = %v", b, pos, err)
				}
			}
		}
	}
	if total != bodyLen*194 || accepted != 0 {
		t.Fatalf("accepted %d of %d invalid-character inputs (want 0 of %d)", accepted, total, bodyLen*194)
	}
}

func TestHasPrefix(t *testing.T) {
	for raw, want := range map[string]bool{
		goldenRaw:                true,
		"ctxm_":                  true,
		"ctxm_garbage":           true,
		" \tctxm_x\n":            true,
		"CTXM_x":                 false,
		"ctxt_x":                 false,
		"ctxr_x":                 false,
		"x ctxm_":                false,
		strings.Repeat("ab", 32): false,
		"":                       false,
	} {
		if got := HasPrefix(raw); got != want {
			t.Errorf("HasPrefix(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestGenerateRejectionSampling(t *testing.T) {
	r := bytes.NewReader(goldenEntropy())
	k, err := generate(r)
	if err != nil {
		t.Fatal(err)
	}
	if body := k.Plaintext()[len(Prefix) : len(Prefix)+bodyLen]; body != goldenBody {
		t.Fatalf("body = %q, want %q (bytes ≥ %d must be discarded, not reduced)", body, goldenBody, rejectFrom)
	}
	if r.Len() != 0 {
		t.Fatalf("%d entropy bytes left unread, want exactly the needed ones consumed", r.Len())
	}
	// A reader that runs dry fails the draw instead of returning a short key.
	short, err := generate(bytes.NewReader(goldenEntropy()[:40]))
	if err == nil || err == ErrShape || short.Plaintext() != "" { //nolint:errorlint // identity is the contract
		t.Fatalf("short reader: key %q, err %v; want a read error and a zero Key", short.Plaintext(), err)
	}
}

// 10^5 keys from crypto/rand: no duplicate, every key parses, and the body
// digits are uniform. Without rejection sampling the digits 0…7 come up
// 5/256 instead of 4/256 of the time, which puts chi² near 28 000; the bound
// 150 at 61 degrees of freedom is exceeded by a uniform source with p ≈ 3e-9.
func TestGenerateUniqueAndUniform(t *testing.T) {
	const n = 100000
	seen := make(map[string]struct{}, n)
	var counts [62]int
	for i := 0; i < n; i++ {
		k, err := Generate()
		if err != nil {
			t.Fatal(err)
		}
		raw := k.Plaintext()
		if _, dup := seen[raw]; dup {
			t.Fatalf("duplicate key after %d draws", i)
		}
		seen[raw] = struct{}{}
		if _, err := Parse(raw); err != nil {
			t.Fatalf("draw %d does not parse: %v", i, err)
		}
		for j := len(Prefix); j < len(Prefix)+bodyLen; j++ {
			counts[strings.IndexByte(alphabet, raw[j])]++
		}
	}
	expected := float64(n*bodyLen) / 62
	chi2 := 0.0
	for _, c := range counts {
		d := float64(c) - expected
		chi2 += d * d / expected
	}
	if chi2 > 150 {
		t.Fatalf("body digits not uniform: chi² = %.1f over 61 df (bound 150)", chi2)
	}
	t.Logf("chi² = %.1f over 61 df", chi2)
}

func TestZeroKey(t *testing.T) {
	var k Key
	if k.Plaintext() != "" || k.Hash() != "" || k.Hint() != "" || k.String() != "" || k.HashPrefix() != [8]byte{} {
		t.Fatal("zero Key must expose nothing")
	}
	if out := fmt.Sprintf("%v|%+v|%#v|%s|%d", k, k, k, k, k); strings.Contains(out, "PANIC") { //nolint:govet // %d on a Key is the point
		t.Fatalf("formatting a zero Key panicked: %s", out)
	}
}

// leakOutputs renders k through every formatting path a Key can plausibly
// take into a log line or a response body.
func leakOutputs(t *testing.T, k Key) map[string]string {
	t.Helper()
	type wrapUnexported struct{ key Key }
	type wrapExported struct{ Key Key }
	noTime := func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}
	logLine := func(json bool, attr slog.Attr) string {
		var buf bytes.Buffer
		var h slog.Handler = slog.NewTextHandler(&buf, &slog.HandlerOptions{ReplaceAttr: noTime})
		if json {
			h = slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: noTime})
		}
		slog.New(h).Info("auth", attr)
		return buf.String()
	}
	marshal := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			return "error: " + err.Error()
		}
		return string(b)
	}
	return map[string]string{
		"fmt %v|%+v|%#v|%s":     fmt.Sprintf("%v|%+v|%#v|%s", k, k, k, k),
		"fmt %d|%x|%q|%o":       fmt.Sprintf("%d|%x|%q|%o", k, k, k, k),                                                                    //nolint:govet // verbs a Stringer does not serve are the point
		"fmt nested unexported": fmt.Sprintf("%v|%+v|%#v|%d", wrapUnexported{k}, &wrapUnexported{k}, wrapUnexported{k}, wrapUnexported{k}), //nolint:govet // reflection path is the point
		"fmt nested exported":   fmt.Sprintf("%v|%+v|%#v", wrapExported{k}, wrapExported{k}, wrapExported{k}),
		"fmt slice/map":         fmt.Sprintf("%v|%+v", []Key{k}, map[string]Key{"k": k}),
		"error wrap":            fmt.Errorf("auth failed for %v", k).Error(),
		"slog text":             logLine(false, slog.Any("key", k)),
		"slog json":             logLine(true, slog.Any("key", k)),
		"slog text nested":      logLine(false, slog.Any("req", wrapUnexported{k})),
		"slog json nested":      logLine(true, slog.Any("req", wrapExported{k})),
		"json.Marshal":          marshal(k),
		"json.Marshal nested":   marshal(wrapExported{k}),
		"String/Hint":           k.String() + "|" + k.Hint(),
		"fmt.Fprintln":          fmtFprintln(k),
	}
}

// Leak gate (W01-2 (b)): no formatting path shows any 6-character window of
// the body, nor the full checksum — only the Hint's last four characters.
func TestKeyNeverPrintsBody(t *testing.T) {
	k := mustParse(t, goldenRaw)
	leaks := 0
	for path, out := range leakOutputs(t, k) {
		leaked := strings.Contains(out, goldenSum)
		for i := 0; i+6 <= len(goldenBody) && !leaked; i++ {
			leaked = strings.Contains(out, goldenBody[i:i+6])
		}
		if leaked {
			leaks++
			t.Errorf("%s leaks the key: %s", path, out)
		}
	}
	if leaks != 0 {
		t.Fatalf("%d formatting path(s) leak the key body", leaks)
	}
	if s := fmt.Sprintf("%v", k); s != goldenHint {
		t.Fatalf("%%v = %q, want the hint %q", s, goldenHint)
	}
	if b, _ := json.Marshal(k); string(b) != "{}" { //nolint:staticcheck // SA9005 states the contract: no exported field, no marshaler
		t.Fatalf("json.Marshal = %s, want {}", b)
	}
}

// The hint is what a Key shows wherever it is rendered on purpose — pinned
// exactly, so dropping GoString or LogValue (which would leave an address or
// {} instead, no leak) is still a red test.
func TestKeyRendersAsHint(t *testing.T) {
	k := mustParse(t, goldenRaw)
	out := leakOutputs(t, k)
	for path, want := range map[string]string{
		"fmt %v|%+v|%#v|%s": goldenHint + "|" + goldenHint + "|modelkey.Key(\"" + goldenHint + "\")|" + goldenHint,
		"slog text":         "level=INFO msg=auth key=" + goldenHint + "\n",
		"slog json":         `{"level":"INFO","msg":"auth","key":"` + goldenHint + `"}` + "\n",
		"String/Hint":       goldenHint + "|" + goldenHint,
		"error wrap":        "auth failed for " + goldenHint,
	} {
		if out[path] != want {
			t.Errorf("%s = %q, want %q", path, out[path], want)
		}
	}
}

func TestHashMatchesKnowledgeKeyProcedure(t *testing.T) {
	k := mustParse(t, goldenRaw)
	sum := sha256.Sum256([]byte(goldenRaw))
	if k.Hash() != hex.EncodeToString(sum[:]) || len(k.Hash()) != 64 || strings.ToLower(k.Hash()) != k.Hash() {
		t.Fatalf("Hash = %q, want lowercase hex(sha256(raw))", k.Hash())
	}
}

func FuzzParse(f *testing.F) {
	f.Add(goldenRaw, goldenEntropy())
	f.Add(Prefix+goldenBody+"0KG44E", []byte{})
	f.Add(Prefix+goldenBody+goldenAliasSum, bytes.Repeat([]byte{255}, 64))
	f.Add("", []byte{0})
	f.Add(Prefix, bytes.Repeat([]byte{61}, bodyLen))
	f.Add(strings.Repeat("ab", 32), bytes.Repeat([]byte{247}, bodyLen))
	f.Add(" "+goldenRaw, []byte("not enough"))
	f.Add(Prefix+goldenBody[:10]+"é"+goldenBody[12:]+goldenSum, goldenEntropy()[:bodyLen])
	f.Fuzz(func(t *testing.T, raw string, entropy []byte) {
		k, err := Parse(raw)
		if err != nil {
			if err != ErrShape || k.Plaintext() != "" { //nolint:errorlint // identity is the contract
				t.Fatalf("rejection must be ErrShape with a zero Key, got %v", err)
			}
		} else {
			if k.Plaintext() != raw || !HasPrefix(raw) || !keyShape.MatchString(raw) {
				t.Fatalf("accepted %q but round trip gives %q", raw, k.Plaintext())
			}
			again, err := Parse(k.Plaintext())
			if err != nil || again.Hash() != k.Hash() || again.Hint() != k.Hint() {
				t.Fatalf("re-parse of an accepted key failed: %v", err)
			}
		}
		g, err := generate(bytes.NewReader(entropy))
		if err != nil {
			return
		}
		if _, err := Parse(g.Plaintext()); err != nil {
			t.Fatalf("generated key does not parse: %v", err)
		}
	})
}

func fmtFprintln(k Key) string {
	var b bytes.Buffer
	_, _ = fmt.Fprintln(io.Writer(&b), k)
	return b.String()
}

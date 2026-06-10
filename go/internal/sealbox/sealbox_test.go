package sealbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testKeyHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

func newTestBox(t *testing.T) *Box {
	t.Helper()
	b, err := New(testKeyHex(t), "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// --- key parsing ---.

func TestNew_RejectsBadKeys(t *testing.T) {
	cases := map[string]struct{ current, prev string }{
		"empty current":     {"", ""},
		"non-hex current":   {strings.Repeat("zz", KeySize), ""},
		"short current":     {strings.Repeat("ab", KeySize-1), ""},
		"long current":      {strings.Repeat("ab", KeySize+1), ""},
		"aes-128 sized key": {strings.Repeat("ab", 16), ""},
		"bad prev":          {strings.Repeat("ab", KeySize), "not-hex"},
		"short prev":        {strings.Repeat("ab", KeySize), "abcd"},
	}
	for label, c := range cases {
		if _, err := New(c.current, c.prev); err == nil {
			t.Errorf("%s: New accepted invalid key material", label)
		}
	}
}

func TestNew_ErrorNeverEchoesKeyMaterial(t *testing.T) {
	// 31 bytes — wrong length, valid hex. The error must name the env var,
	// never the value.
	leaky := strings.Repeat("ab", KeySize-1)
	_, err := New(leaky, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), leaky) {
		t.Errorf("error echoes key material: %v", err)
	}
	if !strings.Contains(err.Error(), EnvKey) {
		t.Errorf("error should name %s: %v", EnvKey, err)
	}
}

func TestNew_AcceptsWhitespacePaddedKey(t *testing.T) {
	// .env values sometimes carry a trailing newline through docker exec -e.
	if _, err := New("  "+testKeyHex(t)+"\n", ""); err != nil {
		t.Errorf("whitespace-padded key rejected: %v", err)
	}
}

func TestFromEnv_RoundTrip(t *testing.T) {
	key := testKeyHex(t)
	t.Setenv(EnvKey, key)
	t.Setenv(EnvKeyPrev, "")
	b, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if b.HasPrev() {
		t.Error("HasPrev = true without prev key")
	}
	nonce, ct, err := b.Seal("env-probe", "_global", []byte("v"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := b.Open("env-probe", "_global", nonce, ct); err != nil {
		t.Errorf("open: %v", err)
	}
}

// --- seal/open core properties ---.

func TestSealOpen_RoundTrip(t *testing.T) {
	b := newTestBox(t)
	plaintext := []byte("sk-or-v1-" + strings.Repeat("0123456789abcdef", 4)) // 73 chars, realistic provider-key length; assembled so secret scanners see no key-shaped literal
	nonce, ct, err := b.Seal("openrouter-main", "_global", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if len(nonce) != 12 {
		t.Errorf("nonce length = %d, want 12", len(nonce))
	}
	if len(ct) != len(plaintext)+16 {
		t.Errorf("ct length = %d, want plaintext+16 (GCM tag)", len(ct))
	}

	got, usedPrev, err := b.Open("openrouter-main", "_global", nonce, ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if usedPrev {
		t.Error("usedPrev = true with current key")
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("roundtrip plaintext mismatch")
	}
}

func TestSeal_RejectsEmptyInputs(t *testing.T) {
	b := newTestBox(t)
	if _, _, err := b.Seal("", "_global", []byte("v")); err == nil {
		t.Error("empty name accepted")
	}
	if _, _, err := b.Seal("n", "", []byte("v")); err == nil {
		t.Error("empty scope accepted")
	}
	if _, _, err := b.Seal("n", "_global", nil); err == nil {
		t.Error("empty plaintext accepted")
	}
}

func TestOpen_TamperedCiphertextFails(t *testing.T) {
	b := newTestBox(t)
	nonce, ct, err := b.Seal("tamper-probe", "_global", []byte("attack at dawn"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, idx := range []int{0, len(ct) / 2, len(ct) - 1} { // body + auth tag
		mutated := bytes.Clone(ct)
		mutated[idx] ^= 0x01
		if _, _, err := b.Open("tamper-probe", "_global", nonce, mutated); err == nil {
			t.Errorf("tampered byte %d accepted", idx)
		}
	}
}

func TestOpen_TamperedNonceFails(t *testing.T) {
	b := newTestBox(t)
	nonce, ct, err := b.Seal("nonce-probe", "_global", []byte("v"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	mutated := bytes.Clone(nonce)
	mutated[0] ^= 0x01
	if _, _, err := b.Open("nonce-probe", "_global", mutated, ct); err == nil {
		t.Error("tampered nonce accepted")
	}
}

// AAD binding: a ciphertext copied onto another row (different name OR
// scope) must fail authentication — this is what makes context_secrets rows
// non-interchangeable.
func TestOpen_AADMismatchFails(t *testing.T) {
	b := newTestBox(t)
	nonce, ct, err := b.Seal("openrouter-main", "_global", []byte("v"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := b.Open("openrouter-stolen", "_global", nonce, ct); err == nil {
		t.Error("open with different name accepted — AAD does not bind name")
	}
	if _, _, err := b.Open("openrouter-main", "tenant-b", nonce, ct); err == nil {
		t.Error("open with different scope accepted — AAD does not bind scope")
	}
}

// Colon-bearing scopes must not be confusable with a (name, scope) pair that
// concatenates to the same AAD string. name cannot contain ':' (enforced by
// store.ValidSecretName), which keeps the layout unambiguous.
func TestOpen_ColonScopeAADUnambiguous(t *testing.T) {
	b := newTestBox(t)
	nonce, ct, err := b.Seal("key", "tenant:sub", []byte("v"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := b.Open("key", "tenant:sub", nonce, ct); err != nil {
		t.Errorf("open with colon scope: %v", err)
	}
	if _, _, err := b.Open("key", "tenant", nonce, ct); err == nil {
		t.Error("truncated scope accepted")
	}
}

func TestSeal_FreshNoncePerCall(t *testing.T) {
	b := newTestBox(t)
	plaintext := []byte("same plaintext twice")
	n1, c1, err := b.Seal("fresh", "_global", plaintext)
	if err != nil {
		t.Fatalf("seal 1: %v", err)
	}
	n2, c2, err := b.Seal("fresh", "_global", plaintext)
	if err != nil {
		t.Fatalf("seal 2: %v", err)
	}
	if bytes.Equal(n1, n2) {
		t.Error("nonce reuse across two Seal calls — catastrophic for GCM")
	}
	if bytes.Equal(c1, c2) {
		t.Error("identical ciphertexts for identical plaintexts")
	}
}

func TestOpen_ErrorCarriesNoSensitiveMaterial(t *testing.T) {
	b := newTestBox(t)
	plaintext := []byte("PLAINTEXTMARKER-do-not-leak")
	nonce, ct, err := b.Seal("leak-probe", "_global", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	mutated := bytes.Clone(ct)
	mutated[0] ^= 0x01
	_, _, err = b.Open("leak-probe", "_global", nonce, mutated)
	if err == nil {
		t.Fatal("expected auth failure")
	}
	msg := err.Error()
	if strings.Contains(msg, "PLAINTEXTMARKER") {
		t.Errorf("error leaks plaintext: %s", msg)
	}
	if strings.Contains(msg, hex.EncodeToString(mutated)) || strings.Contains(msg, base64.StdEncoding.EncodeToString(mutated)) {
		t.Errorf("error leaks ciphertext: %s", msg)
	}
}

// --- master-key rotation (prev-key slot) ---.

func TestOpen_PrevKeyFallback(t *testing.T) {
	oldHex, newHex := testKeyHex(t), testKeyHex(t)
	oldBox, err := New(oldHex, "")
	if err != nil {
		t.Fatalf("old box: %v", err)
	}
	plaintext := []byte("sealed under the OLD master key")
	nonce, ct, err := oldBox.Seal("rotated", "_global", plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// Rotation in flight: new current, old in the prev slot.
	rotated, err := New(newHex, oldHex)
	if err != nil {
		t.Fatalf("rotated box: %v", err)
	}
	if !rotated.HasPrev() {
		t.Fatal("HasPrev = false with prev key set")
	}
	got, usedPrev, err := rotated.Open("rotated", "_global", nonce, ct)
	if err != nil {
		t.Fatalf("open via prev: %v", err)
	}
	if !usedPrev {
		t.Error("usedPrev = false for a prev-key open — boot sweep would never re-seal")
	}
	if !bytes.Equal(got, plaintext) {
		t.Error("prev-key plaintext mismatch")
	}

	// Re-seal sweep semantics: seal with current, then a box WITHOUT the old
	// key (rotation finished, CTX_SECRETS_KEY_PREV removed) opens it.
	n2, c2, err := rotated.Seal("rotated", "_global", got)
	if err != nil {
		t.Fatalf("re-seal: %v", err)
	}
	finalBox, err := New(newHex, "")
	if err != nil {
		t.Fatalf("final box: %v", err)
	}
	got2, usedPrev2, err := finalBox.Open("rotated", "_global", n2, c2)
	if err != nil {
		t.Fatalf("open after sweep: %v", err)
	}
	if usedPrev2 {
		t.Error("usedPrev = true after re-seal with current")
	}
	if !bytes.Equal(got2, plaintext) {
		t.Error("post-sweep plaintext mismatch")
	}

	// The ORIGINAL old-key ciphertext is unreadable once prev is dropped —
	// exactly the "key loss = total loss by design" property.
	if _, _, err := finalBox.Open("rotated", "_global", nonce, ct); err == nil {
		t.Error("old ciphertext readable without old key")
	}
}

func TestOpen_NoPrevNoFallback(t *testing.T) {
	a, b := newTestBox(t), newTestBox(t)
	nonce, ct, err := a.Seal("foreign", "_global", []byte("v"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, _, err := b.Open("foreign", "_global", nonce, ct); err == nil {
		t.Error("foreign-key ciphertext opened without matching key")
	}
}

// --- break-glass stdin format ---.

func TestDecodeLine_RoundTrip(t *testing.T) {
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	ct := bytes.Repeat([]byte{0xAB}, 89) // realistic provider-key ct length
	line := base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(ct) + ":openrouter-main:_global\n"

	gotNonce, gotCt, name, scope, err := DecodeLine([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(gotNonce, nonce) || !bytes.Equal(gotCt, ct) {
		t.Error("nonce/ct mismatch")
	}
	if name != "openrouter-main" || scope != "_global" {
		t.Errorf("name/scope = %q/%q", name, scope)
	}
}

// PG's encode(bytea,'base64') is MIME (RFC 2045): a line break every 76
// chars. DecodeLine must strip CR/LF from the FULL input — this is the exact
// shape `psql -At` emits for any realistic provider key.
func TestDecodeLine_MIMEWrappedBase64(t *testing.T) {
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	ct := bytes.Repeat([]byte{0xCD}, 89) // b64 = 120 chars -> wraps at 76
	b64 := base64.StdEncoding.EncodeToString(ct)
	if len(b64) <= 76 {
		t.Fatalf("test ct too short to trigger MIME wrap: b64 len %d", len(b64))
	}
	mimeB64 := b64[:76] + "\n" + b64[76:] // what PG's encode() produces
	line := base64.StdEncoding.EncodeToString(nonce) + ":" + mimeB64 + ":k:s\n"

	_, gotCt, name, scope, err := DecodeLine([]byte(line))
	if err != nil {
		t.Fatalf("decode MIME-wrapped input: %v", err)
	}
	if !bytes.Equal(gotCt, ct) {
		t.Error("ct mismatch after MIME unwrap")
	}
	if name != "k" || scope != "s" {
		t.Errorf("name/scope = %q/%q", name, scope)
	}
}

func TestDecodeLine_CRLFStripped(t *testing.T) {
	nonce := []byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
	ct := []byte("ctbytes")
	line := base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(ct) + ":n:s\r\n"
	if _, _, _, _, err := DecodeLine([]byte(line)); err != nil {
		t.Errorf("CRLF input rejected: %v", err)
	}
}

// Scopes may carry colons ('tenant:crag-research-q1'); SplitN(…, 4) must
// keep them in the scope field.
func TestDecodeLine_ColonScopePreserved(t *testing.T) {
	line := base64.StdEncoding.EncodeToString([]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}) + ":" +
		base64.StdEncoding.EncodeToString([]byte("x")) + ":key-name:tenant:crag-research-q1"
	_, _, name, scope, err := DecodeLine([]byte(line))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if name != "key-name" {
		t.Errorf("name = %q", name)
	}
	if scope != "tenant:crag-research-q1" {
		t.Errorf("scope = %q, colons must stay in scope", scope)
	}
}

func TestDecodeLine_Malformed(t *testing.T) {
	for label, input := range map[string]string{
		"empty":          "",
		"whitespace":     "  \n",
		"too few fields": "AAAA:BBBB:name",
		"empty field":    "AAAA::name:scope",
		"bad nonce b64":  "@@@@:BBBB:name:scope",
		"bad ct b64":     "AAAA:@@@@:name:scope",
	} {
		if _, _, _, _, err := DecodeLine([]byte(input)); err == nil {
			t.Errorf("%s: accepted %q", label, input)
		}
	}
}

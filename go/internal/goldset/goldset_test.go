package goldset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Gate (b): redaction sweep. A query carrying a credential is DISCARDED,
// never carried on redacted (design 04 §4.5).

func TestScanQueryDiscardsCredentials(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, query, wantKind string
	}{
		{"bearer", "wie war das mit Bearer sk_live_AbCdEf0123456789xyz nochmal", "bearer-token"},
		{"bearer-header", "authorization: Bearer abcdefghijklmnop0123456789", "bearer-token"},
		{"jwt", "warum failt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "jwt"},
		{"pem", "was macht -----BEGIN RSA PRIVATE KEY----- im repo", "pem-private-key"},
		{"aws", "ist AKIAIOSFODNN7EXAMPLE noch gültig", "aws-key"},
		{"sk-token", "openai key sk-abcdefghijklmnopqrstuvwxyz012345 rotieren", "token-prefix"},
		{"assignment", "warum steht password=Xj4!kQ92zLpR im compose file", "secret-assignment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, hit := ScanQuery(tc.query)
			if !hit {
				t.Fatalf("credential query passed the sweep: kind=%q", m.Kind)
			}
			if m.Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", m.Kind, tc.wantKind)
			}
			if strings.Contains(m.Reason, "Bearer sk_live") || strings.Contains(m.Reason, "AKIA") {
				t.Errorf("reason echoes the secret: %q", m.Reason)
			}
		})
	}
}

// The sweep must not eat ordinary queries — a slice emptied by false positives
// would lose exactly the external validity it exists for.
func TestScanQueryKeepsOrdinaryQueries(t *testing.T) {
	t.Parallel()
	for _, q := range []string{
		"wie funktioniert das rrf ranking in ctx",
		"which migration introduced ctx_rrf_arms?",
		"password rotation policy für die deploy-kette",
		"sha256: 3f786850e387550fdab836ed7e6dc881de23001b7d97ff9f9f0e7b0a5f3c1e2d",
	} {
		if m, hit := ScanQuery(q); hit {
			t.Errorf("ordinary query discarded as %q: %q", m.Kind, q)
		}
	}
}

// A credential smuggled into the drawn pool must not reach the slice file.
func TestRedactionSweepDropsInjectedCase(t *testing.T) {
	t.Parallel()
	pool := []string{
		"wie ist der dream-cooldown definiert",
		"debug: Authorization: Bearer ya29.A0ARrdaM9qTESTTOKEN0123456789abcdef",
		"welche gewichte hat ctx_rrf heute",
	}
	var kept []Case
	discarded := 0
	for _, q := range pool {
		if _, hit := ScanQuery(q); hit {
			discarded++
			continue
		}
		kept = append(kept, Case{Slice: SliceReal, Query: q, Origin: "access-log"})
	}
	if discarded != 1 {
		t.Fatalf("discarded = %d, want 1", discarded)
	}
	if len(kept) != 2 {
		t.Fatalf("kept = %d, want 2", len(kept))
	}
	for _, c := range kept {
		if strings.Contains(strings.ToLower(c.Query), "bearer") {
			t.Fatal("a redacted remnant reached the slice — the case must be dropped whole")
		}
	}
}

// --- Gate (c): split determinism.

func TestSplitIsDeterministicInSeed(t *testing.T) {
	t.Parallel()
	keys := make([]string, 200)
	for i := range keys {
		keys[i] = string(rune('a'+i%26)) + "-" + itoa(i)
	}
	a := Split(keys, 20260825)
	b := Split(keys, 20260825)
	if SplitFingerprint(a) != SplitFingerprint(b) {
		t.Fatal("same seed produced a different DERIV/HOLD partition")
	}
	// Input order must not matter — the sample is drawn by a hash order that is
	// not the split order.
	shuffled := append([]string(nil), keys...)
	for i := range shuffled {
		j := (i * 7) % len(shuffled)
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	if SplitFingerprint(Split(shuffled, 20260825)) != SplitFingerprint(a) {
		t.Fatal("partition depends on input order")
	}
	if SplitFingerprint(Split(keys, 20260826)) == SplitFingerprint(a) {
		t.Fatal("a different seed produced the same partition")
	}
	deriv, hold := SplitCounts(a)
	if deriv != 100 || hold != 100 {
		t.Errorf("counts = %d/%d, want 100/100", deriv, hold)
	}
}

func TestSplitOddCountPadsDeriv(t *testing.T) {
	t.Parallel()
	deriv, hold := SplitCounts(Split([]string{"a", "b", "c"}, 1))
	if deriv != 2 || hold != 1 {
		t.Errorf("counts = %d/%d, want 2/1 (HOLD is never the padded half)", deriv, hold)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// --- Gate (d): provenance. An entry without an on-prem endpoint aborts.

func TestRequireOnPremRejectsExternal(t *testing.T) {
	t.Parallel()
	external := []Backend{
		{Name: "openrouter", BaseURL: "https://openrouter.ai/api", Locality: "external", Trust: "no-credentials"},
		{Name: "lonius-embed", BaseURL: "https://llm.thelonius.it:4000", Locality: "external", Trust: "full-trust"},
		// Mislabelled row: the locality column claims lan, the host is public.
		// The column is editable state, not a proof — the host check must fire.
		{Name: "fake-lan", BaseURL: "https://api.example.com/v1", Locality: "lan", Trust: "full-trust"},
	}
	for _, b := range external {
		t.Run(b.Name, func(t *testing.T) {
			t.Parallel()
			err := RequireOnPrem(b)
			if err == nil {
				t.Fatalf("external backend %q accepted as generator", b.Name)
			}
			if _, cErr := NewChatClient(b, "m", 0); cErr == nil {
				t.Fatalf("chat client built for external backend %q", b.Name)
			}
		})
	}
}

func TestRequireOnPremAcceptsLocal(t *testing.T) {
	t.Parallel()
	for _, b := range []Backend{
		{Name: "spark-chat", BaseURL: "http://10.13.37.22:30000", Locality: "lan"},
		{Name: "llama-cpu", BaseURL: "http://llama-cpu:8090", Locality: "lan"},
		{Name: "loopback", BaseURL: "http://127.0.0.1:11434", Locality: "local"},
	} {
		if err := RequireOnPrem(b); err != nil {
			t.Errorf("on-prem backend %q rejected: %v", b.Name, err)
		}
	}
}

func TestChatClientCarriesBackendExtraBody(t *testing.T) {
	t.Parallel()
	// enable_thinking=false is mandatory for qwen38 on SGLang; it lives in the
	// backend row and must survive into the client.
	b := Backend{
		Name: "spark-chat", BaseURL: "http://10.13.37.22:30000", Locality: "lan",
		ModelMap:  json.RawMessage(`{"default":{"model":"qwen38-27b"}}`),
		ExtraBody: json.RawMessage(`{"chat_template_kwargs":{"enable_thinking":false}}`),
	}
	c, err := NewChatClient(b, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "qwen38-27b" {
		t.Errorf("model = %q, want qwen38-27b", c.Model)
	}
	if _, ok := c.ExtraBody["chat_template_kwargs"]; !ok {
		t.Error("extra_body from the backend row was dropped")
	}
}

// PromptSHA256 is the stamped provenance of the frozen prompt: it must be
// stable and must change when the prompt does.
func TestPromptHashStable(t *testing.T) {
	t.Parallel()
	if got := PromptSHA256(); got != SHA256Hex(GQSystem()+"\n\n"+gqUserTemplate) {
		t.Fatalf("prompt hash does not cover the frozen prompt pair: %s", got)
	}
}

// --- Path guard (§3.3).

func TestGuardConfinesWrites(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), DirName)
	g, err := NewGuard(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Resolve(FileKI); err != nil {
		t.Fatalf("in-directory write refused: %v", err)
	}
	for _, bad := range []string{"/tmp/g-ki.jsonl", "../escape.jsonl", "sub/../../escape.jsonl"} {
		if p, err := g.Resolve(bad); err == nil {
			t.Errorf("guard permitted %q -> %q", bad, p)
		}
	}
	// The override is the only way out, and callers stamp it.
	go2, err := NewGuard(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := go2.Resolve("/tmp/g-ki.jsonl"); err != nil {
		t.Errorf("override did not permit the outside write: %v", err)
	}
	if !go2.AllowOutside() {
		t.Error("override not visible for the stamp")
	}
}

func TestGuardRejectsWrongRootName(t *testing.T) {
	t.Parallel()
	if _, err := NewGuard(filepath.Join(t.TempDir(), "goldset-typo"), false); err == nil {
		t.Fatal("a root that is not the mandated directory was accepted")
	}
}

func TestGuardRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, DirName)
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	g, err := NewGuard(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if p, err := g.Resolve("link/g-ki.jsonl"); err == nil {
		t.Errorf("symlink escape permitted -> %q", p)
	}
}

// --- Slice files and stamp round-trip.

func TestWriteJSONLIsOwnerOnlyAndIndexed(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), DirName)
	g, err := NewGuard(root, false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := g.Resolve(FileReal)
	if err != nil {
		t.Fatal(err)
	}
	in := []Case{{Slice: SliceReal, Query: "alpha"}, {Slice: SliceReal, Query: "beta"}}
	if err := WriteJSONL(p, in); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — the slice carries private query texts", fi.Mode().Perm())
	}
	out, err := ReadJSONL(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[1].Index != 1 {
		t.Fatalf("round-trip lost data: %+v", out)
	}
	if out[0].QuerySHA256 != SHA256Hex("alpha") {
		t.Error("query hash not assigned — reports could not cite the case without the text")
	}
	// G-REAL must carry no labels at stage 1.
	if len(out[0].GoldIDs) != 0 {
		t.Error("G-REAL case carries labels; those belong to wave B-W6")
	}
}

func TestStampRoundTrip(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), FileStamp)
	empty, err := ReadStamp(p)
	if err != nil || empty.Slices == nil {
		t.Fatalf("missing stamp not handled: %v", err)
	}
	in := Stamp{Version: 1, CorpusMaxCreatedAt: "2026-08-25T13:49:49Z", SplitSeed: 7,
		Generator: &Generator{Backend: "spark-chat", Model: "qwen38-27b",
			Endpoint: "http://10.13.37.22:30000/v1/chat/completions", Locality: "lan"},
		Slices: map[string]SliceStamp{SliceQ: {N: 200, File: FileQ}}}
	if err := WriteStamp(p, in); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStamp(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Generator == nil || got.Generator.Model != "qwen38-27b" || got.Generator.Locality != "lan" {
		t.Fatalf("generator provenance lost: %+v", got.Generator)
	}
	if names := SliceNames(got); len(names) != 1 || names[0] != SliceQ {
		t.Errorf("slice names = %v", names)
	}
}

// --- G-KI paraphrase.

func TestParaphraseTitleIsDeterministicAndNotVerbatim(t *testing.T) {
	t.Parallel()
	titles := []string{
		"ctx Multi-Tenant — Modell C: Scope als Daten-Diskriminator",
		"Dream v3 Confidence-Kalibrierung",
		"RRF weights and the trigram arm",
		"⚠️ Deploy-Doktrin: nie up -d ohne Freigabe",
	}
	for _, title := range titles {
		a := ParaphraseTitle(title, 20260812)
		b := ParaphraseTitle(title, 20260812)
		if a != b {
			t.Fatalf("paraphrase not deterministic: %q vs %q", a, b)
		}
		if a == "" || len(strings.Fields(a)) < 2 {
			t.Fatalf("paraphrase degenerate for %q: %q", title, a)
		}
		if a == title {
			t.Errorf("paraphrase is the title verbatim: %q", a)
		}
	}
}

func TestParaphraseBalancesBrackets(t *testing.T) {
	t.Parallel()
	// Segment reordering cuts through the parenthetical; the query must not
	// carry a bracket the title never had unmatched.
	got := ParaphraseTitle("Vorhaben-Kandidat E — Inference-Scheduler für ctxd (Prioritäts-Queue vor den Backends)", 20260812)
	if strings.Count(got, "(") != strings.Count(got, ")") {
		t.Errorf("unbalanced brackets in %q", got)
	}
	if b := balanceBrackets("a ) b ( c ] d"); strings.ContainsAny(b, "()[]{}") {
		t.Errorf("stray brackets survived: %q", b)
	}
	if b := balanceBrackets("keep (this) intact"); b != "keep (this) intact" {
		t.Errorf("balanced brackets were altered: %q", b)
	}
}

func TestParaphraseKeepsIdentifiers(t *testing.T) {
	t.Parallel()
	got := ParaphraseTitle("Migration 135 baut ctx_rrf_arms für SGLang", 1)
	for _, want := range []string{"135", "ctx_rrf_arms", "SGLang"} {
		if !strings.Contains(got, want) {
			t.Errorf("identifier %q lost in paraphrase %q", want, got)
		}
	}
}

// --- G-Q shape filter.

func TestAcceptQuestionRejectsTitleRestatement(t *testing.T) {
	t.Parallel()
	title := "Dream v3 Confidence-Kalibrierung"
	if _, ok := AcceptQuestion("Was besagt die Dream v3 Confidence-Kalibrierung?", title); ok {
		t.Error("a question restating the title was accepted — that is a G-KI case in disguise")
	}
	if _, ok := AcceptQuestion("Ab welchem Confidence-Wert liegt die Trefferquote bei 100 Prozent?", title); !ok {
		t.Error("a valid content question was rejected")
	}
	for _, bad := range []string{"", "Warum?", "Erste Frage? Zweite Frage?", "Das ist keine Frage."} {
		if _, ok := AcceptQuestion(bad, title); ok {
			t.Errorf("malformed question accepted: %q", bad)
		}
	}
}

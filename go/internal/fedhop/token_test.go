package fedhop

// Gates of F-W3 on the hop token (design/04 §4.1 "Hop-Token (normativ)"):
// token = hex(nonce ‖ HMAC-SHA256(key, nonce)[:8]), 8 + 8 bytes, 32 lower-case
// hex characters; IsOwnToken recomputes the MAC and compares with hmac.Equal.
//
//   - TestMintTokenIsOwnToken: IsOwnToken(MintToken()) for the minting
//     identity, grammar of the token.
//   - TestTokenOfForeignIdentityIsNotOwn: another key — even under the same
//     id — does not recognise it.
//   - TestMintTokenUnique: 10⁵ tokens of one identity, no duplicate.
//   - TestTamperedTokenIsNotOwn: every changed character (nonce or MAC), an
//     upper-case spelling and every grammar break ⇒ false.
//   - TestZeroIdentityHasNoToken: the zero Identity (and an empty key) mints
//     nothing and recognises nothing, without a panic.
//   - TestIsOwnTokenComparesInConstantTime: source pin — the MAC comparison
//     is hmac.Equal and nothing else, the nonce comes from crypto/rand.
//
//	go test -count=1 -run 'Token' ./internal/fedhop/

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestMintTokenIsOwnToken(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	for range 100 {
		tk := self.MintToken()
		if !validToken(tk) || len(tk) != 32 || strings.ToLower(tk) != tk {
			t.Fatalf("MintToken = %q, want 32 lower-case hex characters", tk)
		}
		if !self.IsOwnToken(tk) {
			t.Fatalf("IsOwnToken(MintToken()) = false for %q", tk)
		}
	}
}

func TestTokenOfForeignIdentityIsNotOwn(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	otherKey := bytes.Clone(testSecret)
	otherKey[31] ^= 0x01
	for _, other := range []Identity{
		NewIdentity(testID, otherKey), // same id, one key bit different
		NewIdentity(uid(2), bytes.Repeat([]byte{7}, 32)),
	} {
		for range 1000 {
			if tk := other.MintToken(); self.IsOwnToken(tk) {
				t.Fatalf("token %q of another key recognised as own", tk)
			}
			if tk := self.MintToken(); other.IsOwnToken(tk) {
				t.Fatalf("another key recognises own token %q", tk)
			}
		}
	}
}

func TestMintTokenUnique(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	const n = 100_000
	seen := make(map[string]struct{}, n)
	nonces := make(map[string]struct{}, n)
	for range n {
		tk := self.MintToken()
		if _, dup := seen[tk]; dup {
			t.Fatalf("duplicate token %q after %d", tk, len(seen))
		}
		seen[tk] = struct{}{}
		nonces[tk[:16]] = struct{}{}
	}
	if len(nonces) != n {
		t.Fatalf("%d distinct nonces in %d tokens", len(nonces), n)
	}
}

// TestMintTokenNonceSpread: every one of the 8 nonce bytes is random. Over
// 1000 tokens a uniform byte takes ≈ 251 of 256 values (fewer than 200 has a
// probability far below 10⁻³⁰); a shorter nonce leaves the rest constant,
// which no uniqueness test of 10⁵ tokens would notice at 40 bits.
func TestMintTokenNonceSpread(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	var seen [nonceLen]map[string]bool
	for i := range seen {
		seen[i] = map[string]bool{}
	}
	for range 1000 {
		tk := self.MintToken()
		for i := range nonceLen {
			seen[i][tk[2*i:2*i+2]] = true
		}
	}
	for i := range seen {
		if len(seen[i]) < 200 {
			t.Errorf("nonce byte %d took %d distinct values in 1000 tokens, want ≥ 200", i, len(seen[i]))
		}
	}
	if nonceLen != 8 || macLen != 8 {
		t.Errorf("nonce %d / MAC %d bytes, want 8 + 8 (design/04 §4.1)", nonceLen, macLen)
	}
}

func TestTamperedTokenIsNotOwn(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	tk := self.MintToken()
	for pos := range len(tk) {
		for _, c := range "0123456789abcdef" {
			if byte(c) == tk[pos] {
				continue
			}
			changed := tk[:pos] + string(c) + tk[pos+1:]
			if self.IsOwnToken(changed) {
				part := "MAC"
				if pos < 16 {
					part = "nonce"
				}
				t.Fatalf("token with %s character %d changed to %q still own", part, pos, c)
			}
		}
	}
	for _, bad := range []string{
		strings.ToUpper(tk), tk[:31], tk + "0", tk[:31] + "g", " " + tk, tk + " ", "", tk[:16],
		tk[:16] + strings.Repeat("0", 16), // the nonce with a zero MAC
	} {
		if self.IsOwnToken(bad) {
			t.Errorf("IsOwnToken(%q) = true", bad)
		}
	}
	// Upper case decodes to the same bytes, but it is not the grammar.
	if strings.ToUpper(tk) != tk && self.IsOwnToken(strings.ToUpper(tk)) {
		t.Error("upper-case spelling of the own token recognised")
	}
}

func TestZeroIdentityHasNoToken(t *testing.T) {
	if tk := (Identity{}).MintToken(); tk != "" {
		t.Errorf("zero Identity minted %q", tk)
	}
	if (Identity{}).IsOwnToken(NewIdentity(testID, testSecret).MintToken()) {
		t.Error("zero Identity recognises a token")
	}
	empty := NewIdentity(testID, nil)
	if tk := empty.MintToken(); tk != "" {
		t.Errorf("identity without key minted %q", tk)
	}
	if empty.IsOwnToken(strings.Repeat("0", 32)) {
		t.Error("identity without key recognises a token")
	}
}

// TestIsOwnTokenComparesInConstantTime pins the source of token.go: the
// only comparison of MAC bytes is hmac.Equal (no bytes.Equal, no string or
// byte ==, no subtle shortcut of another name), and the nonce is read from
// crypto/rand. A timing side channel cannot be measured reliably in a unit
// test; its absence is a property of the code, so the code is pinned.
func TestIsOwnTokenComparesInConstantTime(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "token.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	imports := map[string]bool{}
	for _, im := range file.Imports {
		p, _ := strconv.Unquote(im.Path.Value)
		imports[p] = true
	}
	if !imports["crypto/rand"] || !imports["crypto/hmac"] || imports["math/rand"] || imports["math/rand/v2"] {
		t.Errorf("token.go imports %v: want crypto/rand and crypto/hmac, never math/rand", imports)
	}
	var hmacEqual, randRead int
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					switch id.Name + "." + sel.Sel.Name {
					case "hmac.Equal":
						hmacEqual++
					case "rand.Read":
						randRead++
					case "bytes.Equal", "subtle.ConstantTimeCompare", "reflect.DeepEqual", "slices.Equal":
						t.Errorf("%s: %s.%s — the MAC comparison is hmac.Equal only", fset.Position(x.Pos()), id.Name, sel.Sel.Name)
					}
				}
			}
		case *ast.BinaryExpr:
			if x.Op == token.EQL || x.Op == token.NEQ {
				if isMACOperand(x.X) || isMACOperand(x.Y) {
					t.Errorf("%s: == / != on MAC bytes", fset.Position(x.Pos()))
				}
			}
		}
		return true
	})
	if hmacEqual != 1 || randRead != 1 {
		t.Errorf("token.go: hmac.Equal ×%d, rand.Read ×%d — want exactly one each", hmacEqual, randRead)
	}
}

// isMACOperand: an operand that is (a slice or conversion of) raw or MAC bytes.
func isMACOperand(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SliceExpr, *ast.IndexExpr:
		return true
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "string" {
			return true
		}
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Sum" {
			return true
		}
	}
	return false
}

package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// TestConsumerAuthNeverPrintsCredential pins the secret freedom of
// ConsumerAuth (design/01 §4.4, B15): the hash ReauthenticateModelKey needs
// rides in an unexported closure, so fmt prints an address instead of the
// hash under every verb — also %+v, %#v and the bad-verb path (%s, %d),
// which format unexported fields by reflection and dereference a pointer to
// a struct there — encoding/json skips it, and slog (text and JSON) records
// no hash.
func TestConsumerAuthNeverPrintsCredential(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	exp := time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC)
	ca := &ConsumerAuth{
		ModelKeyID: "11111111-1111-1111-1111-111111111111", ConsumerID: "22222222-2222-2222-2222-222222222222",
		TenantID: "33333333-3333-3333-3333-333333333333", TenantScope: "work", Kind: KindCtxPeer,
		AllModels: true, PromptSensitivity: "internal", FairWeight: 7, KeyExpiresAt: &exp, IsValid: true,
		AdmissionLane: "urgent", PeerInstanceID: "44444444-4444-4444-4444-444444444444",
		cred: credentialOf(hash, netip.MustParseAddr("10.1.2.3"), true),
	}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		outs = append(outs, fmt.Sprintf(verb, ca), fmt.Sprintf(verb, *ca))
	}
	js, err := json.Marshal(ca)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	outs = append(outs, string(js))
	for _, mk := range []func(*bytes.Buffer) slog.Handler{
		func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
		func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
	} {
		var buf bytes.Buffer
		slog.New(mk(&buf)).Info("probe", "ca", ca, "value", *ca)
		outs = append(outs, buf.String())
	}
	// The hash in any spelling a formatter could pick: as is, upper case, and
	// the hex of its bytes (%x of a string).
	needles := []string{hash, strings.ToUpper(hash), fmt.Sprintf("%x", hash)}
	for _, out := range outs {
		for _, n := range needles {
			if strings.Contains(out, n) {
				t.Fatalf("credential hash leaked in %q", out)
			}
		}
	}
	if !strings.Contains(string(js), `"ConsumerID":"22222222-2222-2222-2222-222222222222"`) {
		t.Errorf("json lost the exported fields: %s", js)
	}
}

// TestConsumerKindMapping pins §4.4: every unknown kind is an app.
func TestConsumerKindMapping(t *testing.T) {
	for in, want := range map[string]ConsumerKind{
		"app": KindApp, "ctx-peer": KindCtxPeer, "": KindApp, "CTX-PEER": KindApp, "peer": KindApp,
	} {
		if got := consumerKind(in); got != want {
			t.Errorf("consumerKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestModelAuthAddrNormalisation pins what reaches ctx_model_auth's INET
// parameter: no zone (inet has none), an IPv4-mapped IPv6 address as IPv4
// (inet '::ffff:10.1.2.3' is not inside cidr '10.0.0.0/8'), an invalid
// address as SQL NULL.
func TestModelAuthAddrNormalisation(t *testing.T) {
	cases := []struct {
		in   netip.Addr
		want any
	}{
		{netip.MustParseAddr("10.1.2.3"), netip.MustParseAddr("10.1.2.3")},
		{netip.MustParseAddr("::ffff:10.1.2.3"), netip.MustParseAddr("10.1.2.3")},
		{netip.MustParseAddr("fe80::1%eth0"), netip.MustParseAddr("fe80::1")},
		{netip.Addr{}, nil},
	}
	for _, c := range cases {
		if got := addrArg(c.in); got != c.want {
			t.Errorf("addrArg(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

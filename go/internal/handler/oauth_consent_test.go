package handler

import (
	"net/url"
	"strings"
	"testing"
	"unicode"

	"github.com/GottZ/ctx/internal/store"
)

// consentSelfRegMarker is the opening sentence of the self-registration
// warning on the consent page (the integration suite pins the full text as
// k7Warning; that file is build-tagged, so the DB-less probes carry their own).
const consentSelfRegMarker = "This client registered itself."

// renderConsentPage renders the consent template exactly as writeConsent
// does, without a DB: newConsentView over a hand-built client row.
func renderConsentPage(t *testing.T, client *store.OAuthClient, redirect string) string {
	t.Helper()
	u, err := url.Parse(redirect)
	if err != nil {
		t.Fatalf("parse redirect %q: %v", redirect, err)
	}
	var sb strings.Builder
	if err := consentTmpl.Execute(&sb, newConsentView(client, u, authorizeParams{RedirectURI: redirect}, "")); err != nil {
		t.Fatalf("render consent page: %v", err)
	}
	return sb.String()
}

// TestConsentSelfRegWarning_RequestedRedirect pins §F-§6 (variant B): the
// self-registration warning keys on the redirect THIS request asks for. A
// self-registered client returning to an operator-vetted callback (the
// static allowlist or CTX_OAUTH_REDIRECT_EXTRA, exact match) shows no
// warning; every other target keeps it — including loopback, which the
// runtime allowlist admits by rule but which the user cannot verify.
func TestConsentSelfRegWarning_RequestedRedirect(t *testing.T) {
	const extraCB = "https://connector.example/oauth/callback"
	t.Setenv(EnvRedirectExtra, " https://other.example/cb , "+extraCB+" ")

	selfReg := &store.OAuthClient{ClientID: "ctx_dcr_open", Label: "Some Connector", RegistrationSource: "dcr"}
	adminKey := "key-1"
	adminDCR := &store.OAuthClient{ClientID: "ctx_dcr_admin", Label: "Operator DCR", RegistrationSource: "dcr", CreatedBy: &adminKey}
	operator := &store.OAuthClient{ClientID: "ctx_admin", Label: "Operator", RegistrationSource: "admin"}

	for _, tc := range []struct {
		name     string
		client   *store.OAuthClient
		redirect string
		wantWarn bool
	}{
		{"SelfRegClaudeCallback", selfReg, "https://claude.ai/api/mcp/auth_callback", false},
		{"SelfRegClaudeComCallback", selfReg, "https://claude.com/api/mcp/auth_callback", false},
		{"SelfRegChatGPTCallback", selfReg, "https://chatgpt.com/connector_platform_oauth_redirect", false},
		{"SelfRegEnvExtraCallback", selfReg, extraCB, false},
		{"SelfRegForeignCallback", selfReg, "https://evil.example/cb", true},
		{"SelfRegLoopbackCallback", selfReg, "http://127.0.0.1:33418/callback", true},
		{"SelfRegLocalhostCallback", selfReg, "http://localhost:8976/cb", true},
		{"SelfRegAllowlistedPlusQuery", selfReg, "https://claude.ai/api/mcp/auth_callback?x=1", true},
		{"SelfRegAllowlistedOtherPath", selfReg, "https://claude.ai/api/mcp/auth_callback/x", true},
		{"SelfRegAllowlistedHTTP", selfReg, "http://claude.ai/api/mcp/auth_callback", true},
		{"AdminDCRForeignCallback", adminDCR, "https://evil.example/cb", false},
		{"OperatorForeignCallback", operator, "https://evil.example/cb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := renderConsentPage(t, tc.client, tc.redirect)
			if got := strings.Contains(body, consentSelfRegMarker); got != tc.wantWarn {
				t.Errorf("warning shown = %v, want %v (client source %q, redirect %q)",
					got, tc.wantWarn, tc.client.RegistrationSource, tc.redirect)
			}
		})
	}
}

// TestConsentClientNameStripsFormatAndControl pins the client_name hygiene
// of §F-§6: the name is attacker-chosen free text under open registration,
// and html/template escapes markup but passes Unicode format characters
// (bidi overrides and isolates, zero-width runes) and control characters
// through. The consent page drops both classes from the name and isolates
// it in <bdi>, so the name cannot reorder or hide text around it. The
// render side covers every row alike — DCR, admin, CLI-created and rows
// stored before the registration-side filter existed.
func TestConsentClientNameStripsFormatAndControl(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label string
		want  string
	}{
		{"BidiAndZeroWidth", "Claude\u202e AI\u200b\u2066x\u2069", "Claude AIx"},
		{"ControlRunes", "ctx\u0007 \u0085client\u007f", "ctx client"},
		{"TagAndBOM", "\ufeffName\U000e0041\u061c", "Name"},
		{"OnlyInvisible", "\u200b\u202e\u2066", "unnamed client"},
		{"PlainUnchanged", "Claude Desktop — Prod", "Claude Desktop — Prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &store.OAuthClient{ClientID: "ctx_cf", Label: tc.label, RegistrationSource: "dcr"}
			body := renderConsentPage(t, client, "https://evil.example/cb")
			for _, r := range body {
				if unicode.Is(unicode.Cf, r) {
					t.Fatalf("rendered page carries format rune %U", r)
				}
			}
			if want := "<strong><bdi>" + tc.want + "</bdi></strong>"; !strings.Contains(body, want) {
				t.Errorf("rendered name: want %q in page", want)
			}
		})
	}
}

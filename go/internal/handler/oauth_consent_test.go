package handler

import (
	"net/url"
	"strings"
	"testing"

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

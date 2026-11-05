//go:build integration

// W06-9 gate (b), B-06-10 (masterplan gate "Slimmed(false) invariant, error
// without body"): what a /v1 row stores, read back the way the gated detail
// view reads it (classifyBodies over the stored columns).
//
//	go test -tags=integration -p 1 -count=1 -run TestLLMLogGatewayBodyState -v ./internal/handler/
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	gwBodyConsumer = "0195a6f0-0000-7000-8000-00000000c921"
	gwBodyModelKey = "0195a6f0-0000-7000-8000-00000000d921"
	gwBodyCanary   = "PROMPT-CANARY-b06-10"
)

// TestLLMLogGatewayBodyState is gate (b):
//   - none ⇒ bodyless: the bodies are '' and never NULL (NULL reads as
//     evicted, which only the retention janitor may write);
//   - full + credentials ⇒ sealed — the constructor has no devmode input,
//     so a tenant with tenant.devmode on cannot open the seal for a /v1 row
//     (Rev.-0 policy Slimmed(devmode) would); an empty or unknown
//     sensitivity stores no body either;
//   - an upstream 400 whose body carries a canary, with the walk's raw error
//     and bodies left on the entry ⇒ the canary is in neither error nor any
//     body column, under every policy.
func TestLLMLogGatewayBodyState(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	row := func(pipeline, policy, sensitivity string, rerr *llmlog.RedactedErr) llmlog.GatewayRow {
		return llmlog.GatewayRow{
			Pipeline:   pipeline,
			ConsumerID: gwBodyConsumer,
			Gateway:    llmlog.Gateway{Alias: "body-probe", ModelKeyID: gwBodyModelKey, Finish: llmlog.FinishStop},
			Telemetry: llmlog.Entry{
				Model: "m", Host: "h", RequiredSensitivity: sensitivity,
				Err:             &httpx.StatusError{Code: 400, Body: `{"error":"bad prompt: ` + gwBodyCanary + `"}`},
				RequestSystem:   gwBodyCanary,
				RequestUser:     gwBodyCanary,
				ResponseContent: gwBodyCanary,
			},
			Attempts:    []llmlog.WireAttempt{{Backend: "b", Ms: 5}},
			Err:         rerr,
			Request:     `{"messages":[{"role":"user","content":"hello"}]}`,
			Response:    `{"content":"world"}`,
			BodyLogging: policy,
		}
	}
	upstream400 := &llmlog.RedactedErr{Class: "bad_request", Status: 400, Code: "upstream_rejected"}

	cases := []struct {
		name  string
		row   llmlog.GatewayRow
		state string
	}{
		{"none", row("probe-none", llmlog.BodyLoggingNone, "internal", nil), bodyBodyless},
		{"none, error", row("probe-none-err", llmlog.BodyLoggingNone, "internal", upstream400), bodyBodyless},
		{"errors, success", row("probe-errors-ok", llmlog.BodyLoggingErrors, "internal", nil), bodyBodyless},
		{"errors, error", row("probe-errors-err", llmlog.BodyLoggingErrors, "internal", upstream400), bodyPresent},
		{"full, internal", row("probe-full", llmlog.BodyLoggingFull, "internal", nil), bodyPresent},
		{"full, credentials, tenant devmode on", row("probe-full-cred", llmlog.BodyLoggingFull, "credentials", nil), bodySealed},
		{"full, credentials, error", row("probe-full-cred-err", llmlog.BodyLoggingFull, "credentials", upstream400), bodySealed},
		// An undefined sensitivity is credentials (fail-closed): no bodies
		// stored, although Slimmed seals only the exact class name.
		{"full, empty sensitivity", row("probe-full-nosens", llmlog.BodyLoggingFull, "", nil), bodyBodyless},
		{"full, unknown sensitivity", row("probe-full-badsens", llmlog.BodyLoggingFull, "Credentials", nil), bodyBodyless},
	}
	for _, tc := range cases {
		llmlog.RecordGateway(pool, tc.row)
	}
	settleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := llmlog.Settle(settleCtx, pool); err != nil {
		t.Fatalf("settle: %v", err)
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				sens            *string
				sys, user, resp *string
				errCol          *string
				canaryAnywhere  bool
			)
			if err := pool.QueryRow(ctx, `
				SELECT required_sensitivity, request_system, request_user, response_content, error,
				       position($2 in concat_ws('|', error, request_system, request_user, response_content, metadata::text)) > 0
				  FROM context_llm_log WHERE pipeline = $1`, cases[i].row.Pipeline, gwBodyCanary).
				Scan(&sens, &sys, &user, &resp, &errCol, &canaryAnywhere); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if sys == nil || user == nil || resp == nil {
				t.Fatalf("a body column is NULL (%v/%v/%v) — '' is the gateway's empty, NULL reads as evicted", sys, user, resp)
			}
			s := ""
			if sens != nil {
				s = *sens
			}
			if state, _, _, _ := classifyBodies(s, sys, user, resp); state != tc.state {
				t.Fatalf("body_state = %q, want %q (bodies %q / %q / %q)", state, tc.state, *sys, *user, *resp)
			}
			if canaryAnywhere {
				t.Fatalf("the upstream canary is stored: error=%v bodies %q / %q / %q", gwDeref(errCol), *sys, *user, *resp)
			}
			if tc.row.Err != nil && gwDeref(errCol) != "bad_request 400 upstream_rejected" {
				t.Fatalf("error column %q, want the redacted text", gwDeref(errCol))
			}
			if tc.row.Err == nil && errCol != nil {
				t.Fatalf("success row has error %q", *errCol)
			}
		})
	}
}

func gwDeref(s *string) string {
	if s == nil {
		return "<NULL>"
	}
	return *s
}

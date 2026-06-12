package backends

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
)

func statusErr(code int, body string) error {
	return fmt.Errorf("llm: %w", &httpx.StatusError{Code: code, Body: body})
}

// TestClassifyCatalog probes one case per error class, exactly against the
// failover catalog table (design 03 §2.4).
func TestClassifyCatalog(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		provider string
		want     ErrClass
	}{
		{"nil is ok", nil, ProviderGeneric, ClassOK},
		{"transport errno", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), ProviderGeneric, ClassTransport},
		{"500 stops the chain", statusErr(500, "boom"), ProviderGeneric, ClassServerFault},
		{"502 goes next", statusErr(502, "bad gateway"), ProviderGeneric, ClassGateway},
		{"503 goes next", statusErr(503, "overloaded"), ProviderGeneric, ClassGateway},
		{"504 goes next", statusErr(504, "upstream timeout"), ProviderGeneric, ClassGateway},
		{"429 rate limit", statusErr(429, "slow down"), ProviderGeneric, ClassRateLimit},
		{"402 billing", statusErr(402, "negative balance"), ProviderGeneric, ClassBilling},
		{"401 auth", statusErr(401, "bad key"), ProviderGeneric, ClassAuth},
		{"403 auth", statusErr(403, "forbidden"), ProviderGeneric, ClassAuth},
		{"400 bad request", statusErr(400, "context overflow"), ProviderGeneric, ClassBadRequest},
		{"404 bad request", statusErr(404, "no such model"), ProviderGeneric, ClassBadRequest},
		{"413 bad request", statusErr(413, "too large"), ProviderGeneric, ClassBadRequest},
		{"unknown 5xx is gateway", statusErr(599, "weird"), ProviderGeneric, ClassGateway},
		{"unknown 4xx is bad request", statusErr(418, "teapot"), ProviderGeneric, ClassBadRequest},
		{"openrouter no providers", statusErr(404, `{"error":{"message":"No allowed providers are available for the selected model."}}`), ProviderOpenRouter, ClassNoProviders},
		{"no-providers body needs openrouter class", statusErr(404, "No allowed providers are available"), ProviderGeneric, ClassBadRequest},
		{"attempt deadline", context.DeadlineExceeded, ProviderGeneric, ClassTimeout},
		{"parent canceled", context.Canceled, ProviderGeneric, ClassCanceled},
		{"unclassified stops", errors.New("decode: invalid JSON"), ProviderGeneric, ClassServerFault},
	}
	for _, c := range cases {
		if got := Classify(c.err, c.provider); got != c.want {
			t.Errorf("%s: Classify = %s, want %s", c.name, got, c.want)
		}
	}
}

// TestClassNextAndCooldown pins the chain-continuation and cooldown columns.
func TestClassNextAndCooldown(t *testing.T) {
	next := map[ErrClass]bool{
		ClassTransport: true, ClassGateway: true, ClassRateLimit: true,
		ClassBilling: true, ClassAuth: true, ClassBadRequest: true,
		ClassNoProviders: true,
		ClassServerFault: false, ClassTimeout: false, ClassCanceled: false,
		ClassOK: false,
	}
	for class, want := range next {
		if got := class.Next(); got != want {
			t.Errorf("%s.Next() = %v, want %v", class, got, want)
		}
	}

	if d := ClassTransport.Cooldown(0); d != 30*time.Second {
		t.Errorf("transport cooldown = %s", d)
	}
	if d := ClassRateLimit.Cooldown(0); d != 60*time.Second {
		t.Errorf("429 default cooldown = %s", d)
	}
	if d := ClassRateLimit.Cooldown(90 * time.Second); d != 90*time.Second {
		t.Errorf("429 Retry-After not honored: %s", d)
	}
	if d := ClassRateLimit.Cooldown(20 * time.Minute); d != 5*time.Minute {
		t.Errorf("429 cap missed: %s", d)
	}
	if d := ClassBilling.Cooldown(0); d != time.Hour {
		t.Errorf("402 cooldown = %s", d)
	}
	if d := ClassAuth.Cooldown(0); d != 15*time.Minute {
		t.Errorf("auth cooldown = %s", d)
	}
	if d := ClassBadRequest.Cooldown(0); d != 0 {
		t.Errorf("400 must not punish the backend, got %s", d)
	}
}

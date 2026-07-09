package handler

import (
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
)

// TestMetadataDocument pins the 02-W3 gate: RFC 8414 required fields present,
// existing fields unchanged, and NO statement the server does not serve —
// no scopes_supported (no enforced catalog), no registration_endpoint while
// DCR is off (route lands in 02-W4). Since S4 the refresh_token grant IS
// served (rotation + reuse detection), so its advertisement flipped from a
// forbidden to a required statement — the coupling rule, applied in both
// directions.
func TestMetadataDocument(t *testing.T) {
	h := &OAuthHandler{issuer: "https://ctx.example"}

	fetch := func(t *testing.T) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.Metadata(rec, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("metadata decode: %v", err)
		}
		return doc
	}

	t.Run("rfc8414 required + unchanged base fields", func(t *testing.T) {
		doc := fetch(t)
		if doc["issuer"] != "https://ctx.example" {
			t.Errorf("issuer = %v", doc["issuer"])
		}
		if doc["authorization_endpoint"] != "https://ctx.example/authorize" {
			t.Errorf("authorization_endpoint = %v", doc["authorization_endpoint"])
		}
		if doc["token_endpoint"] != "https://ctx.example/token" {
			t.Errorf("token_endpoint = %v", doc["token_endpoint"])
		}
		if rt := toStrings(doc["response_types_supported"]); !slices.Equal(rt, []string{"code"}) {
			t.Errorf("response_types_supported = %v", rt)
		}
		if cc := toStrings(doc["code_challenge_methods_supported"]); !slices.Equal(cc, []string{"S256"}) {
			t.Errorf("code_challenge_methods_supported = %v", cc)
		}
		if am := toStrings(doc["token_endpoint_auth_methods_supported"]); !slices.Equal(am, []string{"none", "client_secret_basic", "client_secret_post"}) {
			t.Errorf("token_endpoint_auth_methods_supported = %v", am)
		}
		if v, ok := doc["client_id_metadata_document_supported"].(bool); !ok || v {
			t.Errorf("client_id_metadata_document_supported = %v (want false)", doc["client_id_metadata_document_supported"])
		}
	})

	t.Run("no unserved statements", func(t *testing.T) {
		doc := fetch(t)
		if _, ok := doc["scopes_supported"]; ok {
			t.Error("scopes_supported advertised without an enforced catalog")
		}
		if gt := toStrings(doc["grant_types_supported"]); !slices.Equal(gt, []string{"authorization_code", "refresh_token"}) {
			t.Errorf("grant_types_supported = %v (S4 serves refresh_token — advertise exactly what is served)", gt)
		}
		if _, ok := doc["registration_endpoint"]; ok {
			t.Error("registration_endpoint advertised while DCR mode is off")
		}
	})

	t.Run("registration_endpoint follows DCR mode", func(t *testing.T) {
		t.Setenv(EnvDCRMode, "admin")
		if doc := fetch(t); doc["registration_endpoint"] != "https://ctx.example/register" {
			t.Errorf("registration_endpoint = %v (want advertised in admin mode)", doc["registration_endpoint"])
		}
		t.Setenv(EnvDCRMode, "banana")
		if doc := fetch(t); doc["registration_endpoint"] != nil {
			t.Error("unknown DCR mode must fail closed to off")
		}
	})
}

func toStrings(v any) []string {
	arr, _ := v.([]any)
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

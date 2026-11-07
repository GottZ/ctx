package backends

import (
	"slices"
	"strings"
	"testing"
)

// ctxRow is the minimal valid peer row of design/04 §4.12: protocol openai,
// the peer's model key by name, a LAN locality, an interactive role.
func ctxRow(mut func(b *Backend)) Backend {
	b := Backend{
		Name: "peer-a", Host: "http://10.0.0.5:8077", Protocol: ProtocolOpenAI,
		ProviderClass: ProviderCtx, APIKeyRef: "peer-key", Locality: LocalityLAN,
		Trust: TrustPublic, Roles: []string{RoleChat}, Enabled: true,
		ModelMap: map[string]ModelSpec{"default": {Model: "peer-model"}},
	}
	if mut != nil {
		mut(&b)
	}
	return b
}

func errFields(errs []FieldError) []string {
	var out []string
	for _, e := range errs {
		if !slices.Contains(out, e.Field) {
			out = append(out, e.Field)
		}
	}
	slices.Sort(out)
	return out
}

// TestValidateCtxPositive is gate G2: the peer row of the design validates
// without an error or a warning — provider_class ctx is a writable class.
func TestValidateCtxPositive(t *testing.T) {
	b := ctxRow(nil)
	warnings, errs := ValidateBackend(&b)
	if len(errs) != 0 || len(warnings) != 0 {
		t.Fatalf("ctx row: errs %v warnings %v, want none", errs, warnings)
	}
}

// TestValidateCtxRules is gate G3 (design/04 §7 F-W5 (b), §4.12): each rule
// of the ctx row refuses its own field with its own reason, and nothing
// stricter than the rules — a generic LAN embed row and a ctx row without a
// background role stay valid. wantFields is the exact set of refused fields.
func TestValidateCtxRules(t *testing.T) {
	const (
		msgProtocol = "provider_class ctx requires protocol 'openai'"
		msgKey      = "provider_class ctx requires api_key_ref"
		msgEmbed    = "vector-space pin not available for ctx rows yet"
		msgSlots    = "provider_class ctx with a background role requires limits.slots"
	)
	for _, tc := range []struct {
		name       string
		row        Backend
		wantFields []string
		wantMsg    string // substring of one error of the first wantFields entry
		wantWarns  int
	}{
		{"protocol ollama", ctxRow(func(b *Backend) { b.Protocol = ProtocolOllama }), []string{"protocol"}, msgProtocol, 0},
		{"protocol rerank", ctxRow(func(b *Backend) { b.Protocol = "rerank" }), []string{"protocol"}, msgProtocol, 0},
		// Bestand: anthropic already demands provider_class generic (validateAnthropic).
		{"protocol anthropic", ctxRow(func(b *Backend) { b.Protocol = ProtocolAnthropic; b.NumCtx = 100000 }),
			[]string{"locality", "protocol", "provider_class"}, "", 0},
		{"keyless", ctxRow(func(b *Backend) { b.APIKeyRef = "" }), []string{"api_key_ref"}, msgKey, 0},
		{"blank key", ctxRow(func(b *Backend) { b.APIKeyRef = "  " }), []string{"api_key_ref"}, msgKey, 0},
		{"lan embed with slots", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleEmbed}, map[string]any{"slots": float64(2)}
		}), []string{"roles"}, msgEmbed, 0},
		// embed is a background role too: without slots both rules refuse.
		{"lan embed without slots", ctxRow(func(b *Backend) { b.Roles = []string{RoleEmbed} }), []string{"limits.slots", "roles"}, msgSlots, 0},
		{"lan dream-embed with slots", ctxRow(func(b *Backend) {
			b.Roles = []string{RoleDreamEmbed}
			b.Limits = map[string]any{"slots": float64(2)}
		}), []string{"roles"}, msgEmbed, 0},
		{"external embed with an equivalence proof", ctxRow(func(b *Backend) {
			b.Host, b.Locality, b.Roles = "https://peer.example.com", LocalityExternal, []string{RoleEmbed}
			b.Metadata = map[string]any{"embed_equivalence_verified": true}
			b.Limits = map[string]any{"slots": float64(2)}
		}), []string{"roles"}, msgEmbed, 0},
		{"dream without slots", ctxRow(func(b *Backend) { b.Roles = []string{RoleDream} }), []string{"limits.slots"}, msgSlots, 0},
		{"digest without slots", ctxRow(func(b *Backend) { b.Roles = []string{RoleChat, RoleDigest} }), []string{"limits.slots"}, msgSlots, 0},
		{"classify without slots", ctxRow(func(b *Backend) { b.Roles = []string{RoleClassify} }), []string{"limits.slots"}, msgSlots, 0},
		{"dream slots 0", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": float64(0)}
		}), []string{"limits.slots"}, msgSlots, 0},
		{"dream slots string", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": "4"}
		}), []string{"limits.slots"}, msgSlots, 0},
		{"dream slots 4.5", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": 4.5}
		}), []string{"limits.slots"}, msgSlots, 0},
		{"dream slots negative", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": float64(-1)}
		}), []string{"limits.slots"}, msgSlots, 0},
		{"dream slots above int32", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": 1e19}
		}), []string{"limits.slots"}, msgSlots, 0},
		{"dream slots 4 over a peer limit of 2 warns", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": float64(4)}
			b.Metadata = map[string]any{"peer_limits": map[string]any{"max_concurrency": float64(2)}}
		}), nil, "", 1},
		{"dream slots 2 at a peer limit of 2", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": float64(2)}
			b.Metadata = map[string]any{"peer_limits": map[string]any{"max_concurrency": float64(2)}}
		}), nil, "", 0},
		{"dream slots 4 without a peer limit", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": float64(4)}
		}), nil, "", 0},
		{"dream slots 4, unreadable peer limit", ctxRow(func(b *Backend) {
			b.Roles, b.Limits = []string{RoleDream}, map[string]any{"slots": float64(4)}
			b.Metadata = map[string]any{"peer_limits": map[string]any{"max_concurrency": "x"}}
		}), nil, "", 0},
		// Against over-strictness: no background role, no slots duty; the
		// embed refusal is the ctx class's, never every LAN row's.
		{"ctx chat without slots", ctxRow(nil), nil, "", 0},
		{"ctx rerank role without slots", ctxRow(func(b *Backend) { b.Roles = []string{RoleRerank} }), nil, "", 0},
		{"generic lan embed", ctxRow(func(b *Backend) {
			b.ProviderClass, b.APIKeyRef, b.Roles = ProviderGeneric, "", []string{RoleEmbed}
		}), nil, "", 0},
		{"generic lan dream without slots", ctxRow(func(b *Backend) {
			b.ProviderClass, b.APIKeyRef, b.Roles = ProviderGeneric, "", []string{RoleDream}
		}), nil, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := tc.row
			warnings, errs := ValidateBackend(&row)
			if got := errFields(errs); !slices.Equal(got, tc.wantFields) {
				t.Fatalf("refused fields %v, want %v (errs %v)", got, tc.wantFields, errs)
			}
			if tc.wantMsg != "" {
				found := false
				for _, e := range errs {
					if e.Field == tc.wantFields[0] && strings.Contains(e.Message, tc.wantMsg) {
						found = true
					}
				}
				if !found {
					t.Errorf("no %s error carries %q: %v", tc.wantFields[0], tc.wantMsg, errs)
				}
			}
			if len(warnings) != tc.wantWarns {
				t.Errorf("warnings %v, want %d", warnings, tc.wantWarns)
			}
			if tc.wantWarns == 1 && !strings.Contains(warnings[0], "metadata.peer_limits.max_concurrency") {
				t.Errorf("warning %q does not name metadata.peer_limits.max_concurrency", warnings[0])
			}
		})
	}
}

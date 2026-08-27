package dream

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// TestGenerateKeywords_FillsTooFew pins den Ornith-Fix (prod 2026-08-27):
// wenn das LLM valide, aber zu wenige Keywords liefert (2 statt >= MinKeywords),
// werden sie mit dem deterministischen Tokenizer aufgefuellt statt den Versuch
// als Fehler zu verwerfen. Die LLM-Keywords bleiben erhalten.
func TestGenerateKeywords_FillsTooFew(t *testing.T) {
	r := newTestRouter()
	mockChatJSON(t, func(ctx context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		// Ornith-Signatur: nur 2 Keywords
		return constResp(`["Reverse Proxy","Load Balancing"]`)(ctx, "", "", "", nil, "", "", llm.Options{}, 0)
	})

	blk := srcBlock("00000000-0000-4000-8000-00000000000b")
	blk.Sensitivity = backends.SensInternal
	blk.Title = "Reverse Proxy Setup"
	blk.Content = "Ein Reverse Proxy ist ein Server der als Vermittler zwischen Clients und Backend-Servern fungiert. Er terminiert TLS, verteilt Last und versteckt die Backend-Infrastruktur. Traefik ist unser Reverse Proxy auf INFRA-RP."

	kws, err := GenerateKeywords(context.Background(), nil, r, &blk)
	if err != nil {
		t.Fatalf("GenerateKeywords should fill too-few, got error: %v", err)
	}
	if len(kws) < MinKeywords {
		t.Fatalf("keywords = %v, want >= %d (filled)", kws, MinKeywords)
	}
	// LLM-Keywords muessen erhalten bleiben
	found := false
	for _, k := range kws {
		if k == "reverse proxy" || k == "Reverse Proxy" || k == "load balancing" || k == "Load Balancing" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("LLM-Keywords gingen verloren: %v", kws)
	}
}

// TestFillKeywords_DedupAndFill pinnt fillKeywords direkt: keine Duplikate,
// LLM-Keywords zuerst, Tokenizer fuellt bis MinKeywords.
func TestFillKeywords_DedupAndFill(t *testing.T) {
	llm := []string{"Reverse Proxy", "Traefik"}
	filled := fillKeywords("Reverse Proxy Setup", "Reverse Proxy Traefik INFRA-RP TLS Backend Load Balancer", llm)
	if len(filled) < MinKeywords {
		t.Fatalf("filled = %v, want >= %d", filled, MinKeywords)
	}
	seen := map[string]bool{}
	for _, k := range filled {
		if seen[k] {
			t.Fatalf("Duplikat: %q in %v", k, filled)
		}
		seen[k] = true
	}
	// LLM-Keywords zuerst
	if filled[0] != "Reverse Proxy" || filled[1] != "Traefik" {
		t.Errorf("LLM-Keywords nicht zuerst: %v", filled)
	}
}

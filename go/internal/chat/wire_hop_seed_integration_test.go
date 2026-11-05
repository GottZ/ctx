//go:build integration

package chat

// F-W4 gates (b) openrouter clause and (f) on rows that only the psql path
// can write: extra_headers the API denylist refuses — federation headers and
// the core credential headers — seeded by SQL, loaded by Pool.Reload, sent at
// every pinned wire site. The stamp removes the row's federation headers
// (step 1): openrouter sends none at all, a generic row its own fresh token
// only. The core headers stay the last word: on the OpenAI, Ollama, embed and
// rerank wires the row's Authorization is overwritten by the key from
// api_key_ref, on the Messages API wires x-api-key and anthropic-version by
// the key and the version pin. Authorization is no core header of the
// Messages API: a psql-written row Authorization reaches that wire beside
// x-api-key, before and after this wave alike (not checked here, an open
// point of the credential-carrier rule). Red before the stamp: openrouter
// sends the seeded Ctx-Hop and X-Ctx-Dispatch-Class, the generic row the
// forged token.
//
//	go test -tags=integration -p 1 ./internal/chat/ -run TestWireHopSeededRowHeaders -count=1 -v

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestWireHopSeededRowHeaders_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const seeded = `{"Ctx-Hop":"x","X-Ctx-Dispatch-Class":"interactive","Authorization":"Bearer row-forged","x-api-key":"row-forged","anthropic-version":"row-forged"}`
	for _, r := range []struct{ name, class, url string }{
		{"seed-openrouter", backends.ProviderOpenRouter, "https://openrouter.ai/api"},
		{"seed-generic", backends.ProviderGeneric, "http://seed-generic"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO context_backends
			(name, base_url, provider_class, api_key_ref, trust, roles, model_map, extra_headers)
			VALUES ($1, $2, $3, 'seed-key', 'full-trust', '{chat,embed,rerank}', '{"default":"m"}', $4::jsonb)`,
			r.name, r.url, r.class, seeded); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
	}
	bp := backends.NewPool(pool, func(context.Context, string, string, string) (string, error) { return "core-key", nil })
	if err := bp.Reload(ctx); err != nil {
		t.Fatalf("pool reload: %v", err)
	}
	rows := map[string]backends.Backend{}
	for _, b := range bp.Snapshot() {
		rows[b.Name] = b
	}
	if len(rows) != 2 || rows["seed-generic"].ExtraHeaders["Ctx-Hop"] != "x" || rows["seed-openrouter"].APIKey != "core-key" {
		t.Fatalf("seeded rows not loaded as written: %+v", rows)
	}

	id := hopTestIdentity()
	sctx := fedhop.WithIdentity(ctx, id)
	for _, name := range []string{"seed-openrouter", "seed-generic"} {
		row := rows[name]
		for _, file := range pinnedWireFiles() {
			got, errs := captureHops(t, sctx, file, row, nil)
			checkSiteErrors(t, file, errs)
			for i, c := range got {
				where := fmt.Sprintf("%s request %d (%s, %s)", file, i+1, c.Path, name)
				fh := federationHeaders(c.Header)
				switch hop := c.Header.Values(fedhop.HeaderHop); {
				case row.ProviderClass == backends.ProviderOpenRouter && len(fh) != 0:
					t.Errorf("%s: sends %v to openrouter — the seeded federation headers must be gone and tier None adds none", where, fh)
				case row.ProviderClass != backends.ProviderOpenRouter &&
					(!slices.Equal(fh, []string{fedhop.HeaderHop}) || len(hop) != 1 || !id.IsOwnToken(hop[0])):
					t.Errorf("%s: federation headers %v, Ctx-Hop %q — want the own token in place of the seeded one", where, fh, hop)
				}
				if c.Path == "/v1/messages" {
					if got := c.Header.Get("x-api-key"); got != "core-key" {
						t.Errorf("%s: x-api-key %q, want the core key from api_key_ref", where, got)
					}
					if got := c.Header.Get("anthropic-version"); got != llm.AnthropicVersion {
						t.Errorf("%s: anthropic-version %q, want the pin %q", where, got, llm.AnthropicVersion)
					}
					continue
				}
				if got := c.Header.Get("Authorization"); got != "Bearer core-key" {
					t.Errorf("%s: Authorization %q, want the core key from api_key_ref", where, got)
				}
			}
		}
	}
}

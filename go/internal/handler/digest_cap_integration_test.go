//go:build integration

// Issue #518, the request: POST /api/digest over a read window whose topic map
// would be past PostgreSQL's 1 MB limit on a tsvector. Before the cap the
// request answered 500 ("Digest generation failed", SQLSTATE 54000 in the log)
// on every call; now it answers 200 with a map under the cap, and the answer
// says the map is truncated.
//
//	go test -tags=integration ./internal/handler/ -run TestDigestCapsTheMapPastTheVectorLimit -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// digestCapTitle is a hyphenated compound of 14 alphanumeric parts — the
// title pattern that costs the most full-text vector bytes per map byte.
func digestCapTitle(r *rand.Rand) string {
	parts := make([]string, 14)
	for i := range parts {
		parts[i] = string([]byte{byte('a' + r.IntN(26)), byte('0' + r.IntN(10)), byte('a' + r.IntN(26)), byte('0' + r.IntN(10))})
	}
	return strings.Join(parts, "-")
}

func TestDigestCapsTheMapPastTheVectorLimit(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// 9,000 blocks through the production write path, over two scopes of the
	// key's window and three categories: an uncapped map of about 760 KB, past
	// the vector limit for these titles (about 570 KB).
	const n = 9000
	r := rand.New(rand.NewPCG(518, 7))
	titles := make([]string, n)
	for i := range titles {
		titles[i] = digestCapTitle(r)
	}
	cats := []string{"decisions", "learnings", "reference"}
	scopes := []string{"private", "work"}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < n; i += 4 {
				if _, err := store.UpsertBlock(ctx, pool, cats[i%3], titles[i], "cap fixture", nil, nil,
					scopes[i%2], true, store.SensitivityWrite{}, ""); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		t.Fatalf("seed: %v", err)
	}

	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	cfg := &config.Config{}
	cfg.Digest.Mode = "full"
	cfg.RootMap.CountTimeout = 5 * time.Second
	h := NewDigestHandler(pool, reg, staticConfigStore{cfg: cfg})
	ar := &auth.AuthResult{IsValid: true, ApiKeyID: "00000000-0000-7000-8000-0000000005e1",
		HomeScope: "private", ReadScopes: scopes}

	req := httptest.NewRequest(http.MethodPost, "/api/digest", strings.NewReader(`{"trigger":"manual"}`))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleDigest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/digest past the vector limit: status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Success       bool `json:"success"`
		ContentLength int  `json:"contentLength"`
		ListedBlocks  int  `json:"listedBlocks"`
		Truncated     bool `json:"truncated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if !body.Success || !body.Truncated || body.ListedBlocks <= 0 || body.ListedBlocks >= n {
		t.Errorf("answer = %s, want success, truncated, 0 < listedBlocks < %d", rec.Body.String(), n)
	}
	if body.ContentLength <= 0 || body.ContentLength > 256<<10 {
		t.Errorf("contentLength = %d, want a map under the cap of %d bytes", body.ContentLength, 256<<10)
	}
}

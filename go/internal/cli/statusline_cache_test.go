package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The cache file's rules (#265), read at the functions that apply them:
// lifetime boundary, entries of another server/key, files that are not ours,
// and no cache directory at all.

func cacheEntryFixture(t *testing.T, key string) (path, id string) {
	t.Helper()
	isolateStatuslineDirs(t)
	id = statuslineCacheID(Config{BaseURL: "http://127.0.0.1:1", Key: key})
	path = statuslineCachePath(id)
	if path == "" {
		t.Fatal("no cache path although XDG_CACHE_HOME is set")
	}
	return path, id
}

// TestStatuslineCacheLifetime pins the boundary: an entry serves while it is
// younger than statuslineCacheTTL, and an entry stamped in the future (a clock
// that went back, a hand-made file) serves never.
func TestStatuslineCacheLifetime(t *testing.T) {
	path, id := cacheEntryFixture(t, statusKeyA)
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	want := statusCache{Health: "ok", BlockCount: 9, HasBlockCount: true}
	if err := writeStatuslineCache(path, id, &want, t0); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got, ok := readStatuslineCache(path, id, t0.Add(statuslineCacheTTL-time.Nanosecond)); !ok || *got != want {
		t.Errorf("entry just inside the lifetime = %v, %v; want %v, true", got, ok, want)
	}
	if _, ok := readStatuslineCache(path, id, t0.Add(statuslineCacheTTL)); ok {
		t.Error("entry exactly statuslineCacheTTL old still serves")
	}
	if _, ok := readStatuslineCache(path, id, t0.Add(-time.Second)); ok {
		t.Error("entry stamped in the future serves")
	}
}

// TestStatuslineCacheRejectsForeignFiles: a file that is not an entry for this
// server and key is no cache.
func TestStatuslineCacheRejectsForeignFiles(t *testing.T) {
	path, id := cacheEntryFixture(t, statusKeyA)
	now := time.Now()
	ok := statusCache{Health: "ok", BlockCount: 9, HasBlockCount: true}

	otherID := statuslineCacheID(Config{BaseURL: "http://127.0.0.1:1", Key: statusKeyB})
	if err := writeStatuslineCache(path, otherID, &ok, now); err != nil {
		t.Fatal(err)
	}
	if _, hit := readStatuslineCache(path, id, now); hit {
		t.Error("an entry of another key, placed at this key's path, serves")
	}

	negative := statusCache{Health: "ok", BlockCount: -1, HasBlockCount: true}
	if err := writeStatuslineCache(path, id, &negative, now); err != nil {
		t.Fatal(err)
	}
	if _, hit := readStatuslineCache(path, id, now); hit {
		t.Error("an entry with a negative block count serves")
	}

	if err := writeStatuslineCache(path, id, &ok, now); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	padded := strings.Replace(string(data), "{", "{"+strings.Repeat(" ", statuslineCacheMaxBytes), 1)
	if err := os.WriteFile(path, []byte(padded), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, hit := readStatuslineCache(path, id, now); hit {
		t.Error("an entry larger than statuslineCacheMaxBytes serves")
	}

	if _, hit := readStatuslineCache(filepath.Join(filepath.Dir(path), "absent.json"), id, now); hit {
		t.Error("a missing file serves")
	}
}

// TestStatuslineCacheIDSeparates: the ID differs per server and per key and
// does not depend on a trailing slash of the base URL.
func TestStatuslineCacheIDSeparates(t *testing.T) {
	a := statuslineCacheID(Config{BaseURL: "https://ctx.example", Key: statusKeyA})
	if b := statuslineCacheID(Config{BaseURL: "https://ctx.example/", Key: statusKeyA}); a != b {
		t.Error("a trailing slash changes the cache ID")
	}
	if b := statuslineCacheID(Config{BaseURL: "https://ctx.example", Key: statusKeyB}); a == b {
		t.Error("two keys share a cache ID")
	}
	if b := statuslineCacheID(Config{BaseURL: "https://other.example", Key: statusKeyA}); a == b {
		t.Error("two servers share a cache ID")
	}
}

// TestStatuslineExpiredEntryRefetches: fetchBackendData honours the lifetime —
// an entry older than statuslineCacheTTL leads to a fetch.
func TestStatuslineExpiredEntryRefetches(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "ok", false, 7)
	runStatusline(t, srv.url, statusKeyA)

	cfg := Config{BaseURL: srv.url, Key: statusKeyA}
	id := statuslineCacheID(cfg)
	stale := statusCache{Health: "ok", BlockCount: 1, HasBlockCount: true}
	if err := writeStatuslineCache(statuslineCachePath(id), id, &stale, time.Now().Add(-statuslineCacheTTL)); err != nil {
		t.Fatal(err)
	}

	plain := statusANSI.ReplaceAllString(runStatusline(t, srv.url, statusKeyA), "")
	if h := srv.healthCalls.Load(); h != 2 {
		t.Errorf("health calls = %d, want 2 (the expired entry is a miss)", h)
	}
	if !strings.Contains(plain, "\U000F09EE 7") {
		t.Errorf("the expired entry was rendered instead of the fresh fetch: %q", plain)
	}
}

// TestStatuslineWithoutCacheDir: no cache directory resolvable means no cache —
// every run fetches, and nothing lands in the working directory.
func TestStatuslineWithoutCacheDir(t *testing.T) {
	isolateStatuslineDirs(t)
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	cwd := t.TempDir()
	t.Chdir(cwd)
	if p := statuslineCachePath("x"); p != "" {
		t.Fatalf("cache path without any home = %q, want \"\"", p)
	}

	srv := newFakeCtxd(t, "ok", false, 7)
	runStatusline(t, srv.url, statusKeyA)
	plain := statusANSI.ReplaceAllString(runStatusline(t, srv.url, statusKeyA), "")
	if !strings.Contains(plain, "\U000F09EE 7") {
		t.Errorf("render without a cache directory: %q", plain)
	}
	if h := srv.healthCalls.Load(); h != 2 {
		t.Errorf("health calls = %d, want 2 (no cache, every run fetches)", h)
	}
	if files := cacheFiles(t, cwd); len(files) != 0 {
		t.Errorf("files written into the working directory: %v", files)
	}
}

// TestStatuslineHealthBodyTimeoutIsUnknown: the budget also runs out while
// the body is read (headers sent, body stalls) — that is the unknown state
// too, not offline.
func TestStatuslineHealthBodyTimeoutIsUnknown(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":`))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"stats":{"total_blocks":5}}`))
	}))
	t.Cleanup(srv.Close)

	out := runStatusline(t, srv.URL, statusKeyA)
	if !strings.Contains(out, glyphTimeout) || strings.Contains(out, glyphOffline) {
		t.Errorf("a body that stalls past the budget is not the unknown state: %q", statusANSI.ReplaceAllString(out, ""))
	}
}

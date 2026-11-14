package cli

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// #265: the statusline shows the block count whenever the stats call returned
// one, puts the health indicator next to it, tells a health call that ran out
// of time apart from an offline server, and serves one fetch result from a
// small per-server, per-key cache file for a short lifetime.

const (
	glyphOffline  = "" // red warning: unhealthy or unreachable
	glyphDegraded = "" // yellow warning: degraded
	glyphTimeout  = "" // dim hourglass: no answer within the call budget
	statusKeyA    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	statusKeyB    = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

var statusANSI = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// fakeCtxd is a stand-in ctxd: GET /health answers {"status": health}, or
// never (hangHealth) until the client gives up; POST /api/manage answers the
// stats envelope with blocks. Both routes count their requests.
type fakeCtxd struct {
	url         string
	healthCalls atomic.Int32
	statsCalls  atomic.Int32
}

func newFakeCtxd(t *testing.T, health string, hangHealth bool, blocks int) *fakeCtxd {
	t.Helper()
	f := &fakeCtxd{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			f.healthCalls.Add(1)
			if hangHealth {
				<-r.Context().Done() // answers only when the caller gave up
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": health, "services": map[string]string{"database": "ok"}})
		case "/api/manage":
			f.statsCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "stats": map[string]any{"total_blocks": blocks}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// isolateStatuslineDirs points every user directory the CLI could resolve at
// test-owned temp dirs and returns the cache root.
func isolateStatuslineDirs(t *testing.T) string {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("APPDATA", "")
	return cache
}

// runStatusline drives `ctx statusline` end to end — Claude Code's JSON on
// stdin, config from the environment, the rendered line from stdout.
func runStatusline(t *testing.T, baseURL, key string) string {
	t.Helper()
	t.Setenv("CTX_BASE_URL", baseURL)
	t.Setenv("CTX_KEY", key)
	var out string
	withStdin(t, "{}", func() {
		out = captureStdout(t, func() {
			cmd := statuslineCmd(nil)
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Errorf("statusline returned %v, want nil (it must never fail the prompt)", err)
			}
		})
	})
	return out
}

// cacheFiles lists the regular files under the cache root.
func cacheFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cache root: %v", err)
	}
	return out
}

// TestStatuslineHealthTimeoutKeepsBlockCount is the issue's observation: a
// /health call that outlives the statusline budget hid a block count fetched
// in the same run and showed the server as offline.
func TestStatuslineHealthTimeoutKeepsBlockCount(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "ok", true, 10519)

	out := runStatusline(t, srv.url, statusKeyA)
	plain := statusANSI.ReplaceAllString(out, "")
	if !strings.Contains(plain, "10519") {
		t.Errorf("block count missing although the stats call returned it: %q", plain)
	}
	if strings.Contains(out, glyphOffline) {
		t.Errorf("a health call that ran out of time is shown as offline: %q", plain)
	}
	if !strings.Contains(out, glyphTimeout) {
		t.Errorf("a health call that ran out of time carries no no-answer indicator: %q", plain)
	}
}

// TestStatuslineDegradedShowsCountAndIndicator: health not ok puts the
// indicator next to the count instead of replacing it.
func TestStatuslineDegradedShowsCountAndIndicator(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "degraded", false, 42)

	out := runStatusline(t, srv.url, statusKeyA)
	plain := statusANSI.ReplaceAllString(out, "")
	if !strings.Contains(plain, "42") {
		t.Errorf("block count missing on a degraded server: %q", plain)
	}
	if !strings.Contains(out, glyphDegraded) {
		t.Errorf("degraded indicator missing: %q", plain)
	}
}

// TestStatuslineEmptyStoreShowsZero: a stats call that returned 0 returned a
// count, and the count is shown.
func TestStatuslineEmptyStoreShowsZero(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "ok", false, 0)

	plain := statusANSI.ReplaceAllString(runStatusline(t, srv.url, statusKeyA), "")
	if !strings.Contains(plain, "\U000F09EE 0") {
		t.Errorf("a stats answer of 0 blocks is not shown: %q", plain)
	}
}

// TestStatuslineUnreachableServerIsOffline: a refused connection is offline
// (red indicator, no count) — the timeout state does not swallow it.
func TestStatuslineUnreachableServerIsOffline(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	out := runStatusline(t, url, statusKeyA)
	if !strings.Contains(out, glyphOffline) {
		t.Errorf("an unreachable server is not shown as offline: %q", statusANSI.ReplaceAllString(out, ""))
	}
	if strings.Contains(out, glyphTimeout) {
		t.Errorf("a refused connection is shown as a timeout: %q", statusANSI.ReplaceAllString(out, ""))
	}
	if strings.Contains(out, "\U000F09EE") {
		t.Errorf("a block count is shown for an unreachable server: %q", statusANSI.ReplaceAllString(out, ""))
	}
}

// TestStatuslineCacheServesWithinLifetime: the second run inside the cache
// lifetime renders the same line without a request.
func TestStatuslineCacheServesWithinLifetime(t *testing.T) {
	isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "ok", false, 7)

	first := runStatusline(t, srv.url, statusKeyA)
	second := runStatusline(t, srv.url, statusKeyA)
	if h, s := srv.healthCalls.Load(), srv.statsCalls.Load(); h != 1 || s != 1 {
		t.Errorf("two runs inside the cache lifetime made %d health and %d stats calls, want 1 and 1", h, s)
	}
	if first != second {
		t.Errorf("cached run renders %q, fresh run %q", second, first)
	}
}

// TestStatuslineCachePerServerAndKey: the cache is keyed by server URL and API
// key, the key and the URL appear in neither file name nor content, and the
// file is private to the user.
func TestStatuslineCachePerServerAndKey(t *testing.T) {
	root := isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "ok", false, 7)
	other := newFakeCtxd(t, "ok", false, 8)

	runStatusline(t, srv.url, statusKeyA)
	runStatusline(t, srv.url, statusKeyB)
	runStatusline(t, other.url, statusKeyA)
	if h := srv.healthCalls.Load(); h != 2 {
		t.Errorf("the same server with two keys made %d health calls, want 2 (one cache entry per key)", h)
	}
	if h := other.healthCalls.Load(); h != 1 {
		t.Errorf("a second server made %d health calls, want 1 (one cache entry per server)", h)
	}

	files := cacheFiles(t, root)
	if len(files) != 3 {
		t.Fatalf("cache files = %v, want 3 (server×key pairs)", files)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Dir(files[0]))
		if err != nil {
			t.Fatalf("stat cache dir: %v", err)
		}
		if mode := st.Mode().Perm(); mode != 0o700 {
			t.Errorf("cache directory mode = %o, want 700", mode)
		}
	}
	for _, p := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, secret := range []string{statusKeyA, statusKeyB, srv.url, other.url, strings.TrimPrefix(srv.url, "http://")} {
			if strings.Contains(p, secret) || strings.Contains(string(data), secret) {
				t.Errorf("cache file %s carries %q in its name or content", p, secret)
			}
		}
		if runtime.GOOS != "windows" {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", p, err)
			}
			if mode := st.Mode().Perm(); mode != 0o600 {
				t.Errorf("cache file %s mode = %o, want 600", p, mode)
			}
		}
	}
}

// TestStatuslineBrokenCacheIsNoCache: a cache file that does not parse is a
// miss — the run fetches, renders and replaces it, it does not fail.
func TestStatuslineBrokenCacheIsNoCache(t *testing.T) {
	root := isolateStatuslineDirs(t)
	srv := newFakeCtxd(t, "ok", false, 7)

	runStatusline(t, srv.url, statusKeyA)
	files := cacheFiles(t, root)
	if len(files) != 1 {
		t.Fatalf("cache files after one run = %v, want 1", files)
	}
	if err := os.WriteFile(files[0], []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	plain := statusANSI.ReplaceAllString(runStatusline(t, srv.url, statusKeyA), "")
	if !strings.Contains(plain, "\U000F09EE 7") {
		t.Errorf("a broken cache file broke the render: %q", plain)
	}
	if h := srv.healthCalls.Load(); h != 2 {
		t.Errorf("health calls = %d, want 2 (the broken file is a miss)", h)
	}
	data, err := os.ReadFile(files[0])
	if err != nil || !json.Valid(data) {
		t.Errorf("the broken file was not replaced by a fresh entry: %q (%v)", data, err)
	}
}

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/clientconfig"
)

// statuslineCacheTTL is how long one fetch result serves the statusline (#265).
// Claude Code reruns the statusline on every conversation update — several
// times a second while a reply streams — and every fetch is two HTTP calls, of
// which /health fans out on the server into a probe per backend role. Ten
// seconds turn that into at most one fetch per server and key every ten
// seconds, and a health or block-count change still shows within that time.
// The value is fixed on purpose: it bounds how stale a status bar may be, and
// nothing on the server or in any other command depends on it.
const statuslineCacheTTL = 10 * time.Second

// statuslineCacheMaxBytes caps what a cache read accepts. An entry is about
// 150 bytes; anything larger is not one of ours and counts as no cache.
const statuslineCacheMaxBytes = 4096

// statuslineCacheEntry is the cache file's content. It names its server and
// key only through ID, never in clear.
type statuslineCacheEntry struct {
	ID        string      `json:"id"`
	FetchedAt time.Time   `json:"fetched_at"`
	Backend   statusCache `json:"backend"`
}

// statuslineCacheID names one server-and-key pair without carrying either: a
// domain-separated SHA-256 over both. Neither the key nor the URL reaches the
// disk in clear, and the digest is no hash the server keeps of the key.
func statuslineCacheID(cfg Config) string {
	h := sha256.New()
	h.Write([]byte("ctx statusline cache v1\x00"))
	h.Write([]byte(strings.TrimSuffix(cfg.BaseURL, "/")))
	h.Write([]byte{0})
	h.Write([]byte(cfg.Key))
	return hex.EncodeToString(h.Sum(nil))
}

// statuslineCachePath is the cache file of one ID under clientconfig.CacheDir,
// "" when there is no cache directory (no cache then).
func statuslineCachePath(id string) string {
	dir := clientconfig.CacheDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "statusline-"+id+".json")
}

// readStatuslineCache returns the cached result at path when the file is an
// entry for id that is younger than statuslineCacheTTL at now. Every other
// file — missing, unreadable, oversized, not JSON, another ID, stamped in the
// future, expired, a negative count — is no cache, never an error.
func readStatuslineCache(path, id string, now time.Time) (*statusCache, bool) {
	if path == "" {
		return nil, false
	}
	f, err := os.Open(path) //nolint:gosec // G304: the path is CacheDir plus a hex digest, built here, not input
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, statuslineCacheMaxBytes+1))
	if err != nil || len(data) > statuslineCacheMaxBytes {
		return nil, false
	}
	var e statuslineCacheEntry
	if json.Unmarshal(data, &e) != nil || e.ID != id {
		return nil, false
	}
	if age := now.Sub(e.FetchedAt); age < 0 || age >= statuslineCacheTTL {
		return nil, false
	}
	if e.Backend.HasBlockCount && e.Backend.BlockCount < 0 {
		return nil, false
	}
	return &e.Backend, true
}

// writeStatuslineCache stores b as the entry for id at path: in a private
// directory (0700), as a private file (0600), atomically — a temp file in the
// same directory renamed over the old entry, so a concurrent statusline of
// another session reads the old entry or the new one, never half of one.
func writeStatuslineCache(path, id string, b *statusCache, now time.Time) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(statuslineCacheEntry{ID: id, FetchedAt: now.UTC(), Backend: *b})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".statusline-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

package handler

// Issue #66: a write POST /api/store refuses after the body parsed gets ONE
// WARN line naming the key (api_key_id), the verdict (code, status) and the
// request (request_id, category, scope) — and nothing of the block itself.
//
// RED on b52b340a: HandleStore answered every rejection through
// writeJSONReject alone, so the capture below saw no line at all; the only
// server-side trace was the request logger's status per path.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// rejectLogRecord is one captured slog record, attributes resolved.
type rejectLogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// text renders the whole record — message, every key and every value — so a
// negative check sees everything a log sink would.
func (r rejectLogRecord) text() string {
	var b strings.Builder
	b.WriteString(r.msg)
	for k, v := range r.attrs {
		fmt.Fprintf(&b, " %s=%v", k, v)
	}
	return b.String()
}

// rejectLogCapture is a slog.Handler that keeps every record. Mutex-guarded:
// HandleStore's success path logs from goroutines, and a record may arrive
// while the test reads.
type rejectLogCapture struct {
	mu   sync.Mutex
	recs []rejectLogRecord
}

func (c *rejectLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *rejectLogCapture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *rejectLogCapture) WithGroup(string) slog.Handler            { return c }
func (c *rejectLogCapture) Handle(_ context.Context, r slog.Record) error {
	rec := rejectLogRecord{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Resolve().Any()
		return true
	})
	c.mu.Lock()
	c.recs = append(c.recs, rec)
	c.mu.Unlock()
	return nil
}

// withMsg returns the captured records with exactly this message.
func (c *rejectLogCapture) withMsg(msg string) []rejectLogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []rejectLogRecord
	for _, r := range c.recs {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

func (c *rejectLogCapture) all() []rejectLogRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rejectLogRecord(nil), c.recs...)
}

// installRejectLogCapture routes the default logger into a fresh capture for
// the rest of the test. The handler package runs no parallel tests, so the
// global swap cannot steal another test's lines.
func installRejectLogCapture(t *testing.T) *rejectLogCapture {
	t.Helper()
	c := &rejectLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(c))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

const (
	rejectLogTestKey   = "01990000-0000-7000-8000-000000000066"
	rejectLogTestScope = "private"
	rejectLogMsg       = "store: write rejected"
)

// rejectLogAR is a valid knowledge key of the home scope "private" and nothing
// else — enough for every gate before the rate limit, which stays off.
func rejectLogAR(keyID string) *auth.AuthResult {
	return &auth.AuthResult{ApiKeyID: keyID, IsValid: true, HomeScope: rejectLogTestScope,
		ReadScopes: []string{rejectLogTestScope}}
}

// storeRejectedAs posts body to h.HandleStore as ar (nil = unauthenticated)
// under request id reqID and returns the status and the envelope's code.
func storeRejectedAs(t *testing.T, h *StoreHandler, ar *auth.AuthResult, reqID string, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	ctx := context.WithValue(context.Background(), requestIDKey, reqID)
	if ar != nil {
		ctx = context.WithValue(ctx, authResultKey, ar)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.HandleStore(rec, req)
	var resp struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp.Code
}

// rejectLogHandler is a StoreHandler with no pool and no registry: every gate
// up to the scope gate decides without the database, and RateLimitWrite 0
// keeps the one gate that would query it off. Default sensitivity public so
// the sensitivity gate never answers for a body that names none.
func rejectLogHandler() *StoreHandler {
	return NewStoreHandler(nil, staticConfigStore{cfg: &config.Config{
		Pool: config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
	}}, nil)
}

// The block's own strings. None of them may reach the log: the title and the
// content are the block, the metadata and the tags travel with it, and the
// line names the write by category and scope only.
const (
	rejectLogSecretTitle   = "TITLE-must-not-be-logged-66"
	rejectLogSecretContent = "CONTENT-must-not-be-logged-66"
	rejectLogSecretMeta    = "METADATA-must-not-be-logged-66"
	rejectLogSecretTag     = "TAG-must-not-be-logged-66"
)

func TestStoreWriteRejectionLogged(t *testing.T) {
	oversized := strings.Repeat("ö", 150) // 300 bytes: over the 100-byte category cap

	cases := []struct {
		name         string
		category     string
		scope        string
		content      string
		wantStatus   int
		wantCode     string
		wantCategory string // as logged
		wantScope    string // as logged
	}{
		{
			// The issue's own example: an agent writing into a reserved
			// category loses the block, and the server must name the key.
			name: "reserved_category", category: "session-insights", scope: rejectLogTestScope,
			content: rejectLogSecretContent, wantStatus: http.StatusForbidden, wantCode: "reserved_category",
			wantCategory: "session-insights", wantScope: rejectLogTestScope,
		},
		{
			// A size_cap verdict on the CATEGORY: the logged value is cut to
			// the cap (100 runes), so the refused value cannot size the line.
			name: "size_cap_category", category: oversized, scope: rejectLogTestScope,
			content: rejectLogSecretContent, wantStatus: http.StatusRequestEntityTooLarge, wantCode: "size_cap",
			wantCategory: strings.Repeat("ö", 100), // blockSizeLimit's category cap, in runes
			wantScope:    rejectLogTestScope,
		},
		{
			// The scope field is logged as REQUESTED — here a scope the key may
			// not write, which is exactly what the operator needs to see.
			name: "scope_denied", category: "learnings", scope: "not-a-scope-of-this-key",
			content: rejectLogSecretContent, wantStatus: http.StatusForbidden, wantCode: "scope_denied",
			wantCategory: "learnings", wantScope: "not-a-scope-of-this-key",
		},
		{
			// The scope has no size gate of its own (the body cap is 1 MiB), so
			// the cap on the line is the only bound: the same 100 runes.
			name: "scope_denied_oversized", category: "learnings", scope: oversized,
			content: rejectLogSecretContent, wantStatus: http.StatusForbidden, wantCode: "scope_denied",
			wantCategory: "learnings", wantScope: strings.Repeat("ö", 100),
		},
		{
			name: "missing_fields", category: "learnings", scope: "",
			content: "", wantStatus: http.StatusBadRequest, wantCode: "missing_fields",
			wantCategory: "learnings", wantScope: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			reqID := "req-66-" + tc.name

			status, code := storeRejectedAs(t, rejectLogHandler(), rejectLogAR(rejectLogTestKey), reqID, map[string]any{
				"category": tc.category,
				"title":    rejectLogSecretTitle,
				"content":  tc.content,
				"scope":    tc.scope,
				"tags":     []string{rejectLogSecretTag},
				"metadata": map[string]any{"note": rejectLogSecretMeta},
			})
			if status != tc.wantStatus || code != tc.wantCode {
				t.Fatalf("answer = %d %q, want %d %q — the probe no longer reaches the gate it names",
					status, code, tc.wantStatus, tc.wantCode)
			}

			lines := logs.withMsg(rejectLogMsg)
			if len(lines) != 1 {
				t.Fatalf("%q lines = %d, want 1 — a %d %s refusal leaves no trace naming the key (all records: %v)",
					rejectLogMsg, len(lines), status, code, logs.all())
			}
			line := lines[0]
			if line.level != slog.LevelWarn {
				t.Errorf("level = %v, want WARN", line.level)
			}
			want := map[string]any{
				"api_key_id": rejectLogTestKey,
				"code":       tc.wantCode,
				"status":     int64(tc.wantStatus),
				"request_id": reqID,
				"category":   tc.wantCategory,
				"scope":      tc.wantScope,
			}
			for k, v := range want {
				if got, ok := line.attrs[k]; !ok || got != v {
					t.Errorf("attr %s = %v (present %v), want %v", k, got, ok, v)
				}
			}

			// Negative half: nothing of the block reaches ANY record of this
			// request — not the rejection line, not another one.
			for _, r := range logs.all() {
				txt := r.text()
				for _, secret := range []string{rejectLogSecretTitle, rejectLogSecretContent, rejectLogSecretMeta, rejectLogSecretTag} {
					if strings.Contains(txt, secret) {
						t.Errorf("record %q carries block data %q: %s", r.msg, secret, txt)
					}
				}
			}
			// The key set is CLOSED: an attribute beyond these seven — the
			// rejection's prose under any name, a title, a hash — fails here
			// whatever it is called.
			allowed := map[string]bool{"api_key_id": true, "code": true, "status": true, "request_id": true,
				"category": true, "scope": true, "suppressed": true}
			for k := range line.attrs {
				if !allowed[k] {
					t.Errorf("rejection line carries attr %q — the line names the write, never its payload or prose", k)
				}
			}
			if len(line.attrs) != len(allowed) {
				t.Errorf("rejection line attrs = %v, want exactly the keys %v", line.attrs, allowed)
			}
		})
	}
}

// Before the key is known there is nothing to attribute: an unauthenticated
// write keeps the request logger's line and gets no rejection line.
func TestStoreUnauthenticatedRejectionNotLogged(t *testing.T) {
	logs := installRejectLogCapture(t)
	status, code := storeRejectedAs(t, rejectLogHandler(), nil, "req-66-unauth", map[string]any{
		"category": "learnings", "title": rejectLogSecretTitle, "content": rejectLogSecretContent,
	})
	if status != http.StatusUnauthorized || code != "unauthorized" {
		t.Fatalf("answer = %d %q, want 401 unauthorized", status, code)
	}
	if lines := logs.withMsg(rejectLogMsg); len(lines) != 0 {
		t.Errorf("%q lines = %d for an unauthenticated write, want 0: %v", rejectLogMsg, len(lines), lines)
	}
}

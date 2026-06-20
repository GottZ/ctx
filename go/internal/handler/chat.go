// Package handler — web-chat HTTP surface (F6-C4/G37).
//
// POST /api/chat/stream runs ONE turn through the headless chat.Engine and
// streams its events as SSE; the session routes (list / detail / delete) are
// the read-lastig GET/DELETE companions. Two enforcement seams live here, not
// in the engine: the per-home_scope turn semaphore (429 before any stream byte,
// §3.3) and the read-only ctx_query delegation (chatQueryRunner) that runs the
// retrieval pipeline under the SESSION's read_scopes snapshot.
//
// Header timing: the stream header is committed LAZILY by the chatSink (sse.go)
// on the first event, so a turn that loses the busy claim (ErrTurnBusy, no event
// emitted) still gets a clean 409 JSON instead of a half-open 200 stream.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/store"
)

// chatQueryRunner implements chat.QueryRunner by delegating to the real
// /api/query handler via a responseRecorder — the MCP pattern (mcp.go) — so the
// tool inherits the full pipeline (translate → embed → RRF → graph → rerank,
// scope filter, scheduler signal, access log). The injected AuthResult carries
// the REAL key id (rate-limit + access-log attribution, §3.7) but the SESSION's
// read_scopes snapshot drives the scope filter (deterministic + auditable, not
// the continuing key's possibly-broader set, §3.1). Body forces synthesize:false
// (retrieval-only — no agent-in-agent, ~10s less latency) + include_content:true
// (the G36 flag) so the chat model synthesizes from the snippets itself.
type chatQueryRunner struct {
	handler http.Handler // queryHandler.HandleQuery, scheduler-wrapped
}

func (q *chatQueryRunner) RunQuery(ctx context.Context, readScopes []string, query string, limit int) (chat.QueryResult, error) {
	apiKeyID := ""
	if ar := AuthResultFromContext(ctx); ar != nil {
		apiKeyID = ar.ApiKeyID
	}
	injected := &auth.AuthResult{ApiKeyID: apiKeyID, ReadScopes: readScopes, IsValid: true}
	ctx = context.WithValue(ctx, authResultKey, injected)

	body, _ := json.Marshal(map[string]any{
		"query": query, "limit": limit, "synthesize": false, "include_content": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/api/query", io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return chat.QueryResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	rec := &responseRecorder{headers: make(http.Header)}
	q.handler.ServeHTTP(rec, req)

	var resp struct {
		Confidence string `json:"confidence"`
		Error      string `json:"error"`
		Sources    []struct {
			ID       string  `json:"id"`
			Title    string  `json:"title"`
			Category string  `json:"category"`
			Score    float64 `json:"score"`
			AgeDays  int     `json:"age_days"`
			Content  string  `json:"content"`
		} `json:"sources"`
	}
	// The recorded body may carry leading heartbeat whitespace (mcp.go note);
	// RFC-8259 tolerates it, so the decode stays correct.
	if err := json.Unmarshal(rec.body, &resp); err != nil {
		return chat.QueryResult{}, err
	}
	if resp.Error != "" {
		return chat.QueryResult{}, errors.New(resp.Error)
	}
	blocks := make([]chat.QueryBlock, len(resp.Sources))
	for i, s := range resp.Sources {
		blocks[i] = chat.QueryBlock{
			ID: s.ID, Title: s.Title, Category: s.Category,
			Score: s.Score, AgeDays: s.AgeDays, Content: s.Content,
		}
	}
	return chat.QueryResult{Confidence: resp.Confidence, Blocks: blocks}, nil
}

// ChatHandler serves the web-chat routes. The pool-backed provider and the
// query runner are stateless and built once; the engine + executor are rebuilt
// per turn from a fresh config snapshot so the hot webchat.* knobs apply to the
// next turn without a restart.
type ChatHandler struct {
	pool        *pgxpool.Pool
	cfg         ConfigStore
	provider    chat.BackendProvider
	queryRunner chat.QueryRunner

	mu       sync.Mutex
	inflight map[string]int // home_scope → running turns (the §3.3 semaphore)
}

// NewChatHandler wires the chat handler. queryHandler is the /api/query handler
// (scheduler-wrapped) the ctx_query tool delegates to.
func NewChatHandler(pool *pgxpool.Pool, cfg ConfigStore, backendPool *backends.Pool, queryHandler http.Handler) *ChatHandler {
	// GamingState stays on the tenant-blind Snapshot() (NOT SnapshotForRequest):
	// gaming.active and disabled_backends are global-only keys (03-W2/T28 tags +
	// §10.5) — a tenant override is dropped before the overlay build, so the
	// per-tenant generation carries the _global values here regardless. The
	// closure is also built at construction time with no request context to
	// resolve a tenant from. The per-tenant request surface (WebChat etc.) flows
	// through SnapshotForRequest in HandleStream instead.
	provider := chat.NewPoolProvider(backendPool, func() backends.GamingState {
		return cfg.Snapshot().GamingState()
	})
	return &ChatHandler{
		pool:        pool,
		cfg:         cfg,
		provider:    provider,
		queryRunner: &chatQueryRunner{handler: queryHandler},
		inflight:    map[string]int{},
	}
}

// acquire reserves a turn slot for home_scope; ok=false means the per-scope cap
// (webchat.concurrent_turns) is reached → the handler answers 429 before any
// stream byte (R1, multi-tenant fairness on the single llama.cpp slot). The map
// holds only scopes with inflight > 0.
func (h *ChatHandler) acquire(scope string, limit int) bool {
	if limit <= 0 {
		limit = 1
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[scope] >= limit {
		return false
	}
	h.inflight[scope]++
	return true
}

func (h *ChatHandler) release(scope string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.inflight[scope] <= 1 {
		delete(h.inflight, scope)
		return
	}
	h.inflight[scope]--
}

// streamRequest is the POST /api/chat/stream body (§3.3).
type streamRequest struct {
	SessionID    string `json:"session_id"`   // empty = create a new session
	Message      string `json:"message"`
	Sensitivity  string `json:"sensitivity"`  // optional; request > setting > default
	ToolsEnabled *bool  `json:"tools_enabled"` // nil = default true
	MaxTokens    int    `json:"max_tokens"`   // optional; clamped to webchat.max_tokens
}

// HandleStream runs one turn and streams its events. Pre-stream failures
// (feature off, bad body, unknown/foreign session, busy, scope semaphore) are
// plain JSON 4xx/404; once the engine emits its first event the header is
// committed and any later failure is an error EVENT, not a status code (§3.3).
func (h *ChatHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	// MT 06-C5: the per-turn config resolves per-tenant from the request
	// context — WebChat knobs (ConcurrentTurns, MaxTokens, SessionRetention)
	// honor the caller tenant's overrides; the §3.3 semaphore is already
	// home_scope-keyed (chat.go inflight map).
	cfg := h.cfg.SnapshotForRequest(r.Context())
	wc := cfg.WebChat
	if !wc.Enabled {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "web chat is disabled"})
		return
	}

	var req streamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "message is required"})
		return
	}

	// Sensitivity precedence: request > settings default > code default (§2.3).
	reqSens := cfg.Pool.DefaultQuerySensitivity
	if req.Sensitivity != "" {
		s := backends.Sensitivity(req.Sensitivity)
		if !backends.ValidSensitivity(s) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid sensitivity (credentials|personal|internal|public)"})
			return
		}
		reqSens = s
	}
	toolsEnabled := true
	if req.ToolsEnabled != nil {
		toolsEnabled = *req.ToolsEnabled
	}

	// Load (read_scopes-gated, §3.1) or create the session before committing the
	// stream — a miss is 404, indistinguishable from non-existent.
	var sess *store.ChatSession
	var err error
	if req.SessionID != "" {
		sess, err = store.GetSession(r.Context(), h.pool, req.SessionID, ar.ReadScopes)
		if errors.Is(err, store.ErrSessionNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "session not found"})
			return
		}
	} else {
		sess, err = store.CreateSession(r.Context(), h.pool, ar.HomeScope, ar.ReadScopes, ar.ApiKeyID, "")
	}
	if err != nil {
		slog.Error("chat: load/create session", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	// Per-scope turn semaphore — 429 before the stream starts (§3.3).
	if !h.acquire(sess.Scope, wc.ConcurrentTurns) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "a turn is already running for this scope"})
		return
	}
	defer h.release(sess.Scope)

	maxTokens := wc.MaxTokens
	if req.MaxTokens > 0 && req.MaxTokens < maxTokens {
		maxTokens = req.MaxTokens
	}
	engineCfg := chat.Config{
		MaxIterations:      wc.MaxIterations,
		MaxTokens:          maxTokens,
		CompletionBudget:   wc.CompletionBudget,
		ToolResultMaxChars: wc.ToolResultMaxChars,
		HistoryBudgetChars: wc.HistoryBudgetChars,
		LLMTimeout:         wc.LLMTimeout,
		Timezone:           tzName(cfg.Query.Timezone),
	}
	exec := chat.NewExecutor(h.pool, h.queryRunner, wc.ToolResultMaxChars)
	engine := chat.NewEngine(h.pool, h.provider, exec, engineCfg)

	sink := newChatSink(w)
	hbCtx, cancelHB := context.WithCancel(r.Context())
	defer cancelHB()
	go sink.runHeartbeat(hbCtx)

	err = engine.RunTurn(r.Context(), ar, sess, req.Message, reqSens, toolsEnabled, sink)
	cancelHB()

	switch {
	case err == nil:
		// Terminal done/error already emitted through the sink — nothing to add.
	case errors.Is(err, chat.ErrTurnBusy) && !sink.committed():
		// Busy claim lost before any event (engine guarantees no event first) —
		// the stream never started, so a clean 409 is still possible.
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": "a turn is already running for this session"})
	case !sink.committed():
		slog.Error("chat: turn failed before stream start", "session", sess.ID, "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
	default:
		// Stream already open: the client vanished or a mid-stream persist
		// failed. The laundered error (no raw URL) already went to slog inside
		// the engine; the 200 status is spent, so we cannot send more.
		slog.Warn("chat: turn ended with error after stream start", "session", sess.ID, "error", err)
	}
}

// chatSessionView is the session metadata shared by the list and detail
// responses. It deliberately omits read_scopes + created_by (internal) and
// keeps max_sensitivity (the UI's full-trust-only banner, §3.9).
type chatSessionView struct {
	ID             string               `json:"id"`
	Title          string               `json:"title"`
	Scope          string               `json:"scope"`
	MaxSensitivity backends.Sensitivity `json:"max_sensitivity"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
}

func viewOf(s store.ChatSession) chatSessionView {
	return chatSessionView{
		ID: s.ID, Title: s.Title, Scope: s.Scope,
		MaxSensitivity: s.MaxSensitivity, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

// chatSessionListItem adds the message_count navigation anchor to the view.
type chatSessionListItem struct {
	chatSessionView
	MessageCount int `json:"message_count"`
}

// chatMessageView is the wire shape of one message in the detail response. It
// mirrors store.ChatMessage but carries tool_calls as json.RawMessage so the
// stored JSONB embeds RAW — store.ChatMessage.ToolCalls is []byte, which
// json.Marshal would base64-encode (unusable by the SPA). id/session_id are
// dropped (internal); seq is the stable handle.
type chatMessageView struct {
	Seq              int                  `json:"seq"`
	Role             string               `json:"role"`
	Content          string               `json:"content"`
	Sensitivity      backends.Sensitivity `json:"sensitivity"`
	ToolCalls        json.RawMessage      `json:"tool_calls,omitempty"`
	ToolCallID       string               `json:"tool_call_id,omitempty"`
	ToolName         string               `json:"tool_name,omitempty"`
	Backend          string               `json:"backend,omitempty"`
	Model            string               `json:"model,omitempty"`
	PromptTokens     *int                 `json:"prompt_tokens,omitempty"`
	CompletionTokens *int                 `json:"completion_tokens,omitempty"`
	DurationMs       *int                 `json:"duration_ms,omitempty"`
	Metadata         map[string]any       `json:"metadata,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
}

func messageViewsOf(msgs []store.ChatMessage) []chatMessageView {
	out := make([]chatMessageView, len(msgs))
	for i, m := range msgs {
		out[i] = chatMessageView{
			Seq: m.Seq, Role: m.Role, Content: m.Content, Sensitivity: m.Sensitivity,
			ToolCalls:  json.RawMessage(m.ToolCalls), // raw JSONB, not base64
			ToolCallID: m.ToolCallID, ToolName: m.ToolName,
			Backend: m.Backend, Model: m.Model,
			PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens, DurationMs: m.DurationMs,
			Metadata: m.Metadata, CreatedAt: m.CreatedAt,
		}
	}
	return out
}

// HandleListSessions lists the caller's home-scope sessions (metadata only, no
// content), newest first (§3.3). Scope-owned, NOT read-scope filtered.
func (h *ChatHandler) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	sessions, err := store.ListSessions(r.Context(), h.pool, ar.HomeScope, limit)
	if err != nil {
		slog.Error("chat: list sessions", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	ids := make([]string, len(sessions))
	for i := range sessions {
		ids[i] = sessions[i].ID
	}
	counts, err := store.MessageCounts(r.Context(), h.pool, ids)
	if err != nil {
		slog.Error("chat: message counts", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	items := make([]chatSessionListItem, len(sessions))
	for i := range sessions {
		items[i] = chatSessionListItem{chatSessionView: viewOf(sessions[i]), MessageCount: counts[sessions[i].ID]}
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "sessions": items})
}

// HandleGetSession returns one session + its messages (full tool-result
// contents). Gated by read_scopes ⊆ caller (§3.1) → 404 on miss. Pagination is
// additive: ?after_seq=N&limit=M (default = all).
func (h *ChatHandler) HandleGetSession(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	id := chi.URLParam(r, "id")
	sess, err := store.GetSession(r.Context(), h.pool, id, ar.ReadScopes)
	if errors.Is(err, store.ErrSessionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "session not found"})
		return
	}
	if err != nil {
		slog.Error("chat: get session", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	afterSeq := atoiDefault(r.URL.Query().Get("after_seq"), 0)
	limit := atoiDefault(r.URL.Query().Get("limit"), 0)
	msgs, err := store.ListMessages(r.Context(), h.pool, sess.ID, afterSeq, limit)
	if err != nil {
		slog.Error("chat: list messages", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "session": viewOf(*sess), "messages": messageViewsOf(msgs)})
}

// HandleDeleteSession hard-deletes a session (messages cascade) within the
// caller's home scope (§3.3) → 404 on miss. Complete because llmlog logs
// web-chat metadata-only (§3.6) — no shadow body corpus to mop up.
func (h *ChatHandler) HandleDeleteSession(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	id := chi.URLParam(r, "id")
	err := store.DeleteSession(r.Context(), h.pool, id, ar.HomeScope)
	if errors.Is(err, store.ErrSessionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "session not found"})
		return
	}
	if err != nil {
		slog.Error("chat: delete session", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// atoiDefault parses a query-param int, falling back to def on empty/garbage.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// tzName renders a *time.Location for chat.Config.Timezone (a string the engine
// re-loads via time.LoadLocation). nil (unset query.timezone) → "" → the engine
// defaults to UTC.
func tzName(loc *time.Location) string {
	if loc == nil {
		return ""
	}
	return loc.String()
}

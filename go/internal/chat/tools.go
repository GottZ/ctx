// Package chat — server-side web-chat harness (F6-C3/G36).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Source: https://github.com/GottZ/ctx
//
// tools.go is the read-only tool registry + executor. Every tool runs under the
// session's read_scopes snapshot (the handler passes it in); ctx_query delegates
// to the QueryHandler through an injected QueryRunner (the chat package never
// imports handler — that would be an import cycle). Each result is annotated
// with max(sensitivity of the blocks it carries) so the engine can raise the
// session HWM before the next model call (design 06 §2.3/§3.7).
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// QueryRunner runs a retrieval-only query under the given read-scopes and
// returns the ranked blocks. The handler (C4) implements it by delegating to
// the QueryHandler with an injected AuthResult (read_scopes = the session
// snapshot) via a responseRecorder; tests inject a scriptable fake.
type QueryRunner interface {
	RunQuery(ctx context.Context, readScopes []string, query string, limit int) (QueryResult, error)
}

// QueryResult is the retrieval-only outcome the QueryRunner hands back.
type QueryResult struct {
	Confidence string
	Blocks     []QueryBlock
}

// QueryBlock is one ranked block from a ctx_query delegation.
type QueryBlock struct {
	ID       string
	Title    string
	Category string
	Score    float64
	AgeDays  int
	Content  string
}

// EventBlock is the slim per-block reference carried in the tool_result SSE
// event (never the full content — that lives in the session API, §3.5).
type EventBlock struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Category string  `json:"category"`
	Score    float64 `json:"score,omitempty"`
}

// ToolOutcome is one executed tool's result: the JSON string fed back to the
// model, its content sensitivity, and the metadata the engine forwards as a
// tool_result event. A tool error is OK=false with an {"error":…} content — it
// never aborts the turn (the model self-corrects, §3.7).
type ToolOutcome struct {
	Content     string
	Sensitivity backends.Sensitivity
	OK          bool
	DurationMs  int64
	Chars       int
	Truncated   bool
	Summary     string
	Blocks      []EventBlock
}

// Executor runs the read-only tools. maxContentWindow is the ctx_get content
// window (CTX_WEBCHAT_TOOL_RESULT_MAX_CHARS) — a larger block is paged via the
// offset marker so it stays fully readable (§3.7).
type Executor struct {
	pool             *pgxpool.Pool
	query            QueryRunner
	maxContentWindow int
}

// NewExecutor builds the tool executor. maxContentWindow <= 0 falls back to 8000.
func NewExecutor(pool *pgxpool.Pool, query QueryRunner, maxContentWindow int) *Executor {
	if maxContentWindow <= 0 {
		maxContentWindow = 8000
	}
	return &Executor{pool: pool, query: query, maxContentWindow: maxContentWindow}
}

// Defs returns the tool schemas offered to the model.
func (ex *Executor) Defs() []llm.ToolDef { return toolDefs }

// Run dispatches one tool call. Unknown tool / bad arguments / not-found /
// ambiguous-prefix / DB error all return OK=false outcomes — only the engine's
// transport/context errors end a turn.
func (ex *Executor) Run(ctx context.Context, readScopes []string, apiKeyID string, call llm.ToolCall) ToolOutcome {
	start := time.Now()
	var out ToolOutcome
	switch call.Function.Name {
	case "ctx_query":
		out = ex.runQuery(ctx, readScopes, call.Function.Arguments)
	case "ctx_search":
		out = ex.runSearch(ctx, readScopes, call.Function.Arguments)
	case "ctx_get":
		out = ex.runGet(ctx, readScopes, apiKeyID, call.Function.Arguments)
	case "ctx_recent":
		out = ex.runRecent(ctx, readScopes, call.Function.Arguments)
	default:
		out = errOutcome("unknown tool: " + call.Function.Name)
	}
	out.DurationMs = time.Since(start).Milliseconds()
	out.Chars = len(out.Content)
	return out
}

func (ex *Executor) runQuery(ctx context.Context, readScopes []string, raw json.RawMessage) ToolOutcome {
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errOutcome("invalid arguments: " + err.Error())
	}
	if a.Query == "" {
		return errOutcome("query is required")
	}
	limit := clamp(a.Limit, 5, 1, 10)
	res, err := ex.query.RunQuery(ctx, readScopes, a.Query, limit)
	if err != nil {
		return errOutcome("query failed") // laundered — raw error goes to slog upstream
	}
	type qb struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Category string  `json:"category"`
		Score    float64 `json:"score"`
		AgeDays  int     `json:"age_days"`
		Content  string  `json:"content,omitempty"`
	}
	blocks := make([]qb, len(res.Blocks))
	ids := make([]string, len(res.Blocks))
	events := make([]EventBlock, len(res.Blocks))
	for i, b := range res.Blocks {
		blocks[i] = qb(b)
		ids[i] = b.ID
		events[i] = EventBlock{ID: b.ID, Title: b.Title, Category: b.Category, Score: b.Score}
	}
	content := mustJSON(map[string]any{"count": len(blocks), "confidence": res.Confidence, "blocks": blocks})
	return ToolOutcome{
		Content:     content,
		Sensitivity: ex.annotate(ctx, ids),
		OK:          true,
		Summary:     fmt.Sprintf("%d blocks", len(blocks)),
		Blocks:      events,
	}
}

func (ex *Executor) runSearch(ctx context.Context, readScopes []string, raw json.RawMessage) ToolOutcome {
	var a struct {
		Query    string   `json:"query"`
		Category string   `json:"category"`
		Tags     []string `json:"tags"`
		Limit    int      `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errOutcome("invalid arguments: " + err.Error())
	}
	limit := clamp(a.Limit, 10, 1, 20)
	// grantedBlockIDs nil (T40a): the chat-tool read path is NOT live-wired for
	// block grants in T40a (its grant resolution is a later wave) — nil ⇒ no-op.
	previews, err := store.SearchBlocks(ctx, ex.pool, a.Query, readScopes, a.Category, a.Tags, limit, true, nil, nil)
	if err != nil {
		return errOutcome("search failed")
	}
	type sb struct {
		ID        string   `json:"id"`
		Title     string   `json:"title"`
		Category  string   `json:"category"`
		Tags      []string `json:"tags"`
		Preview   string   `json:"preview"`
		UpdatedAt string   `json:"updated_at"`
	}
	blocks := make([]sb, len(previews))
	ids := make([]string, len(previews))
	events := make([]EventBlock, len(previews))
	for i, p := range previews {
		blocks[i] = sb{ID: p.ID, Title: p.Title, Category: p.Category, Tags: p.Tags, Preview: p.ContentPreview, UpdatedAt: p.UpdatedAt.Format(time.RFC3339)}
		ids[i] = p.ID
		events[i] = EventBlock{ID: p.ID, Title: p.Title, Category: p.Category}
	}
	content := mustJSON(map[string]any{"count": len(blocks), "blocks": blocks})
	return ToolOutcome{
		Content:     content,
		Sensitivity: ex.annotate(ctx, ids),
		OK:          true,
		Summary:     fmt.Sprintf("%d blocks", len(blocks)),
		Blocks:      events,
	}
}

func (ex *Executor) runGet(ctx context.Context, readScopes []string, apiKeyID string, raw json.RawMessage) ToolOutcome {
	var a struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errOutcome("invalid arguments: " + err.Error())
	}
	if a.ID == "" {
		return errOutcome("id is required")
	}
	// grantedBlockIDs nil (T40a): chat-tool grant resolution is a later wave.
	resolvedID, _, err := store.ResolveBlockID(ctx, ex.pool, a.ID, readScopes, nil)
	if err != nil {
		return errOutcome(err.Error()) // ambiguous prefix / too-short — safe to surface (no URL)
	}
	if resolvedID == "" {
		return errOutcome("block not found")
	}
	block, err := store.GetBlock(ctx, ex.pool, resolvedID, readScopes, nil)
	if err != nil || block == nil {
		return errOutcome("block not found") // scope-miss laundered to not-found (no oracle)
	}
	// Best-effort access log; a failure must not break the tool.
	_ = store.LogAccess(ctx, ex.pool, apiKeyID, resolvedID, "chat-tool")

	window, truncated, nextOffset := windowContent(block.Content, a.Offset, ex.maxContentWindow)
	result := map[string]any{
		"id":         block.ID,
		"title":      block.Title,
		"category":   block.Category,
		"tags":       block.Tags,
		"scope":      block.Scope,
		"created_at": block.CreatedAt.Format(time.RFC3339),
		"updated_at": block.UpdatedAt.Format(time.RFC3339),
		"content":    window,
	}
	if truncated {
		result["next_offset"] = nextOffset
	}
	return ToolOutcome{
		Content:     mustJSON(result),
		Sensitivity: ex.annotate(ctx, []string{resolvedID}),
		OK:          true,
		Truncated:   truncated,
		Summary:     block.Title,
		Blocks:      []EventBlock{{ID: block.ID, Title: block.Title, Category: block.Category}},
	}
}

func (ex *Executor) runRecent(ctx context.Context, readScopes []string, raw json.RawMessage) ToolOutcome {
	var a struct {
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return errOutcome("invalid arguments: " + err.Error())
	}
	previews, err := store.RecentBlocks(ctx, ex.pool, readScopes, a.Category, a.Limit)
	if err != nil {
		return errOutcome("recent failed")
	}
	type rb struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Category  string `json:"category"`
		Preview   string `json:"preview"`
		UpdatedAt string `json:"updated_at"`
	}
	blocks := make([]rb, len(previews))
	ids := make([]string, len(previews))
	events := make([]EventBlock, len(previews))
	for i, p := range previews {
		blocks[i] = rb{ID: p.ID, Title: p.Title, Category: p.Category, Preview: p.ContentPreview, UpdatedAt: p.UpdatedAt.Format(time.RFC3339)}
		ids[i] = p.ID
		events[i] = EventBlock{ID: p.ID, Title: p.Title, Category: p.Category}
	}
	content := mustJSON(map[string]any{"count": len(blocks), "blocks": blocks})
	return ToolOutcome{
		Content:     content,
		Sensitivity: ex.annotate(ctx, ids),
		OK:          true,
		Summary:     fmt.Sprintf("%d blocks", len(blocks)),
		Blocks:      events,
	}
}

// annotate folds max(sensitivity) over the blocks a result carries. No blocks
// (empty result, error) → public (nothing to protect). A block whose row went
// missing between retrieval and lookup → credentials, fail-closed (a real block
// we could not classify, §2.3a); a lookup error → credentials for the whole set.
func (ex *Executor) annotate(ctx context.Context, ids []string) backends.Sensitivity {
	if len(ids) == 0 {
		return backends.SensPublic
	}
	m, err := store.FetchSensitivities(ctx, ex.pool, ids)
	if err != nil {
		return backends.SensCredentials
	}
	senses := make([]backends.Sensitivity, 0, len(ids))
	for _, id := range ids {
		if bs, ok := m[id]; ok {
			senses = append(senses, bs.Sensitivity)
		} else {
			senses = append(senses, backends.SensCredentials)
		}
	}
	return backends.MaxSensitivity(senses...)
}

// windowContent returns the rune-safe content window starting at offset, the
// truncation flag and the next offset to page from (0 when complete).
func windowContent(content string, offset, maxChars int) (window string, truncated bool, nextOffset int) {
	if offset < 0 {
		offset = 0
	}
	runes := []rune(content)
	if offset >= len(runes) {
		return "", false, 0
	}
	end := offset + maxChars
	if end >= len(runes) {
		return string(runes[offset:]), false, 0
	}
	return string(runes[offset:end]) + fmt.Sprintf("\n…[truncated, continue with offset=%d]", end), true, end
}

func errOutcome(msg string) ToolOutcome {
	return ToolOutcome{
		Content:     mustJSON(map[string]string{"error": msg}),
		Sensitivity: backends.SensPublic,
		OK:          false,
		Summary:     msg,
	}
}

func clamp(v, def, lo, hi int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// The inputs are all string/number/[]string maps — Marshal cannot fail;
		// fall back to a valid error object rather than emitting broken JSON.
		return `{"error":"failed to encode tool result"}`
	}
	return string(b)
}

// toolDefs are the four read-only tools offered to the chat model (design §3.7).
// Descriptions push full UUIDs (ctx_get prefix resolution is a full-scan at 1M
// blocks, R12) and the offset paging for large blocks.
var toolDefs = []llm.ToolDef{
	{Type: "function", Function: llm.ToolDefFunction{
		Name:        "ctx_query",
		Description: "Hybrid semantic+fulltext retrieval over the knowledge store. Returns the top blocks with content snippets, ranked. Use for any content question. Query may be in any language.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string","description":"natural-language question"},` +
			`"limit":{"type":"integer","minimum":1,"maximum":10,"default":5}},` +
			`"required":["query"]}`),
	}},
	{Type: "function", Function: llm.ToolDefFunction{
		Name:        "ctx_search",
		Description: "Lightweight listing/browse of blocks by keywords, category and/or tags. Returns titles and 200-char previews — use ctx_get for full content.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"query":{"type":"string"},` +
			`"category":{"type":"string"},` +
			`"tags":{"type":"array","items":{"type":"string"}},` +
			`"limit":{"type":"integer","minimum":1,"maximum":20,"default":10}}}`),
	}},
	{Type: "function", Function: llm.ToolDefFunction{
		Name:        "ctx_get",
		Description: "Fetch one block's full content. Pass the full 36-char UUID (ids arrive complete in ctx_query/ctx_search results); unique prefixes of >=8 chars work but are slower. For content beyond the returned window, call again with the offset given in the truncation marker.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string"},` +
			`"offset":{"type":"integer","minimum":0,"default":0}},` +
			`"required":["id"]}`),
	}},
	{Type: "function", Function: llm.ToolDefFunction{
		Name:        "ctx_recent",
		Description: "List recently created or updated blocks (titles + previews), newest first. Use for temporal questions like 'what did I save this week'. Optional category filter.",
		Parameters: json.RawMessage(`{"type":"object","properties":{` +
			`"category":{"type":"string"},` +
			`"limit":{"type":"integer","minimum":1,"maximum":50,"default":10}}}`),
	}},
}

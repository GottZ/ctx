// Package store — web-chat sessions + messages (F6-C2/G35).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Source: https://github.com/GottZ/ctx
//
// Two visibility axes, deliberately separate (design 06 §3.1):
//   - Ownership = scope (home_scope of the creating key). ListSessions and
//     DeleteSession key on scope = caller.HomeScope — a key may not list or
//     delete a foreign tenant's chats, and key rotation never destroys
//     sessions (created_by is an audit pointer only, SET NULL on key delete).
//   - read_scopes = snapshot of the creating key's ReadScopes. Tool results may
//     carry cross-scope content (private reading hth, live-existent) and are
//     persisted ungekürzt; GetSession + continuing a stream therefore require
//     session.read_scopes ⊆ caller.ReadScopes (else ErrSessionNotFound, the
//     404 path) — closing the shadow-corpus channel against future
//     least-privilege agent keys.
//
// max_sensitivity is the session's content High-Water-Mark, MONOTONE rising
// (06 §2.3, F3 §2.2): AppendMessage raises it in the SAME short FOR-UPDATE TX
// that assigns the seq, so the §2.3 backend gate is structurally unforgettable
// — there is no message-insert path around it.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
)

// ErrSessionNotFound collapses "does not exist", "wrong tenant scope" and
// "read_scopes not a subset of the caller's" into ONE indistinguishable
// signal — the handler maps it to 404, so a foreign session is not even
// confirmed to exist (no 403 oracle).
var ErrSessionNotFound = errors.New("store: chat session not found")

// ChatSession is one persisted web-chat session row.
type ChatSession struct {
	ID             string               `json:"id"`
	Scope          string               `json:"scope"`
	ReadScopes     []string             `json:"read_scopes"`
	CreatedBy      string               `json:"created_by,omitempty"` // "" when the key was deleted (SET NULL)
	Title          string               `json:"title"`
	MaxSensitivity backends.Sensitivity `json:"max_sensitivity"`
	BusyUntil      *time.Time           `json:"busy_until,omitempty"`
	Metadata       map[string]any       `json:"metadata,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	ArchivedAt     *time.Time           `json:"archived_at,omitempty"`
}

// ChatMessage is one persisted message row. Optional tool/telemetry fields are
// empty/nil for roles that do not carry them.
type ChatMessage struct {
	ID               string               `json:"id"`
	SessionID        string               `json:"session_id"`
	Seq              int                  `json:"seq"`
	Role             string               `json:"role"`
	Content          string               `json:"content"`
	Sensitivity      backends.Sensitivity `json:"sensitivity"`
	ToolCalls        []byte               `json:"tool_calls,omitempty"` // assistant: raw JSON array
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

// NewMessage is the input for AppendMessage; id, seq and created_at are
// assigned by the store. Sensitivity is normalized fail-closed: any value
// outside the four defined levels (including the zero value) is stored AND
// folded into the HWM as credentials (mirrors backends.sensRank).
type NewMessage struct {
	Role             string
	Content          string
	Sensitivity      backends.Sensitivity
	ToolCalls        []byte
	ToolCallID       string
	ToolName         string
	Backend          string
	Model            string
	PromptTokens     *int
	CompletionTokens *int
	DurationMs       *int
	Metadata         map[string]any
}

// sessionColumns is the SELECT list shared by GetSession/ListSessions.
// created_by is COALESCEd to '' so a SET-NULL row scans into a plain string.
const sessionColumns = `id, scope, read_scopes, COALESCE(created_by::text, ''),
	title, max_sensitivity, busy_until, metadata, created_at, updated_at, archived_at`

func scanSession(row pgx.Row) (*ChatSession, error) {
	s := &ChatSession{}
	var sens string
	if err := row.Scan(&s.ID, &s.Scope, &s.ReadScopes, &s.CreatedBy,
		&s.Title, &sens, &s.BusyUntil, &s.Metadata, &s.CreatedAt, &s.UpdatedAt, &s.ArchivedAt); err != nil {
		return nil, err
	}
	s.MaxSensitivity = backends.Sensitivity(sens)
	return s, nil
}

// CreateSession inserts a new session owned by scope, snapshotting readScopes
// (the creating key's ReadScopes — [HomeScope]+AllowedScopes). createdByKeyID
// "" inserts NULL (anonymous/unknown key). An empty title becomes 'New chat'.
func CreateSession(ctx context.Context, pool *pgxpool.Pool, scope string, readScopes []string, createdByKeyID, title string) (*ChatSession, error) {
	if readScopes == nil {
		readScopes = []string{}
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = "New chat"
	}
	var createdBy *string // nil → NULL (UUID column rejects "")
	if createdByKeyID != "" {
		createdBy = &createdByKeyID
	}
	row := pool.QueryRow(ctx,
		`INSERT INTO context_chat_sessions (scope, read_scopes, created_by, title, created_by_principal)
		 VALUES ($1, $2, $3, $4,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $3::uuid))
		 RETURNING `+sessionColumns,
		scope, readScopes, createdBy, title)
	s, err := scanSession(row)
	if err != nil {
		return nil, fmt.Errorf("store: create chat session: %w", err)
	}
	return s, nil
}

// GetSession loads one session iff its read_scopes snapshot is a subset of the
// caller's ReadScopes (the shadow-corpus gate, 06 §3.1). A miss — non-existent,
// foreign tenant, or narrower read scope — is ErrSessionNotFound (404, no
// oracle). The `read_scopes <@ $2` predicate IS the gate; removing it makes the
// scope-isolation + subset integration probes go red.
func GetSession(ctx context.Context, pool *pgxpool.Pool, id string, callerReadScopes []string) (*ChatSession, error) {
	if callerReadScopes == nil {
		callerReadScopes = []string{}
	}
	row := pool.QueryRow(ctx,
		`SELECT `+sessionColumns+`
		 FROM context_chat_sessions
		 WHERE id = $1 AND read_scopes <@ $2::text[]`,
		id, callerReadScopes)
	s, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get chat session: %w", err)
	}
	return s, nil
}

// ListSessions returns the home-scope's non-archived sessions, newest first
// (metadata only — no messages). Scope-owned, NOT read-scope filtered (06
// §3.1): a key with allowed_scopes=[shared] must not see the shared tenant's
// chats in its list. limit <= 0 falls back to 100.
func ListSessions(ctx context.Context, pool *pgxpool.Pool, homeScope string, limit int) ([]ChatSession, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := pool.Query(ctx,
		`SELECT `+sessionColumns+`
		 FROM context_chat_sessions
		 WHERE scope = $1 AND archived_at IS NULL
		 ORDER BY updated_at DESC LIMIT $2`,
		homeScope, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list chat sessions: %w", err)
	}
	defer rows.Close()

	var out []ChatSession
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list chat sessions scan: %w", err)
		}
		out = append(out, *s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list chat sessions rows: %w", err)
	}
	return out, nil
}

// DeleteSession removes a session (messages cascade) within the caller's home
// scope. Scope-owned like ListSessions — a title is the user's own message and
// deleting exfiltrates nothing, so no read_scopes-subset gate here. Miss →
// ErrSessionNotFound.
func DeleteSession(ctx context.Context, pool *pgxpool.Pool, id, homeScope string) error {
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_chat_sessions WHERE id = $1 AND scope = $2`, id, homeScope)
	if err != nil {
		return fmt.Errorf("store: delete chat session: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// MessageCounts returns message_count per session id in ONE batch query (no
// N+1 across the list). The list handler joins it onto ListSessions to fill the
// message_count navigation anchor (06 §3.3); ids the caller is not allowed to
// see never reach here (ListSessions is already home-scope filtered). An empty
// id slice short-circuits without touching the pool. Sessions with no messages
// are simply absent from the map (caller treats missing as 0).
func MessageCounts(ctx context.Context, pool *pgxpool.Pool, sessionIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT session_id::text, COUNT(*) FROM context_chat_messages
		 WHERE session_id = ANY($1::uuid[]) GROUP BY session_id`,
		sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("store: message counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: message counts scan: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: message counts rows: %w", err)
	}
	return out, nil
}

// AppendMessage assigns the next seq and persists one message in a single short
// FOR-UPDATE transaction, raising the session HWM to max(current, msg) in the
// SAME TX (06 §3.1). Returns the persisted message and the resulting session
// HWM (so the engine knows `required` for the next model call without a
// re-SELECT). Session gone → ErrSessionNotFound.
func AppendMessage(ctx context.Context, pool *pgxpool.Pool, sessionID string, m NewMessage) (*ChatMessage, backends.Sensitivity, error) {
	// Normalize fail-closed: an unknown/empty level would fail the CHECK on
	// INSERT and must never act as a silent public message in the gate.
	msgSens := m.Sensitivity
	if !backends.ValidSensitivity(msgSens) {
		msgSens = backends.SensCredentials
	}
	metadata := m.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	var toolCalls any // nil → NULL jsonb (empty []byte would be invalid JSON)
	if len(m.ToolCalls) > 0 {
		toolCalls = m.ToolCalls
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("store: append message begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock the session row: serializes concurrent appends on the same session
	// (seq race) and reads the current HWM. UNIQUE(session_id,seq) is the net.
	var curHWM string
	err = tx.QueryRow(ctx,
		`SELECT max_sensitivity FROM context_chat_sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&curHWM)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrSessionNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: append message lock session: %w", err)
	}

	var nextSeq int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM context_chat_messages WHERE session_id = $1`,
		sessionID).Scan(&nextSeq); err != nil {
		return nil, "", fmt.Errorf("store: append message next seq: %w", err)
	}

	msg := &ChatMessage{
		SessionID:        sessionID,
		Seq:              nextSeq,
		Role:             m.Role,
		Content:          m.Content,
		Sensitivity:      msgSens,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ToolName:         m.ToolName,
		Backend:          m.Backend,
		Model:            m.Model,
		PromptTokens:     m.PromptTokens,
		CompletionTokens: m.CompletionTokens,
		DurationMs:       m.DurationMs,
		Metadata:         metadata,
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO context_chat_messages
		   (session_id, seq, role, content, sensitivity, tool_calls, tool_call_id,
		    tool_name, backend, model, prompt_tokens, completion_tokens, duration_ms, metadata)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		 RETURNING id, created_at`,
		sessionID, nextSeq, m.Role, m.Content, string(msgSens), toolCalls,
		m.ToolCallID, m.ToolName, m.Backend, m.Model,
		m.PromptTokens, m.CompletionTokens, m.DurationMs, metadata).
		Scan(&msg.ID, &msg.CreatedAt); err != nil {
		return nil, "", fmt.Errorf("store: append message insert: %w", err)
	}

	// HWM is monotone (MaxSensitivity never lowers) — set unconditionally and
	// bump updated_at in the same TX.
	newHWM := backends.MaxSensitivity(backends.Sensitivity(curHWM), msgSens)
	if _, err := tx.Exec(ctx,
		`UPDATE context_chat_sessions SET max_sensitivity = $2, updated_at = now() WHERE id = $1`,
		sessionID, string(newHWM)); err != nil {
		return nil, "", fmt.Errorf("store: append message raise hwm: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, "", fmt.Errorf("store: append message commit: %w", err)
	}
	return msg, newHWM, nil
}

// ListMessages returns a session's messages with seq > afterSeq, ordered by seq
// (the UNIQUE index serves this read). afterSeq 0 = from the start; limit <= 0
// = all. The caller MUST have gated the session via GetSession first — this is
// id-only by design (the GET-detail handler returns session + messages
// together behind the single read_scopes gate).
func ListMessages(ctx context.Context, pool *pgxpool.Pool, sessionID string, afterSeq, limit int) ([]ChatMessage, error) {
	q := `SELECT id, session_id, seq, role, content, sensitivity, tool_calls,
	         COALESCE(tool_call_id,''), COALESCE(tool_name,''), COALESCE(backend,''),
	         COALESCE(model,''), prompt_tokens, completion_tokens, duration_ms, metadata, created_at
	      FROM context_chat_messages
	      WHERE session_id = $1 AND seq > $2
	      ORDER BY seq`
	args := []any{sessionID, afterSeq}
	if limit > 0 {
		q += ` LIMIT $3`
		args = append(args, limit)
	}
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list chat messages: %w", err)
	}
	defer rows.Close()

	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var sens string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &m.Content, &sens,
			&m.ToolCalls, &m.ToolCallID, &m.ToolName, &m.Backend, &m.Model,
			&m.PromptTokens, &m.CompletionTokens, &m.DurationMs, &m.Metadata, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: list chat messages scan: %w", err)
		}
		m.Sensitivity = backends.Sensitivity(sens)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list chat messages rows: %w", err)
	}
	return out, nil
}

// ClaimTurn is the busy CAS (06 §3.1): it stamps busy_until = now()+ttl iff the
// session is free (NULL or expired) and returns claimed=false otherwise — the
// handler maps false to 409 WITHOUT blocking. Existence/readability is gated by
// a prior GetSession (the 404 path), so a false here means "busy", not "gone".
// An expired busy_until claims fine → a crashed turn self-heals after ttl.
func ClaimTurn(ctx context.Context, pool *pgxpool.Pool, id string, ttl time.Duration) (bool, error) {
	var claimed string
	err := pool.QueryRow(ctx,
		`UPDATE context_chat_sessions
		    SET busy_until = now() + make_interval(secs => $2)
		  WHERE id = $1 AND (busy_until IS NULL OR busy_until < now())
		  RETURNING id`,
		id, ttl.Seconds()).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: claim turn: %w", err)
	}
	return true, nil
}

// RefreshTurn extends busy_until while a turn iterates (called by the engine
// per tool loop). Only the turn holder runs this within its serialized turn.
func RefreshTurn(ctx context.Context, pool *pgxpool.Pool, id string, ttl time.Duration) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_chat_sessions SET busy_until = now() + make_interval(secs => $2) WHERE id = $1`,
		id, ttl.Seconds())
	if err != nil {
		return fmt.Errorf("store: refresh turn: %w", err)
	}
	return nil
}

// ReleaseTurn clears busy_until at turn end (and bumps updated_at). Idempotent.
func ReleaseTurn(ctx context.Context, pool *pgxpool.Pool, id string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_chat_sessions SET busy_until = NULL, updated_at = now() WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: release turn: %w", err)
	}
	return nil
}

// SetTitleIfDefault renames a session only while its title is still the
// 'New chat' default — the auto-title path (first user message). An explicit
// later user rename is never clobbered. No-op (no error) if already renamed.
func SetTitleIfDefault(ctx context.Context, pool *pgxpool.Pool, id, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil
	}
	_, err := pool.Exec(ctx,
		`UPDATE context_chat_sessions SET title = $2, updated_at = now()
		  WHERE id = $1 AND title = 'New chat'`,
		id, title)
	if err != nil {
		return fmt.Errorf("store: set title: %w", err)
	}
	return nil
}

// DeriveTitle turns the first user message into a session title: whitespace
// collapsed, trimmed to <= 60 runes on a word boundary where possible. Pure —
// the retention/auto-title engine code path is unit-testable without a DB.
func DeriveTitle(content string) string {
	title := strings.Join(strings.Fields(content), " ")
	if title == "" {
		return "New chat"
	}
	const maxRunes = 60
	r := []rune(title)
	if len(r) <= maxRunes {
		return title
	}
	cut := string(r[:maxRunes])
	if i := strings.LastIndex(cut, " "); i >= maxRunes/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "…"
}

// DeleteExpiredSessions is the retention janitor (06 §3.1, off by default):
// ttl <= 0 is a no-op (and never touches the pool). Messages cascade. The
// scheduler tick that calls this on an interval is wired in C4 behind
// CTX_WEBCHAT_SESSION_RETENTION.
func DeleteExpiredSessions(ctx context.Context, pool *pgxpool.Pool, ttl time.Duration) (int64, error) {
	if ttl <= 0 {
		return 0, nil
	}
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_chat_sessions WHERE updated_at < now() - make_interval(secs => $1)`,
		ttl.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	return tag.RowsAffected(), nil
}

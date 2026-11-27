package llmlog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The read side of context_llm_recordings (issue #575): the server-admin's
// view of the recordings (handler/model_recordings.go, the manage actions
// model-recording-*). Every statement here only reads — no FOR UPDATE, no
// write, no lock beyond the table's ACCESS SHARE —, so a read never waits
// for the gateway's writer and the writer never waits for a read
// (TestRecordingReadsLeaveTheRowAlone pins the row byte-equal, xmin included,
// across every read and translation). The bodies leave the database only in
// GetRecording (the whole row, for the decomposition into blocks) and in
// ReadRecordingBody (one window of one body); the lists carry metadata and
// sizes alone.

// RecordingListPage bounds one list page.
const (
	// RecordingListDefault is a page's size when the caller names none.
	RecordingListDefault = 50
	// RecordingListMax is the largest page.
	RecordingListMax = 100
	// RecordingListScan is the most rows one filtered list call walks on the
	// consumer's time line (idx_llm_recordings_consumer) before it answers:
	// a filter that matches rarely — a state, a status class — would
	// otherwise walk the consumer's whole line (10^8 rows at 30 requests a
	// second over 90 days) in one call. The answer then names where the walk
	// stopped (next_cursor) and the next call goes on from there. A fixed
	// bound on the cost of one call, not a policy of anybody's.
	RecordingListScan = 2000
)

// RecordingMeta is one recording without its bodies.
type RecordingMeta struct {
	ID              string     `json:"id"`
	// Consumer is the consumer's id (not ConsumerID: that name is
	// Entry.ConsumerID's, whose one setter TestGatewayConsumerIDCensus pins).
	Consumer        string     `json:"consumer_id"`
	CreatedAt       time.Time  `json:"created_at"`
	FinishedAt      *time.Time `json:"finished_at"`
	ProcessID       string     `json:"process_id"`
	ModelKeyID      *string    `json:"model_key_id"`
	Path            string     `json:"path"`
	Op              *string    `json:"op"`
	ClientRequestID *string    `json:"client_request_id"`
	Model           *string    `json:"model"`
	Stream          *bool      `json:"stream"`
	DeclaredBytes   *int64     `json:"declared_bytes"`
	RequestBytes    int64      `json:"request_bytes"`
	RequestComplete bool       `json:"request_complete"`
	State           string     `json:"state"`
	Status          *int16     `json:"status"`
	Code            *string    `json:"code"`
	ResponseBytes   int64      `json:"response_bytes"`
	// RequestCleared: the retention NULLed request_body. ResponseNull:
	// response_body is NULL — cleared by the retention when ResponseBytes >
	// 0, never recorded otherwise.
	RequestCleared bool `json:"request_cleared"`
	ResponseNull   bool `json:"response_null"`
}

// recordingMetaCols are the columns RecordingMeta scans, in its order — no
// body: `IS NULL` on a bytea reads the tuple's null bitmap, never the value.
const recordingMetaCols = `id::text AS m_id, consumer_id::text AS m_consumer, created_at AS m_created, finished_at AS m_finished,
	process_id::text AS m_process, model_key_id::text AS m_key, path AS m_path, op AS m_op, client_request_id AS m_client,
	model AS m_model, stream AS m_stream, declared_bytes AS m_declared, request_bytes AS m_req_bytes,
	request_complete AS m_complete, state AS m_state, status AS m_status, code AS m_code, response_bytes AS m_resp_bytes,
	request_body IS NULL AS m_req_null, response_body IS NULL AS m_resp_null`

// recordingMetaNames are recordingMetaCols' names, for a statement that
// selects them out of a CTE.
const recordingMetaNames = `m_id, m_consumer, m_created, m_finished, m_process, m_key, m_path, m_op, m_client, m_model,
	m_stream, m_declared, m_req_bytes, m_complete, m_state, m_status, m_code, m_resp_bytes, m_req_null, m_resp_null`

func scanRecordingMeta(row pgx.Row, m *RecordingMeta, extra ...any) error {
	return row.Scan(append(scanTargets(m), extra...)...)
}

// RecordingConsumer is one consumer of the recordings overview: a consumer
// whose switch is on, or one with recordings (a consumer switched off or
// deleted keeps its rows — no foreign key).
type RecordingConsumer struct {
	Consumer       string     `json:"consumer_id"`
	Label          *string    `json:"label"`
	TenantID       *string    `json:"tenant_id"`
	TenantSlug     *string    `json:"tenant_slug"`
	RecordBodies   bool       `json:"record_bodies"`
	Active         bool       `json:"active"`
	Deleted        bool       `json:"deleted"`
	LastRecordedAt *time.Time `json:"last_recorded_at"`
}

// recordingConsumersSQL: the consumer ids of the table by a loose index scan
// over idx_llm_recordings_consumer (one index descent per consumer — the
// table's 10^8 rows are never walked), united with the consumers whose
// switch is on, each with its newest row (one descent each).
const recordingConsumersSQL = `
WITH RECURSIVE ids AS (
    (SELECT consumer_id FROM context_llm_recordings ORDER BY consumer_id LIMIT 1)
    UNION ALL
    SELECT (SELECT r.consumer_id FROM context_llm_recordings r
             WHERE r.consumer_id > ids.consumer_id ORDER BY r.consumer_id LIMIT 1)
      FROM ids WHERE ids.consumer_id IS NOT NULL
), cand AS (
    SELECT consumer_id AS id FROM ids WHERE consumer_id IS NOT NULL
    UNION
    SELECT id FROM context_model_consumers WHERE record_bodies
)
SELECT cand.id::text, c.label, c.tenant_id::text, t.slug::text,
       COALESCE(c.record_bodies, false), COALESCE(c.active, false), c.id IS NULL,
       (SELECT r.created_at FROM context_llm_recordings r
         WHERE r.consumer_id = cand.id ORDER BY r.created_at DESC LIMIT 1)
  FROM cand
  LEFT JOIN context_model_consumers c ON c.id = cand.id
  LEFT JOIN context_tenants t ON t.id = c.tenant_id
 ORDER BY COALESCE(c.record_bodies, false) DESC, c.label NULLS LAST, cand.id`

// RecordingConsumers lists the consumers of the recordings overview.
func (r Recordings) RecordingConsumers(ctx context.Context) ([]RecordingConsumer, error) {
	if r.Pool == nil {
		return nil, errRecordingPool
	}
	rows, err := r.Pool.Query(ctx, recordingConsumersSQL)
	if err != nil {
		return nil, fmt.Errorf("llmlog: recording consumers: %w", err)
	}
	defer rows.Close()
	out := []RecordingConsumer{}
	for rows.Next() {
		var c RecordingConsumer
		if err := rows.Scan(&c.Consumer, &c.Label, &c.TenantID, &c.TenantSlug, &c.RecordBodies, &c.Active, &c.Deleted, &c.LastRecordedAt); err != nil {
			return nil, fmt.Errorf("llmlog: recording consumers: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llmlog: recording consumers: %w", err)
	}
	return out, nil
}

// RecordingCursor is a position on a consumer's time line: the list goes on
// with the rows strictly older than it (created_at, id descending).
type RecordingCursor struct {
	CreatedAt time.Time
	ID        string
}

// ErrRecordingCursor: a cursor that does not decode.
var ErrRecordingCursor = errors.New("llmlog: invalid recording cursor")

// String encodes c for the wire (opaque to the client).
func (c RecordingCursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

// ParseRecordingCursor decodes a cursor String made.
func ParseRecordingCursor(s string) (RecordingCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return RecordingCursor{}, ErrRecordingCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return RecordingCursor{}, ErrRecordingCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return RecordingCursor{}, ErrRecordingCursor
	}
	if _, err := uuid.Parse(id); err != nil {
		return RecordingCursor{}, ErrRecordingCursor
	}
	return RecordingCursor{CreatedAt: t, ID: id}, nil
}

// RecordingListQuery selects one page of a consumer's recordings, newest
// first. The zero values filter nothing.
type RecordingListQuery struct {
	ConsumerID string
	Cursor     *RecordingCursor // nil: from the newest row on
	Since      *time.Time       // created_at >= Since
	Until      *time.Time       // created_at < Until
	// Ops: the rows whose op is one of these; "" stands for a row no route
	// parsed (op NULL).
	Ops []string
	// State: "open" or "done".
	State string
	// StatusClass: 2..5 for 2xx..5xx, 0 for "no status" (NULL — the gateway
	// wrote nothing); nil filters nothing.
	StatusClass *int
	Limit       int
}

// RecordingPage is one list answer.
type RecordingPage struct {
	Recordings []RecordingMeta
	// Next continues the list (nil: the line is walked to its end).
	Next *RecordingCursor
	// Scanned is how many rows of the consumer's line this call walked;
	// ScanLimitReached: it stopped at RecordingListScan with fewer matches
	// than the page holds — Next goes on from the last walked row.
	Scanned          int
	ScanLimitReached bool
}

func (q RecordingListQuery) filtered() bool {
	return len(q.Ops) > 0 || q.State != "" || q.StatusClass != nil
}

// recordingListSQL builds the statement of one list call. It walks the
// consumer's line in the index's order — at most `scan` rows — and keeps the
// matches of the filters, at most `hits`; the last row says where the walk
// ended and how far it went. The walk rides idx_llm_recordings_consumer
// (consumer_id, created_at DESC): the cursor and the time bounds are index
// conditions, the id tie-break sorts only the rows of one created_at
// (Incremental Sort; TestRecordingListPlans pins the plan at scale, the
// generic plan included). Every clause is in the text only when its filter
// is set — no "$n IS NULL OR …", which a generic plan cannot turn into an
// index bound.
func recordingListSQL(q RecordingListQuery, scan, hits int) (string, []any) {
	args := []any{q.ConsumerID}
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where := []string{"consumer_id = $1::uuid"}
	if q.Cursor != nil {
		ts, id := arg(q.Cursor.CreatedAt), arg(q.Cursor.ID)
		where = append(where, "created_at <= "+ts+"::timestamptz", "(created_at < "+ts+"::timestamptz OR id < "+id+"::uuid)")
	}
	if q.Since != nil {
		where = append(where, "created_at >= "+arg(*q.Since)+"::timestamptz")
	}
	if q.Until != nil {
		where = append(where, "created_at < "+arg(*q.Until)+"::timestamptz")
	}
	var filter []string
	if len(q.Ops) > 0 {
		filter = append(filter, "COALESCE(m_op, '') = ANY ("+arg(q.Ops)+"::text[])")
	}
	if q.State != "" {
		filter = append(filter, "m_state = "+arg(q.State)+"::text")
	}
	if q.StatusClass != nil {
		filter = append(filter, "COALESCE(m_status / 100, 0) = "+arg(*q.StatusClass)+"::int")
	}
	hitWhere := ""
	if len(filter) > 0 {
		hitWhere = "WHERE " + strings.Join(filter, " AND ")
	}
	sql := `
WITH scan AS MATERIALIZED (
    SELECT id AS rid, ` + recordingMetaCols + `
      FROM context_llm_recordings
     WHERE ` + strings.Join(where, " AND ") + `
     ORDER BY created_at DESC, id DESC
     LIMIT ` + arg(scan) + `
), hits AS (
    SELECT * FROM scan ` + hitWhere + `
     ORDER BY m_created DESC, rid DESC
     LIMIT ` + arg(hits) + `
)
SELECT false, 0, ` + recordingMetaNames + ` FROM hits
UNION ALL
SELECT true, (SELECT count(*) FROM scan)::int, ` + recordingMetaNames + `
  FROM (SELECT * FROM scan ORDER BY m_created, rid LIMIT 1) tail`
	return sql, args
}

// ListRecordings returns one page of a consumer's recordings, newest first.
func (r Recordings) ListRecordings(ctx context.Context, q RecordingListQuery) (RecordingPage, error) {
	if r.Pool == nil {
		return RecordingPage{}, errRecordingPool
	}
	limit := q.Limit
	if limit <= 0 {
		limit = RecordingListDefault
	}
	limit = min(limit, RecordingListMax)
	scan := limit + 1
	if q.filtered() {
		scan = RecordingListScan
	}
	sql, args := recordingListSQL(q, scan, limit+1)
	rows, err := r.Pool.Query(ctx, sql, args...)
	if err != nil {
		return RecordingPage{}, fmt.Errorf("llmlog: list recordings: %w", err)
	}
	defer rows.Close()
	page := RecordingPage{Recordings: []RecordingMeta{}}
	var tail *RecordingMeta
	for rows.Next() {
		var isTail bool
		var scanned int
		var m RecordingMeta
		if err := rows.Scan(append([]any{&isTail, &scanned}, scanTargets(&m)...)...); err != nil {
			return RecordingPage{}, fmt.Errorf("llmlog: list recordings: %w", err)
		}
		if isTail {
			tail, page.Scanned = &m, scanned
			continue
		}
		page.Recordings = append(page.Recordings, m)
	}
	if err := rows.Err(); err != nil {
		return RecordingPage{}, fmt.Errorf("llmlog: list recordings: %w", err)
	}
	// The answer's order is the line's, whatever order the statement's
	// branches arrive in.
	sort.SliceStable(page.Recordings, func(i, j int) bool {
		a, b := page.Recordings[i], page.Recordings[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	switch {
	case len(page.Recordings) > limit:
		page.Recordings = page.Recordings[:limit]
		last := page.Recordings[limit-1]
		page.Next = &RecordingCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	case page.Scanned >= scan && tail != nil && q.filtered():
		page.Next = &RecordingCursor{CreatedAt: tail.CreatedAt, ID: tail.ID}
		page.ScanLimitReached = true
	}
	return page, nil
}

// scanTargets are RecordingMeta's scan destinations in recordingMetaCols'
// order.
func scanTargets(m *RecordingMeta) []any {
	return []any{&m.ID, &m.Consumer, &m.CreatedAt, &m.FinishedAt, &m.ProcessID, &m.ModelKeyID,
		&m.Path, &m.Op, &m.ClientRequestID, &m.Model, &m.Stream, &m.DeclaredBytes, &m.RequestBytes, &m.RequestComplete,
		&m.State, &m.Status, &m.Code, &m.ResponseBytes, &m.RequestCleared, &m.ResponseNull}
}

// RecordingRow is one recording with its bodies.
type RecordingRow struct {
	RecordingMeta
	RequestBody  []byte
	ResponseBody []byte
}

// ErrRecordingNotFound: no row of that id (or the id is no UUID).
var ErrRecordingNotFound = errors.New("llmlog: recording not found")

// GetRecording reads one recording whole (its bodies as stored).
func (r Recordings) GetRecording(ctx context.Context, id string) (*RecordingRow, error) {
	if r.Pool == nil {
		return nil, errRecordingPool
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrRecordingNotFound
	}
	var row RecordingRow
	err := scanRecordingMeta(r.Pool.QueryRow(ctx,
		`SELECT `+recordingMetaCols+`, request_body, response_body FROM context_llm_recordings WHERE id = $1::uuid`, id),
		&row.RecordingMeta, &row.RequestBody, &row.ResponseBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRecordingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("llmlog: get recording: %w", err)
	}
	return &row, nil
}

// GetRecordingMeta reads one recording without its bodies.
func (r Recordings) GetRecordingMeta(ctx context.Context, id string) (*RecordingMeta, error) {
	if r.Pool == nil {
		return nil, errRecordingPool
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrRecordingNotFound
	}
	var m RecordingMeta
	err := scanRecordingMeta(r.Pool.QueryRow(ctx,
		`SELECT `+recordingMetaCols+` FROM context_llm_recordings WHERE id = $1::uuid`, id), &m)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRecordingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("llmlog: get recording: %w", err)
	}
	return &m, nil
}

// RecordingWindow is one window of one body.
type RecordingWindow struct {
	Data  []byte
	Total int64
	// Null: the body is NULL (cleared by the retention, or never recorded).
	Null bool
}

// ReadRecordingBody reads bytes [offset, offset+limit) of one body ("request"
// or "response") of recording id — the database cuts the window, so a body of
// MiB never travels whole for one window. offset+limit must fit an int4 (the
// database's substring takes int; a body is at most a few MiB).
func (r Recordings) ReadRecordingBody(ctx context.Context, id, side string, offset, limit int64) (*RecordingWindow, error) {
	if r.Pool == nil {
		return nil, errRecordingPool
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil, ErrRecordingNotFound
	}
	var stmt string
	switch side {
	case "request":
		stmt = `SELECT substring(request_body FROM $2::int + 1 FOR $3::int), octet_length(request_body), request_body IS NULL
		          FROM context_llm_recordings WHERE id = $1::uuid`
	case "response":
		stmt = `SELECT substring(response_body FROM $2::int + 1 FOR $3::int), octet_length(response_body), response_body IS NULL
		          FROM context_llm_recordings WHERE id = $1::uuid`
	default:
		return nil, fmt.Errorf("llmlog: read recording body: unknown side %q", side)
	}
	var w RecordingWindow
	var total *int64
	err := r.Pool.QueryRow(ctx, stmt, id, offset, limit).Scan(&w.Data, &total, &w.Null)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRecordingNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("llmlog: read recording body: %w", err)
	}
	if total != nil {
		w.Total = *total
	}
	return &w, nil
}

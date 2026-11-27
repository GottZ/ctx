package handler

// The recordings view (issue #575): the server-admin's reading of
// context_llm_recordings — the requests of the model consumers whose switch
// record_bodies is on (#571), held whole, credentials in clear text included.
//
// Five manage actions, every one tierServerAdmin in the dispatch table
// (manage_table.go) — the gate stands in the row that routes them, and every
// other caller (a tenant-admin or owner, a member, a session or token of a
// key without the server-admin bit) gets the shared 403 before a handler
// here runs; a model key never authenticates on /api at all:
//
//   - model-recording-consumers — the consumers that record or have
//     recordings, with this process's counts (metadata only);
//   - model-recording-list — one consumer's recordings, newest first, keyset
//     paginated, filtered (metadata and sizes only, never a body);
//   - model-recording-get — one recording as blocks (internal/recblocks) with
//     a preview each;
//   - model-recording-read — one window of a block or of a whole body;
//   - model-recording-translate — one chunk of one block, rendered by a model
//     of this instance (model_recordings_translate.go).
//
// The three that hand out content leave a model_recording audit row each
// (store.WriteRecordingAudit) BEFORE the answer is written: a read that cannot
// be traced is not served (503 audit_unavailable). The two lists are not
// audited one by one — they carry what the status frame and the consumer view
// already show a server-admin (ids, sizes, states, times), no byte of a body.
// Nothing here writes context_llm_recordings: the reads are plain SELECTs
// (llmlog/recordings_read.go).

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/recblocks"
	"github.com/GottZ/ctx/internal/store"
)

// Preview and window bounds of the view. Fixed: they bound one answer of an
// admin view, no policy of anybody's.
const (
	// recordingPreviewBytes is the most of one block the detail carries
	// inline; the rest is read in windows.
	recordingPreviewBytes = 4096
	// recordingPreviewBudget is the most preview bytes one detail answer
	// carries over all its blocks (a chat of thousands of messages):
	// beyond it the blocks are listed without a preview.
	recordingPreviewBudget = 256 << 10
	// recordingBlocksPage is the most blocks one detail answer carries (and
	// its default page): a request of thousands of messages is read page by
	// page (block_offset).
	recordingBlocksPage = 500
	// recordingWindowDefault / recordingWindowMax bound a window read.
	recordingWindowDefault = 64 << 10
	recordingWindowMax     = 1 << 20
)

// recordingContentSlot admits one content read at a time, process-wide: a
// read holds a recording's bodies (up to gateway.max_request_bytes plus the
// answer) and their decomposition in memory, outside the gateway's in-flight
// budget, so their number is bounded here. A second reader waits for the
// slot within its request.
var recordingContentSlot = make(chan struct{}, 1)

func acquireRecordingContent(ctx context.Context) (func(), error) {
	select {
	case recordingContentSlot <- struct{}{}:
		return func() { <-recordingContentSlot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// writeRecordingError is the view's error envelope: the house shape plus a
// machine-readable code.
func writeRecordingError(w http.ResponseWriter, status int, code, msg string, extra ...any) {
	body := map[string]any{"success": false, "error": msg, "code": code}
	for i := 0; i+1 < len(extra); i += 2 {
		if k, ok := extra[i].(string); ok {
			body[k] = extra[i+1]
		}
	}
	writeJSON(w, status, body)
}

// writeRecordingStoreError maps a read's error; the detail goes to the log
// (never a body — the errors here carry none).
func writeRecordingStoreError(w http.ResponseWriter, r *http.Request, err error, action string) {
	switch {
	case errors.Is(err, llmlog.ErrRecordingNotFound):
		writeRecordingError(w, http.StatusNotFound, "not_found", "recording not found")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeRecordingError(w, http.StatusServiceUnavailable, "canceled", "the read was canceled")
	default:
		logRequestError(r.Context(), "manage: "+action+" failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeRecordingError(w, http.StatusInternalServerError, "internal", "Internal server error")
	}
}

// recordingStamp is the audit stamp of the caller.
func recordingStamp(r *http.Request, ar *auth.AuthResult) store.AuditStamp {
	return store.APIStamp(ar.ApiKeyID, RequestIDFromContext(r.Context()))
}

// auditRecordingRead commits the audit row of one content read; false when
// it failed (the answer is then written here: 503 audit_unavailable).
func (h *ManageHandler) auditRecordingRead(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, meta *llmlog.RecordingMeta, action string, detail map[string]any) bool {
	err := store.WriteRecordingAudit(r.Context(), h.pool, store.RecordingAudit{
		Stamp: recordingStamp(r, ar), RecordingID: meta.ID, ConsumerID: meta.Consumer, Action: action, Detail: detail,
	})
	if err != nil {
		logRequestError(r.Context(), "manage: recording audit failed — content not served", "error", err,
			"recording_id", meta.ID, "action", action, "request_id", RequestIDFromContext(r.Context()))
		writeRecordingError(w, http.StatusServiceUnavailable, "audit_unavailable", "the read could not be audited; nothing was served")
		return false
	}
	return true
}

// recordingConsumerView is one row of model-recording-consumers.
type recordingConsumerView struct {
	llmlog.RecordingConsumer
	// Process is this process's count of the consumer's requests since
	// process.since (GET /api/status gateway.recording); null when the
	// process admitted none of them.
	Process *oai.RecordingConsumer `json:"process"`
}

func (h *ManageHandler) handleModelRecordingConsumers(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	var d struct{}
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "model-recording-consumers takes no fields")
		return
	}
	cs, err := llmlog.Recordings{Pool: h.pool}.RecordingConsumers(r.Context())
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-consumers")
		return
	}
	stats := oai.RecordingStats()
	byID := make(map[string]oai.RecordingConsumer, len(stats.Consumers))
	for _, c := range stats.Consumers {
		byID[c.ConsumerID] = c
	}
	out := make([]recordingConsumerView, 0, len(cs))
	for _, c := range cs {
		v := recordingConsumerView{RecordingConsumer: c}
		if p, ok := byID[c.Consumer]; ok {
			v.Process = &p
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "process_id": stats.ProcessID, "since": stats.Since, "consumers": out,
	})
}

// recordingListRequest is model-recording-list's data.
type recordingListRequest struct {
	ConsumerID string   `json:"consumer_id"`
	Cursor     string   `json:"cursor"`
	Since      string   `json:"since"`
	Until      string   `json:"until"`
	Op         []string `json:"op"`
	State      string   `json:"state"`
	Status     string   `json:"status"`
	Limit      int      `json:"limit"`
}

// recordingOps maps the list's op filter values to the column's (none = a
// row no route parsed).
var recordingOps = map[string]string{
	llmlog.GatewayOpChat: llmlog.GatewayOpChat, llmlog.GatewayOpEmbed: llmlog.GatewayOpEmbed,
	llmlog.GatewayOpRerank: llmlog.GatewayOpRerank, "none": "",
}

// recordingStatusClasses maps the status filter to RecordingListQuery's.
var recordingStatusClasses = map[string]int{"2xx": 2, "3xx": 3, "4xx": 4, "5xx": 5, "none": 0}

// parseRecordingList validates the list request; "" or a message.
func parseRecordingList(d recordingListRequest) (llmlog.RecordingListQuery, string) {
	q := llmlog.RecordingListQuery{ConsumerID: d.ConsumerID, Limit: d.Limit}
	if !isUUID(d.ConsumerID) {
		return q, "consumer_id must be a UUID"
	}
	if d.Limit < 0 || d.Limit > llmlog.RecordingListMax {
		return q, "limit must be between 1 and 100"
	}
	if d.Cursor != "" {
		c, err := llmlog.ParseRecordingCursor(d.Cursor)
		if err != nil {
			return q, "cursor is not one this list gave"
		}
		q.Cursor = &c
	}
	for _, f := range []struct {
		v   string
		dst **time.Time
		n   string
	}{{d.Since, &q.Since, "since"}, {d.Until, &q.Until, "until"}} {
		if f.v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, f.v)
		if err != nil {
			return q, f.n + " must be an RFC 3339 time"
		}
		*f.dst = &t
	}
	for _, op := range d.Op {
		col, ok := recordingOps[op]
		if !ok {
			return q, "op must be one of chat, embed, rerank, none"
		}
		q.Ops = append(q.Ops, col)
	}
	switch d.State {
	case "", llmlog.RecordingOpen, llmlog.RecordingDone:
		q.State = d.State
	default:
		return q, "state must be open or done"
	}
	if d.Status != "" {
		c, ok := recordingStatusClasses[d.Status]
		if !ok {
			return q, "status must be one of 2xx, 3xx, 4xx, 5xx, none"
		}
		q.StatusClass = &c
	}
	return q, ""
}

func (h *ManageHandler) handleModelRecordingList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	var d recordingListRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "invalid data for model-recording-list")
		return
	}
	q, msg := parseRecordingList(d)
	if msg != "" {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", msg)
		return
	}
	page, err := llmlog.Recordings{Pool: h.pool}.ListRecordings(r.Context(), q)
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-list")
		return
	}
	var next *string
	if page.Next != nil {
		s := page.Next.String()
		next = &s
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "recordings": page.Recordings, "next_cursor": next,
		"scanned": page.Scanned, "scan_limit_reached": page.ScanLimitReached,
	})
}

// recordingInput is the decomposition's input of a row.
func recordingInput(row *llmlog.RecordingRow) recblocks.Input {
	in := recblocks.Input{
		Path: row.Path, State: row.State, Request: row.RequestBody, RequestNull: row.RequestCleared,
		Response: row.ResponseBody, ResponseNull: row.ResponseNull, ResponseBytes: row.ResponseBytes,
	}
	if row.Op != nil {
		in.Op = *row.Op
	}
	if row.Stream != nil {
		in.Stream = *row.Stream
	}
	if row.Status != nil {
		in.Status = int(*row.Status)
	}
	return in
}

// loadRecordingBlocks reads a recording whole under the content slot and
// decomposes it with split — a window of its blocks (the detail) or the one
// block of an address (a read, a translation): the walk meets every block,
// only those are kept, so the number of blocks of a body (millions, for a
// body of tiny elements) never becomes a list in memory or an answer.
func (h *ManageHandler) loadRecordingBlocks(ctx context.Context, id string, split func(recblocks.Input) recblocks.Result) (*llmlog.RecordingRow, recblocks.Input, recblocks.Result, error) {
	release, err := acquireRecordingContent(ctx)
	if err != nil {
		return nil, recblocks.Input{}, recblocks.Result{}, err
	}
	defer release()
	row, err := llmlog.Recordings{Pool: h.pool}.GetRecording(ctx, id)
	if err != nil {
		return nil, recblocks.Input{}, recblocks.Result{}, err
	}
	in := recordingInput(row)
	return row, in, split(in), nil
}

// recordingBlockAt is the split of a read or a translation: the block at
// address alone.
func recordingBlockAt(address string) func(recblocks.Input) recblocks.Result {
	return func(in recblocks.Input) recblocks.Result {
		r, _, _ := recblocks.Find(in, address)
		return r
	}
}

// recordingBlockView is one block of the detail.
type recordingBlockView struct {
	recblocks.Block
	Preview          string `json:"preview"`
	PreviewTruncated bool   `json:"preview_truncated"`
	// PreviewOmitted: no preview inline — the block is a payload (an image,
	// audio, a file), not valid UTF-8, or past the answer's preview budget.
	PreviewOmitted bool `json:"preview_omitted"`
}

// recordingBlockViews renders the blocks with their previews.
func recordingBlockViews(blocks []recblocks.Block) []recordingBlockView {
	out := make([]recordingBlockView, 0, len(blocks))
	budget := recordingPreviewBudget
	for _, b := range blocks {
		v := recordingBlockView{Block: b}
		c := b.Content()
		switch {
		case !b.Inline || !b.UTF8 || budget <= 0:
			v.PreviewOmitted = len(c) > 0
		default:
			// No builtin min here: an integration test file of this package
			// declares its own two-int min.
			n := len(c)
			if n > recordingPreviewBytes {
				n = recordingPreviewBytes
			}
			if n > budget {
				n = budget
			}
			for n > 0 && n < len(c) && !utf8.RuneStart(c[n]) {
				n--
			}
			v.Preview, v.PreviewTruncated = string(c[:n]), n < len(c)
			budget -= n
		}
		out = append(out, v)
	}
	return out
}

func (h *ManageHandler) handleModelRecordingGet(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d struct {
		ID          string `json:"id"`
		BlockOffset int    `json:"block_offset"`
		BlockLimit  int    `json:"block_limit"`
	}
	if err := decodeConsumerPayload(req.Data, &d); err != nil || d.ID == "" {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "id is required")
		return
	}
	if d.BlockLimit == 0 {
		d.BlockLimit = recordingBlocksPage
	}
	if d.BlockOffset < 0 || d.BlockLimit < 1 || d.BlockLimit > recordingBlocksPage {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "block_offset must be >= 0 and block_limit between 1 and 500")
		return
	}
	row, _, res, err := h.loadRecordingBlocks(r.Context(), d.ID, func(in recblocks.Input) recblocks.Result {
		return recblocks.DecomposeWindow(in, d.BlockOffset, d.BlockLimit)
	})
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-get")
		return
	}
	if !h.auditRecordingRead(w, r, ar, &row.RecordingMeta, store.RecordingAuditRead,
		map[string]any{"block_offset": d.BlockOffset, "blocks": len(res.Blocks), "blocks_total": res.Total}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "recording": row.RecordingMeta,
		"request": res.Request, "response": res.Response, "blocks": recordingBlockViews(res.Blocks),
		"block_offset": d.BlockOffset, "blocks_total": res.Total,
	})
}

// recordingReadRequest is model-recording-read's data.
type recordingReadRequest struct {
	ID      string `json:"id"`
	Address string `json:"address"`
	Offset  *int64 `json:"offset"`
	Limit   *int64 `json:"limit"`
}

// isRecordingSide reports whether address names a whole body.
func isRecordingSide(address string) bool {
	return address == "request" || address == "response"
}

// recordingWindowOf validates offset and limit.
func recordingWindowOf(d recordingReadRequest) (offset, limit int64, msg string) {
	offset, limit = 0, recordingWindowDefault
	if d.Offset != nil {
		offset = *d.Offset
	}
	if d.Limit != nil {
		limit = *d.Limit
	}
	if offset < 0 || offset > math.MaxInt32-recordingWindowMax || limit < 1 || limit > recordingWindowMax {
		return 0, 0, "offset must be between 0 and 2146435071 and limit between 1 and 1048576"
	}
	return offset, limit, ""
}

func (h *ManageHandler) handleModelRecordingRead(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d recordingReadRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil || d.ID == "" || d.Address == "" {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "id and address are required")
		return
	}
	offset, limit, msg := recordingWindowOf(d)
	if msg != "" {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", msg)
		return
	}
	detail := map[string]any{"block": d.Address, "offset": offset, "limit": limit}
	if isRecordingSide(d.Address) {
		h.readRecordingSide(w, r, ar, d, offset, limit, detail)
		return
	}
	row, in, res, err := h.loadRecordingBlocks(r.Context(), d.ID, recordingBlockAt(d.Address))
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-read")
		return
	}
	b, ok := lookupRecordingBlock(w, res, in, d.Address)
	if !ok {
		return
	}
	if !h.auditRecordingRead(w, r, ar, &row.RecordingMeta, store.RecordingAuditReadRaw, detail) {
		return
	}
	c := b.Content()
	total := int64(len(c))
	lo, hi := clampWindow(offset, limit, total)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "address": d.Address, "offset": lo, "limit": hi - lo, "total": total, "eof": hi >= total,
		"utf8": b.UTF8, "data": base64.StdEncoding.EncodeToString(c[lo:hi]),
	})
}

// lookupRecordingBlock finds the block at address; when there is none it
// answers — 409 body_cleared for an address on a side the retention
// cleared, 404 otherwise.
func lookupRecordingBlock(w http.ResponseWriter, res recblocks.Result, in recblocks.Input, address string) (recblocks.Block, bool) {
	if b, ok := recblocks.Lookup(res, in, address); ok {
		return b, true
	}
	side, _, _ := strings.Cut(address, "/")
	if (side == "request" && res.Request.State == "cleared") || (side == "response" && res.Response.State == "cleared") {
		writeRecordingError(w, http.StatusConflict, "body_cleared", "the retention cleared this body")
		return recblocks.Block{}, false
	}
	writeRecordingError(w, http.StatusNotFound, "not_found", "no block at this address")
	return recblocks.Block{}, false
}

// readRecordingSide answers a window of a whole body, cut by the database.
func (h *ManageHandler) readRecordingSide(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, d recordingReadRequest, offset, limit int64, detail map[string]any) {
	rec := llmlog.Recordings{Pool: h.pool}
	meta, err := rec.GetRecordingMeta(r.Context(), d.ID)
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-read")
		return
	}
	win, err := rec.ReadRecordingBody(r.Context(), d.ID, d.Address, offset, limit)
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-read")
		return
	}
	if win.Null {
		code, text := "no_body", "the recording holds no "+d.Address+" body"
		if d.Address == "request" || meta.ResponseBytes > 0 {
			code, text = "body_cleared", "the retention cleared this body"
		}
		writeRecordingError(w, http.StatusConflict, code, text)
		return
	}
	if !h.auditRecordingRead(w, r, ar, meta, store.RecordingAuditReadRaw, detail) {
		return
	}
	lo, _ := clampWindow(offset, limit, win.Total)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "address": d.Address, "offset": lo, "limit": int64(len(win.Data)), "total": win.Total,
		"eof": lo+int64(len(win.Data)) >= win.Total, "data": base64.StdEncoding.EncodeToString(win.Data),
	})
}

// clampWindow is [offset, offset+limit) within [0, total].
func clampWindow(offset, limit, total int64) (lo, hi int64) {
	lo, hi = offset, offset+limit
	if lo > total {
		lo = total
	}
	if hi > total {
		hi = total
	}
	return lo, hi
}

// recordingBrake admits one translation per principal at a time (the
// coupling of the interactive class with a brake, as the daily synthesis
// holds it: SynthesizeHandler.acquireDaily). The zero value is ready.
type recordingBrake struct {
	mu       sync.Mutex
	inflight map[string]struct{}
}

// acquire reserves who's slot; false while one of who's calls runs.
func (b *recordingBrake) acquire(who string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight == nil {
		b.inflight = map[string]struct{}{}
	}
	if _, busy := b.inflight[who]; busy {
		return false
	}
	b.inflight[who] = struct{}{}
	return true
}

func (b *recordingBrake) release(who string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.inflight, who)
}

// recordingBrakeKey is who a translation is counted against: the person
// behind the key (a session, a token and the key itself are one), else the
// key.
func recordingBrakeKey(ar *auth.AuthResult) string {
	if ar.PrincipalID != "" {
		return "p:" + ar.PrincipalID
	}
	return "k:" + ar.ApiKeyID
}

// trimLang lowers and trims a language code.
func trimLang(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/recblocks"
	"github.com/GottZ/ctx/internal/store"
)

// model-recording-translate (issue #575): one chunk of one block of a
// recording, rendered by a model of this instance — a translation into the
// target language and a readable form (Markdown) — as an additional display
// next to the recorded bytes. The request names the recording and the block
// address, never text: the server takes the block from the recording, so the
// action is no general translation proxy. The answer goes to the caller and
// nowhere else (llm.TranslateRecordingBlock: the chat role at credentials,
// local rows only, the slim llmlog row). One call per chunk: a long block is
// asked for chunk by chunk (chunks in the answer), each call bounded by the
// chain's timeouts, each audited.
//
// The dispatcher sees the call as interactive — a human waits for it — and
// the brake holds it to one call per person at a time (recordingBrake, the
// coupling the daily synthesis' interactive class carries). An interactive
// lease takes any free slot of its target (held+1 ≤ S); the external
// partition E = S − R caps external work, it reserves nothing for it. So a
// recording consumer's /v1 request waits only while translations and other
// interactive work fill every slot of the target — bounded by one call per
// server-admin, each ending within the chain's timeouts.

// recordingTranslateRequest is the action's data.
type recordingTranslateRequest struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Language string `json:"language"`
	Chunk    int    `json:"chunk"`
}

// recordingWriteMargin is added to the chain's longest walk for the answer's
// write deadline (the server's WriteTimeout is shorter than a slow model).
const recordingWriteMargin = 15 * time.Second

func (h *ManageHandler) handleModelRecordingTranslate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d recordingTranslateRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil || d.ID == "" || d.Address == "" {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "id and address are required")
		return
	}
	d.Language = trimLang(d.Language)
	if _, ok := llm.RecordingLanguages[d.Language]; !ok {
		writeRecordingError(w, http.StatusBadRequest, "invalid_language", "language must be one of the supported language codes")
		return
	}
	if d.Chunk < 0 || d.Chunk >= llm.RecordingTranslateMaxChunks {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "chunk is out of range")
		return
	}
	if h.backendPool == nil {
		writeRecordingError(w, http.StatusServiceUnavailable, "no_eligible_backend", "no backend of this instance may receive a recording")
		return
	}
	who := recordingBrakeKey(ar)
	if !h.recBrake.acquire(who) {
		w.Header().Set("Retry-After", "1")
		writeRecordingError(w, http.StatusTooManyRequests, "translate_busy", "a translation of yours is running; wait for it")
		return
	}
	defer h.recBrake.release(who)

	var tin llm.RecordingTranslateInput
	meta, found, err := h.withRecording(r.Context(), d.ID, func(in recblocks.Input) bool {
		return recordingTranslateCut(w, in, d, &tin)
	})
	if err != nil {
		writeRecordingStoreError(w, r, err, "model-recording-translate")
		return
	}
	if !found {
		return
	}
	cfg := h.cfg.SnapshotForRequest(r.Context())
	call := llm.RecordingTranslateCall{
		DB: h.pool, Pool: h.backendPool, Quota: h.quota, QuotaScope: ar.HomeScope, APIKeyID: ar.ApiKeyID,
		WindowFallback: cfg.Pool.ExternalNumCtxFallback,
		Admission:      llm.Admission{Admitter: h.admitter, Class: dispatch.ClassInteractive},
	}
	// Nothing is audited that will not be sent: the chain, the tenant quota
	// and the window are checked first, with the call that is then sent (no
	// row for a refusal before the wire).
	if err := llm.CheckRecordingTranslate(call, tin); err != nil {
		writeRecordingTranslateError(w, r, err)
		return
	}
	chain, err := llm.RecordingTranslateChain(h.backendPool)
	if err != nil {
		writeRecordingTranslateError(w, r, err)
		return
	}
	if !h.auditRecordingRead(w, r, ar, meta, store.RecordingAuditTranslate,
		map[string]any{"block": d.Address, "chunk": d.Chunk, "chunks": tin.Chunks, "language": d.Language}) {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(llm.RecordingTranslateDeadline(chain) + recordingWriteMargin))
	out, err := llm.TranslateRecordingBlock(r.Context(), call, tin)
	if err != nil {
		writeRecordingTranslateError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "id": meta.ID, "address": d.Address, "chunk": d.Chunk, "chunks": tin.Chunks,
		"language": d.Language, "text": out.Text, "model": out.Model, "generated": true,
	})
}

// recordingTranslateCut takes, under the content slot, what a translation
// sends: the block at d.Address — refused when it holds no text, is empty or
// is larger than one translation takes — and its chunk d.Chunk, copied, so
// the body is not held while the model answers. False when it wrote the
// refusal.
func recordingTranslateCut(w http.ResponseWriter, in recblocks.Input, d recordingTranslateRequest, tin *llm.RecordingTranslateInput) bool {
	res, _, _ := recblocks.Find(in, d.Address)
	b, ok := lookupRecordingBlock(w, res, in, d.Address)
	if !ok {
		return false
	}
	if !b.Translatable {
		writeRecordingError(w, http.StatusUnprocessableEntity, "not_translatable", "this block holds no text to translate")
		return false
	}
	if len(b.Content()) == 0 {
		writeRecordingError(w, http.StatusUnprocessableEntity, "not_translatable", "this block is empty")
		return false
	}
	chunks := llm.RecordingChunks(string(b.Content()))
	if len(chunks) > llm.RecordingTranslateMaxChunks {
		writeRecordingError(w, http.StatusRequestEntityTooLarge, "block_too_large", "the block is larger than one translation takes; read it in the raw view",
			"limit_runes", llm.RecordingTranslateChunkRunes*llm.RecordingTranslateMaxChunks)
		return false
	}
	if d.Chunk >= len(chunks) {
		writeRecordingError(w, http.StatusBadRequest, "invalid_body", "chunk is out of range", "chunks", len(chunks))
		return false
	}
	*tin = llm.RecordingTranslateInput{
		Address: d.Address, Kind: b.Kind, Role: b.Role, Text: strings.Clone(chunks[d.Chunk]), Chunk: d.Chunk, Chunks: len(chunks), Language: d.Language,
	}
	return true
}

// writeRecordingTranslateError maps the call's error to the view's own codes
// and texts. The log line names the error's class, never its text (the call
// seals a backend's answer out of its errors besides: ChainCall.SealErrors).
func writeRecordingTranslateError(w http.ResponseWriter, r *http.Request, err error) {
	var noBackend *backends.ErrNoEligibleBackend
	var quota *backends.ErrQuotaExceeded
	switch {
	case errors.As(err, &noBackend):
		writeRecordingError(w, http.StatusServiceUnavailable, "no_eligible_backend", "no backend of this instance may receive a recording (chat role, full trust, not external)")
	case errors.Is(err, llm.ErrRecordingWindow):
		writeRecordingError(w, http.StatusServiceUnavailable, "window_too_small", "the context window of the chat backends does not hold one chunk")
	case errors.As(err, &quota):
		writeRecordingError(w, http.StatusTooManyRequests, "quota_exceeded", "the tenant quota does not admit the call")
	case errors.Is(err, context.Canceled), r.Context().Err() != nil:
		writeRecordingError(w, http.StatusServiceUnavailable, "canceled", "the translation was canceled")
	default:
		logRequestError(r.Context(), "manage: model-recording-translate failed", "error_class", errorClassOf(err),
			"request_id", RequestIDFromContext(r.Context()))
		writeRecordingError(w, http.StatusBadGateway, "translate_failed", "the model did not answer")
	}
}

// errorClassOf names an error without its text.
func errorClassOf(err error) string {
	switch {
	case llm.IsAdmissionError(err):
		return "admission"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "backend"
}

// isUUID reports whether s parses as a UUID.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

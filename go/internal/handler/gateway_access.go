package handler

import (
	"cmp"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// gatewayRejectionWindow is the window of the throttled refusal line: per
// (consumer, reason) at most one "gateway rejections" line per window,
// carrying the count. A telemetry cadence, not a policy (like the 10-s
// rejection flusher that drives it, design/06 §4.2): it decides how often the
// log speaks about refusals, never whether a request is refused — hence a
// constant, not a setting. B-06-22: a 429 flood logged per request rotates
// the 50 MB × 3 compose log away in minutes, with every error and security
// line in it.
const gatewayRejectionWindow = time.Minute

// GatewayAccessLog is the request log of the model groups (/v1, /v2;
// design/06 §4.6, B-06-22; wave W06-9) — the OUTERMOST stage of every
// group (mountModelGroup), ahead of the enabled gate and the model-key door,
// so the flag's 404 and the door's 401 are counted too. Per request it
// writes either one INFO "gateway request" line (a success, status < 400,
// while gateway.access_log is on) or one count toward a WARN "gateway
// rejections" line (a refusal, status >= 400), which it writes at most once
// per gatewayRejectionWindow per (consumer, reason). handler.Logger stays
// silent for every request the stage saw, so a gateway request is exactly
// one line or one count.
//
// The refusal ledger (W06-12, design/06 §4.2): a refusal some stage or
// handler declared (oai.NoteRejection) goes to the ledger instead — once,
// here, only because the request ended as a refusal — and reaches the window
// through the rejection flusher (CountRejections), under the ledger's reason
// and detail. Only a refusal nobody declared is counted here directly, so
// every refusal is one count either way and the window stays one window.
//
// One per process: /v1 and /v2 share the counts. The lines carry ids,
// counters and the alias — never a consumer label, content, a key, its hint
// or a query string.
type GatewayAccessLog struct {
	settings func() oai.Settings
	log      *slog.Logger
	now      func() time.Time
	// reasons is the refusal vocabulary a stage may name: the gateway
	// codes and the door's own. Anything else counts as http_<status>, so
	// neither the map nor the log line can carry a stage's free text.
	reasons map[string]bool
	// ledger counts the declared refusals (nil: they count here directly).
	ledger *keyquota.Ledger

	mu        sync.Mutex
	counts    map[gatewayRejectionKey]int64
	lastFlush time.Time
}

// gatewayRejectionKey is one aggregation cell: the consumer ("" before the
// door accepted), the reason and the detail (the ledger's; "" for a refusal
// counted here). All are bounded — consumers by the consumer table, reasons
// by the code and ledger vocabularies and the HTTP statuses, details by the
// ledger's — so the map is too. A line covers every detail of one
// (consumer, reason).
type gatewayRejectionKey struct{ consumer, reason, detail string }

// GatewayAccessLogConfig wires the access log.
type GatewayAccessLogConfig struct {
	// Settings is the gateway getter; the stage reads AccessLog per
	// success (hot). Required.
	Settings func() oai.Settings
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// Ledger is the process's refusal ledger (W06-12): a declared refusal
	// is noted there. Nil counts it in the window directly, like an
	// undeclared one.
	Ledger *keyquota.Ledger
}

// NewGatewayAccessLog builds the access log. A missing getter is a wiring
// bug and panics.
func NewGatewayAccessLog(cfg GatewayAccessLogConfig) *GatewayAccessLog {
	if cfg.Settings == nil {
		panic("handler.NewGatewayAccessLog: nil Settings getter")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	reasons := map[string]bool{
		ModelAuthCodeProxyMisconfigured: true, ModelAuthCodeOverloaded: true, ModelAuthCodeUnavailable: true,
	}
	for _, c := range oai.Codes() {
		reasons[c.Code] = true
	}
	return &GatewayAccessLog{
		settings: cfg.Settings, log: log, now: time.Now, reasons: reasons, ledger: cfg.Ledger,
		counts: map[gatewayRejectionKey]int64{}, lastFlush: time.Now(),
	}
}

// Handler is the stage as middleware. It hands the request a fresh
// oai.AccessRecord, which the door (stage 8: consumer, key, tenant; a
// refusal: its code as the reason) and the handlers fill.
func (a *GatewayAccessLog) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		claimRequestLog(w)
		rec := &oai.AccessRecord{}
		aw := &accessWriter{ResponseWriter: w}
		next.ServeHTTP(aw, r.WithContext(oai.WithAccessRecord(r.Context(), rec)))

		status := aw.statusCode()
		if status >= http.StatusBadRequest {
			if rej, ok := rec.Rejection(); ok && a.ledger != nil {
				a.ledger.Note(rej)
				return
			}
			a.reject(rec.ConsumerID, a.rejectionReason(rec, status))
			return
		}
		if !a.settings().AccessLog {
			return
		}
		a.log.Info("gateway request",
			"consumer_id", rec.ConsumerID,
			"model_key_id", rec.ModelKeyID,
			"tenant_id", rec.TenantID,
			"op", rec.Op,
			"alias", rec.Alias,
			"status", status,
			"stream", rec.Stream,
			"prompt_tokens", rec.PromptTokens,
			"completion_tokens", rec.CompletionTokens,
			"usage", rec.Usage,
			"duration_ms", time.Since(start).Milliseconds(),
			"queue_wait_ms", rec.QueueWaitMs,
			"class", rec.Class,
			"hop", rec.Hop,
			"request_id", RequestIDFromContext(r.Context()),
		)
	})
}

// rejectionReason is the refusing stage's reason when it is a code of the
// vocabulary, else the status.
func (a *GatewayAccessLog) rejectionReason(rec *oai.AccessRecord, status int) string {
	if a.reasons[rec.Reason] {
		return rec.Reason
	}
	return "http_" + strconv.Itoa(status)
}

// reject counts one undeclared refusal and, when a window has passed since
// the last flush, flushes — so the counts reach the log even between two
// ticks of the flusher.
func (a *GatewayAccessLog) reject(consumer, reason string) {
	a.mu.Lock()
	a.counts[gatewayRejectionKey{consumer: consumer, reason: reason}]++
	a.mu.Unlock()
	a.flushDue(a.now())
}

// CountRejections adds the refusals the flusher drained from the ledger to
// the window — the ledger's reason and detail per consumer — so a declared
// refusal reaches the log through the same window as an undeclared one
// (W06-12: one window, fed by the rejection flusher, design/06 §4.2).
func (a *GatewayAccessLog) CountRejections(deltas []keyquota.RejectionDelta) {
	if len(deltas) == 0 {
		return
	}
	a.mu.Lock()
	for _, d := range deltas {
		a.counts[gatewayRejectionKey{consumer: d.ConsumerID, reason: d.Reason, detail: d.Detail}] += d.N
	}
	a.mu.Unlock()
}

// Flush writes the counts of the current window now, one line per
// (consumer, reason), and starts a new window (the flusher's last step at
// shutdown).
func (a *GatewayAccessLog) Flush() { a.flush(a.now(), false) }

// FlushDue flushes only when a full window has passed since the last flush
// — the at-most-once-per-window rule shared by the flusher's ticks and the
// request path.
func (a *GatewayAccessLog) FlushDue() { a.flushDue(a.now()) }

func (a *GatewayAccessLog) flushDue(now time.Time) { a.flush(now, true) }

func (a *GatewayAccessLog) flush(now time.Time, onlyDue bool) {
	a.mu.Lock()
	if onlyDue && now.Sub(a.lastFlush) < gatewayRejectionWindow {
		a.mu.Unlock()
		return
	}
	counts := a.counts
	a.counts = map[gatewayRejectionKey]int64{}
	a.lastFlush = now
	a.mu.Unlock()

	type line struct {
		consumer, reason string
		count            int64
		details          []string
	}
	keys := slices.SortedFunc(maps.Keys(counts), func(x, y gatewayRejectionKey) int {
		return cmp.Or(cmp.Compare(x.consumer, y.consumer), cmp.Compare(x.reason, y.reason), cmp.Compare(x.detail, y.detail))
	})
	var lines []line
	for _, k := range keys {
		if n := len(lines); n == 0 || lines[n-1].consumer != k.consumer || lines[n-1].reason != k.reason {
			lines = append(lines, line{consumer: k.consumer, reason: k.reason})
		}
		l := &lines[len(lines)-1]
		l.count += counts[k]
		if k.detail != "" {
			l.details = append(l.details, k.detail)
		}
	}
	window := windowLabel(gatewayRejectionWindow)
	for _, l := range lines {
		a.log.Warn("gateway rejections",
			"consumer_id", l.consumer,
			"reason", l.reason,
			"detail", strings.Join(l.details, ","),
			"count", l.count,
			"window", window,
		)
	}
}

// windowLabel renders a window the way an operator writes it: whole
// minutes as "1m", anything else as time.Duration prints it ("10s", "1m30s").
func windowLabel(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return d.String()
}

// accessWriter records the response status for the access log. Unwrap
// keeps http.ResponseController (flush, deadlines, hijack) working through
// it; Flush serves callers that assert http.Flusher.
type accessWriter struct {
	http.ResponseWriter
	status int
}

func (w *accessWriter) WriteHeader(code int) {
	if w.status == 0 && code >= http.StatusOK {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// FlushError is what http.ResponseController.Flush calls first: the flush
// error of the writer underneath (a client gone mid-stream) reaches the
// caller instead of ending here.
func (w *accessWriter) FlushError() error {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *accessWriter) Flush() { _ = w.FlushError() }

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusCode is the status the client got; a handler that wrote nothing
// answered 200.
func (w *accessWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

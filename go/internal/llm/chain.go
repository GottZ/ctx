package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Admission binds the ONE process-wide dispatch admitter (I-D1) to a call
// site's class (Vorhaben E wave MW3, design/01 §4.6 N1): the CLASS is bound
// by the caller — the query pipeline binds interactive, scheduler arms bind
// background — while target and deadline hint are bound per attempt inside
// the chain walk. The PRINCIPAL is deliberately NOT a field (MW4, design/03
// §4.1.1): the dispatcher derives it exclusively from the Acquire ctx via
// the boot-installed hook, so a stored AuthResult held in a variable cannot
// buy interactive — an interactive class on a ctx without an authenticated
// principal runs into the B8 downgrade. Admission travels as a mandatory
// positional parameter (pattern: the report ReportFunc parameter; llm stays
// parameter-pure): a call site without an admitter does not compile, and a
// zero Admission fails the acquire loudly instead of passing an unadmitted
// wire call.
type Admission struct {
	Admitter dispatch.Admitter
	Class    dispatch.Class
}

// Acquire leases one attempt on the backend's physical origin (per attempt,
// after chain resolution — I-D1). The deadline hint travels admission-
// anchored (Request.DeadlineIn, rule V1: the wire deadline is
// admission+timeout — an enqueue-anchored absolute hint would underestimate
// the reap reference by the wait time). Exported since MW5: the stream
// engine (chat.runStream, N2) and the cross-encoder rerank
// (rrf.RerankCrossEncoder, N4) share the exact acquire form of the
// non-stream chain walk instead of growing drift copies.
func (a Admission) Acquire(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	if a.Admitter == nil {
		return nil, nil, &AdmissionError{Err: fmt.Errorf("llm: %s call site without dispatch admitter (I-D1)", role)}
	}
	enqueued := time.Now()
	lease, runCtx, err := a.Admitter.Acquire(ctx, dispatch.Request{
		Target:     dispatch.Target{Origin: b.Host}, // Acquire normalizes defensively (design/01 §4.3)
		Class:      a.Class,
		Role:       role,
		DeadlineIn: timeout,
	})
	if err != nil {
		// K9 telemetry payload (MW10): the rejected target and the futile
		// wait travel with the error so the rejection line can attribute
		// them (design/05 §3.2 — backend_name = target of the failed
		// acquire, queue_wait_ms = time wasted waiting).
		return nil, nil, &AdmissionError{
			Err:     err,
			Backend: b.Name,
			Host:    b.Host,
			WaitMs:  time.Since(enqueued).Milliseconds(),
		}
	}
	return lease, runCtx, nil
}

// AdmissionError marks a chain walk ended by a failed Acquire. Acquire-error
// doctrine (design/01 §4.3, binding): a failed acquire is NOT an attempt —
// no ChainAttempt entry, no Classify, no health report, and the walk ends
// TERMINALLY (no failover spill onto a following chain link, e.g. openrouter
// under local saturation). The ONE deliberate exception is the K9 rejection
// TELEMETRY line (MW10, design/05 §3.2): RecordRejection persists a
// never-admitted background acquire_expired/queue_full — everything else
// about the doctrine stays. Unwrap keeps errors.Is against the dispatch
// sentinels (IsRejection) and ctx errors intact.
type AdmissionError struct {
	Err error
	// Backend/Host/WaitMs are the K9 rejection telemetry (MW10): the
	// target of the failed acquire and the futile wait before rejection.
	// Zero-valued for the nil-admitter error (nothing waited anywhere).
	Backend string
	Host    string
	WaitMs  int64
}

func (e *AdmissionError) Error() string { return e.Err.Error() }
func (e *AdmissionError) Unwrap() error { return e.Err }

// IsAdmissionError reports whether err is a chain walk ended by a failed
// acquire (no wire contact of its own) — the ONE check point for the
// telemetry sites honoring the no-llmlog-line doctrine.
func IsAdmissionError(err error) bool {
	var ae *AdmissionError
	return errors.As(err, &ae)
}

// ChainAttempt is one tried backend for llmlog's metadata.chain — the full
// provenance of a chained call (the row's backend_name alone only names the
// winner). Since MW10 it also carries the per-attempt lease wait (WaitMs) and
// — for dispatcher-aborted attempts — the abort kind ("preempted"/"reaped")
// as Class instead of the generic "canceled" (design/05 §3.2): the Σ view
// over ALL attempts (incl. waits on failover predecessors) lives here as
// single-case forensics; the row columns carry only the row-defining attempt.
type ChainAttempt struct {
	Backend string `json:"backend"`
	Class   string `json:"err_class"`
	Ms      int64  `json:"ms"`
	WaitMs  int64  `json:"wait_ms"`
}

// ReportFunc feeds attempt outcomes back into the pool's health state.
// ClassOK clears the failure streak; everything else earns the class
// cooldown. Wired to Pool.ReportSuccess/ReportFailure by the caller — llm
// stays free of pool state.
type ReportFunc func(backendID string, class backends.ErrClass, retryAfter time.Duration)

// ChatFunc is the wire call one chain attempt makes. The backend arrives
// fully resolved for the role: Model from ModelFor, Think/Options merged from
// ModelSpec.Params, NumCtx from the row. ChatChainVia callers inject their
// own transport (the dream package routes through its chatJSON test seam);
// ChatChain binds the production chatWithFormat path.
type ChatFunc func(ctx context.Context, b backends.Backend, systemPrompt, userPrompt string, opts Options, timeout time.Duration) (*ChatResponse, error)

// ChatChain tries the chain in order: per attempt its own role timeout
// (httpx.DoRetryOnce stays INSIDE the attempt — transport blip on the same
// connection), Classify decides continuation, the parent context aborts
// immediately. Returns the ANSWERING backend for llmlog provenance plus all
// attempts for metadata.chain.
//
// Per backend, the request derives from the row: ModelFor(role) names the
// model, ModelSpec.Params merge field-wise over the role's code-default
// options (think included), NumCtx comes from the row. defTimeout is the
// role's code default when the row carries no timeouts entry (synthesis 60s,
// translate/temporal 15s — the P2 hardcoded ChatTimeout generalized in P3).
func ChatChain(ctx context.Context, chain []backends.Backend, role string,
	systemPrompt, userPrompt string, baseOpts Options, format string,
	defTimeout time.Duration, report ReportFunc, adm Admission,
) (*ChatResponse, *backends.Backend, []ChainAttempt, error) {
	return ChatChainVia(ctx, func(ctx context.Context, b backends.Backend, sys, usr string, opts Options, timeout time.Duration) (*ChatResponse, error) {
		return chatWithFormat(ctx, b, sys, usr, opts, format, timeout)
	}, chain, role, systemPrompt, userPrompt, baseOpts, defTimeout, report, adm)
}

// ChatChainVia is ChatChain with an injected wire call (G28: the dream
// package walks its chains through the chatJSON test seam). The attempt loop
// — model resolution, params merge, Classify-driven continuation, health
// reporting — lives ONLY here so the failover error doctrine has one home.
func ChatChainVia(ctx context.Context, call ChatFunc, chain []backends.Backend, role string,
	systemPrompt, userPrompt string, baseOpts Options,
	defTimeout time.Duration, report ReportFunc, adm Admission,
) (*ChatResponse, *backends.Backend, []ChainAttempt, error) {
	if len(chain) == 0 {
		return nil, nil, nil, fmt.Errorf("llm: chain for role %q is empty", role)
	}
	if defTimeout <= 0 {
		defTimeout = ChatTimeout
	}

	var attempts []ChainAttempt
	var lastErr error
	for i := range chain {
		b := &chain[i]
		spec := b.ModelFor(role)
		if spec.Model == "" {
			// Validation guarantees coverage; a row edited via psql can
			// still miss it. Skip — a model-less attempt cannot succeed.
			slog.Error("llm: chain backend has no model for role — skipping",
				"backend", b.Name, "role", role)
			attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: "no_model"})
			continue
		}

		resolved := *b // copy: never mutate the pool snapshot
		opts, think := applyModelParams(baseOpts, spec.Params, &resolved)
		resolved.Model = spec.Model
		resolved.Think = thinkModeOf(think)
		timeout := b.TimeoutFor(role, defTimeout)

		// MW3 admission: one lease per attempt on the attempt's physical
		// origin, acquired immediately before the wire call (design/01 §4.6
		// N1); the resolved timeout doubles as the admission-anchored
		// deadline hint. A failed acquire ends the walk terminally per the
		// AdmissionError doctrine — the continue/Classify machinery below
		// never sees it.
		lease, runCtx, admErr := adm.Acquire(ctx, b, role, timeout)
		if admErr != nil {
			return nil, nil, attempts, admErr
		}
		// Lease wait of THIS attempt (admitted − enqueued, the single
		// queue_wait_ms source — design/05 §3.2). Captured before the wire
		// call; the pass-through state yields a real 0.
		waitMs := lease.WaitDur().Milliseconds()

		start := time.Now()
		resp, err := func() (*ChatResponse, error) {
			// defer is the only allowed release form (B1: panic-safe).
			defer lease.Release()
			resp, err := call(runCtx, resolved, systemPrompt, userPrompt, opts, timeout)
			if resp != nil {
				// MW22 meter feed: backend-reported usage in the llmlog
				// dimensions, booked into the fairness window at Release.
				lease.ReportUsage(dispatch.Usage{PromptTokens: resp.PromptTokens, CompletionTokens: resp.EvalCount})
			}
			return resp, err
		}()
		elapsed := time.Since(start)

		if err == nil {
			attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: "ok", Ms: elapsed.Milliseconds(), WaitMs: waitMs})
			if report != nil {
				report(b.ID, backends.ClassOK, 0)
			}
			if i > 0 {
				slog.Info("llm: chain served by non-primary backend",
					"role", role, "backend", b.Name, "attempt", i+1,
					"duration", elapsed.Round(time.Millisecond))
			}
			return resp, b, attempts, nil
		}

		// §4.4c abort rule (MW10, design/05 — BEFORE the generic Classify):
		// a dispatcher-canceled attempt (cause ErrPreempted/ErrReaped on the
		// lease's runCtx) is TERMINAL — no class.Next() failover onto
		// following links (a preempted background prompt must never spill
		// toward openrouter through a scheduling decision) and no report()
		// (the pool health state is shared with interactive; a preempt/reap
		// must not cooldown the target, B-R9). The attempt carries the abort
		// kind instead of the generic "canceled". Classify's own
		// context.Canceled match would only cover an UNwrapped cause — this
		// rule is errors.Is over context.Cause and therefore wrap-safe.
		if abort := dispatchAbortClass(runCtx); abort != "" {
			attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: abort, Ms: elapsed.Milliseconds(), WaitMs: waitMs})
			slog.Warn("llm: chain attempt aborted by dispatcher", "role", role,
				"backend", b.Name, "attempt", i+1, "abort", abort, "error", err)
			return nil, nil, attempts, err
		}

		class := backends.Classify(err, b.ProviderClass)
		attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: class.String(), Ms: elapsed.Milliseconds(), WaitMs: waitMs})
		lastErr = err
		if report != nil && class != backends.ClassCanceled {
			report(b.ID, class, retryAfterOf(err))
		}
		// Full error (URLs, provider bodies) goes to slog only.
		slog.Warn("llm: chain attempt failed", "role", role, "backend", b.Name,
			"attempt", i+1, "class", class.String(), "error", err)

		if ctx.Err() != nil {
			// Request canceled / parent deadline: stop immediately
			// (today's semantics — the caller's time budget is spent).
			return nil, nil, attempts, lastErr
		}
		if !class.Next() {
			// STOP classes: 500 (the server RAN the request — often
			// deterministic per prompt, the doctrine anchor) and the
			// attempt timeout (slow-but-alive is not down).
			return nil, nil, attempts, lastErr
		}
	}
	return nil, nil, attempts, fmt.Errorf("llm: chain exhausted for role %q: %w", role, lastErr)
}

// dispatchAbortClass classifies a dispatcher-caused cancel of one attempt's
// runCtx (MW10, design/05 §4.4c/B-R8): "preempted"/"reaped" via errors.Is
// over context.Cause — wrap-safe, NEVER sentinel identity (a decorated
// cause, e.g. fmt.Errorf with %w plus target origin, would silently fall to
// "" under ==) and never generic ctx.Err(). Everything else — parent cancel
// (shutdown, dream-off, client disconnect), plain wire errors — returns "".
func dispatchAbortClass(runCtx context.Context) string {
	cause := context.Cause(runCtx)
	switch {
	case errors.Is(cause, dispatch.ErrPreempted):
		return llmlog.AbortPreempted
	case errors.Is(cause, dispatch.ErrReaped):
		return llmlog.AbortReaped
	default:
		return ""
	}
}

// applyDispatchTelemetry derives the MW10 telemetry columns of one llmlog
// row from a chain walk's attempts (design/05 §3.2/§4.4a/§4.4b, shared by
// ChainCall.Do and Synthesize — one derivation, no duplicate code). The
// row-defining attempt is the LAST wire attempt: the answering one on
// success (the walk returns immediately), the last tried on full failure —
// consistent with the row's backend_name attribution. Entry.Duration becomes
// that attempt's wire elapsed (§4.4a duration fix: the old chain-walk span
// contained every lease wait of every attempt and would double-count
// queue_wait_ms under enforcement — exactly in the state the telemetry is
// meant to measure). QueueWaitMs keeps 0 as a real measurement (B-R4).
// "no_model" pseudo-attempts (no lease, no wire) never define the row; a
// walk with ONLY those leaves QueueWaitMs nil and Duration zero.
func applyDispatchTelemetry(entry *llmlog.Entry, attempts []ChainAttempt, class dispatch.Class) {
	entry.DispatchClass = class.String()
	for i := len(attempts) - 1; i >= 0; i-- {
		a := attempts[i]
		if a.Class == "no_model" {
			continue
		}
		entry.Duration = time.Duration(a.Ms) * time.Millisecond
		w := a.WaitMs
		entry.QueueWaitMs = &w
		if a.Class == llmlog.AbortPreempted || a.Class == llmlog.AbortReaped {
			entry.DispatchAbort = a.Class
		}
		return
	}
}

// rejectionEntry builds the ONE K9 rejection telemetry line (MW10,
// design/05 §3.2 — the narrow exception to the acquire-error doctrine of
// design/01 §4.3): a never-admitted BACKGROUND acquire that expired while
// waiting (acquire_expired) or bounced off the full background queue
// (queue_full) persists its futile wait. Without it, starvation under
// permanent interactive demand would produce exactly zero rows and every
// p95 would measure survivors only (survivorship bias, design/05 §6).
// Everything else stays doctrine: no attempt, no Classify, no health
// report, terminal. ok==false for every case that writes nothing:
// interactive rejections (cap ladder — the error goes to the caller, the
// class invariant keeps scheduler traces out of tenant-visible rows) and
// parent cancels (shutdown/dream-off are lifecycle, not admission verdicts).
// Since MW11 the embed walk's mirror error type is recognized too — the K9
// line has ONE shape regardless of which pipeline family was rejected.
func rejectionEntry(pipeline string, err error, class dispatch.Class) (llmlog.Entry, bool) {
	if class != dispatch.ClassBackground {
		return llmlog.Entry{}, false
	}
	var abort string
	switch {
	case errors.Is(err, dispatch.ErrQueueFull):
		abort = llmlog.AbortQueueFull
	case errors.Is(err, context.DeadlineExceeded):
		abort = llmlog.AbortAcquireExpired
	default:
		return llmlog.Entry{}, false
	}
	var host, backend string
	var w int64
	var ae *AdmissionError
	var eae *embedcache.AdmissionError
	switch {
	case errors.As(err, &ae):
		host, backend, w = ae.Host, ae.Backend, ae.WaitMs
	case errors.As(err, &eae):
		host, backend, w = eae.Host, eae.Backend, eae.WaitMs
	default:
		return llmlog.Entry{}, false
	}
	return llmlog.Entry{
		Pipeline:      pipeline,
		Host:          host,
		BackendName:   backend,
		Err:           err,
		QueueWaitMs:   &w,
		DispatchClass: class.String(),
		DispatchAbort: abort,
		NoWireCall:    true, // no physical call — duration_ms stays NULL
	}, true
}

// RecordRejection persists the K9 rejection line (async, fire-and-forget
// like every llmlog write). Safe on every acquire-failure path — the
// non-line cases are filtered inside rejectionEntry. Exported since MW11:
// the background embed call sites (scheduler backfill, dream keyword
// embeds) hook their never-admitted acquires here — EmbedChain's wired
// flag stays false on that path, so their regular llmlog gate writes
// nothing and starvation would otherwise be invisible.
func RecordRejection(db *pgxpool.Pool, pipeline string, err error, class dispatch.Class) {
	if entry, ok := rejectionEntry(pipeline, err, class); ok {
		llmlog.Record(db, entry)
	}
}

// ApplyDispatchOutcome is the dispatch-outcome funnel for SELF-LOGGING
// pipelines — call sites that build and defer-Record their own llmlog entry
// (the five dream chat pipelines via dream's applyChainTelemetry, design/05
// §4.4b: ONE place, five pipelines). Two halves:
//
//   - Walk reached the wire (or failed without an admission error): the
//     MW10 column derivation from the row-defining attempt, identical to
//     ChainCall.Do (applyDispatchTelemetry).
//   - Walk ended by a NEVER-ADMITTED acquire (no wire contact): the entry
//     must not persist as a wire-shaped error row (acquire-error doctrine
//     design/01 §4.3 — closes the MW3 gap where the sites' deferred Record
//     fired anyway). A background queue_full/acquire_expired rejection
//     REPLACES the entry with the uniform K9 line (bodies dropped, duration
//     NULL); every other admission failure (interactive rejection, parent
//     cancel, nil admitter) BLANKS it — llmlog.Record skips an entry
//     without a pipeline, so the deferred Record becomes a no-op.
func ApplyDispatchOutcome(entry *llmlog.Entry, attempts []ChainAttempt, err error, class dispatch.Class) {
	if err != nil && len(attempts) == 0 && IsAdmissionError(err) {
		if rej, ok := rejectionEntry(entry.Pipeline, err, class); ok {
			*entry = rej
		} else {
			*entry = llmlog.Entry{}
		}
		return
	}
	applyDispatchTelemetry(entry, attempts, class)
}

// applyModelParams merges ModelSpec.Params field-wise over the code-default
// options and resolves the think toggle (params.think wins over the row's
// legacy Think field). NumCtx always comes from the row.
//
// Options.NumPredictScale is applied LAST, on the merged result: a caller that
// wants a wider output cap for one attempt (dream's bounded cap-hit retry)
// means the cap this attempt will actually send, and that is only known here —
// after a model_map num_predict/max_tokens param has overwritten the caller's
// value. Scaling on the caller side instead would be a no-op on exactly the
// rows that set such a param. resolveMaxOut (autowindow) walks this same
// function, so the window math sees the scaled cap too.
func applyModelParams(base Options, params map[string]any, b *backends.Backend) (Options, *bool) {
	think := b.Think.Ptr()
	if b.NumCtx > 0 {
		base.NumCtx = b.NumCtx
	}
	for k, v := range params {
		switch k {
		case "temperature":
			base.Temperature = toFloat(v, base.Temperature)
		case "top_p":
			base.TopP = toFloat(v, base.TopP)
		case "top_k":
			base.TopK = int(toFloat(v, float64(base.TopK)))
		case "min_p":
			base.MinP = toFloat(v, base.MinP)
		case "repeat_penalty":
			base.RepeatPenalty = toFloat(v, base.RepeatPenalty)
		case "presence_penalty":
			base.PresencePenalty = toFloat(v, base.PresencePenalty)
		case "num_predict", "max_tokens":
			base.NumPredict = int(toFloat(v, float64(base.NumPredict)))
		case "think":
			if tv, ok := v.(bool); ok {
				t := tv
				think = &t
			}
		default:
			// Passthrough: a model_map param with no dedicated Options field
			// (chat_template_kwargs, provider-specific knobs) is carried to
			// the OpenAI wire path via Options.Extra instead of being
			// silently dropped. The documented contract — "model_map params
			// override the code default at dispatch" (evaluate.go
			// DreamOptions comment) — previously only honoured the fixed
			// Options keys, so e.g. a chat_template_kwargs.enable_thinking
			// disable in the serving row never reached the request, while
			// chatOpenAI's OpenRouter-only `reasoning` field went out instead
			// (a shape non-OpenRouter providers like vLLM/LiteLLM ignore →
			// thinking stays on, output runs into the token cap, structured
			// JSON answers arrive truncated).
			if base.Extra == nil {
				base.Extra = make(map[string]any)
			}
			base.Extra[k] = v
		}
	}
	if base.NumPredictScale > 1 && base.NumPredict > 0 {
		base.NumPredict = int(float64(base.NumPredict) * base.NumPredictScale)
	}
	return base, think
}

func toFloat(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return def
	}
}

// thinkModeOf folds the merged think toggle back into the Backend field so a
// resolved backend carries it through ChatFunc (the wire form *bool and the
// row form ThinkMode are losslessly interchangeable).
func thinkModeOf(think *bool) backends.ThinkMode {
	switch {
	case think == nil:
		return ""
	case *think:
		return "true"
	default:
		return "false"
	}
}

// LogEmbedWire records one slim llmlog row for an embed wire-call sequence:
// full backend/trust/locality/required/attempt telemetry + block_ids, NO
// bodies (§2.7.3 — embeds carry no prompt worth storing). Callers gate on
// EmbedChain's wired flag — cache hits contact no backend and log nothing.
// MW11 (design/05 §4.4a/§4.4b): the row derives its telemetry columns from
// the walk's attempts — duration_ms is the row-defining attempt's WAIT-FREE
// wire span (the old caller-measured sequence span contained every lease
// wait and would double-count queue_wait_ms under enforcement), the full
// per-attempt slice lands in metadata.chain, and class travels from the
// caller's admission binding.
func LogEmbedWire(db *pgxpool.Pool, pipeline, role string, required backends.Sensitivity,
	served *backends.Backend, attempts []embedcache.WireAttempt, blockIDs []string, err error,
	apiKeyID string, class dispatch.Class,
) {
	llmlog.Record(db, embedWireEntry(pipeline, role, required, served, attempts, blockIDs, err, apiKeyID, class))
}

// embedWireEntry builds the embed row (split from LogEmbedWire so the column
// derivation is probe-able without a DB — Record is fire-and-forget).
func embedWireEntry(pipeline, role string, required backends.Sensitivity,
	served *backends.Backend, attempts []embedcache.WireAttempt, blockIDs []string, err error,
	apiKeyID string, class dispatch.Class,
) llmlog.Entry {
	chain := make([]ChainAttempt, len(attempts))
	var promptTokens int
	for i, a := range attempts {
		chain[i] = ChainAttempt{Backend: a.Backend, Class: a.Class, Ms: a.Ms, WaitMs: a.WaitMs}
		// D1(a): the serving attempt carries the embed prompt-token count; a
		// sequence has at most one "ok" attempt (it returns on success), so the
		// running max is exactly the served count without double-counting.
		if a.PromptTokens > promptTokens {
			promptTokens = a.PromptTokens
		}
	}
	entry := llmlog.Entry{
		Pipeline:            pipeline,
		Err:                 err,
		BlockIDs:            blockIDs,
		RequiredSensitivity: string(required),
		Attempt:             len(attempts),
		APIKeyID:            apiKeyID, // T35b: caller attribution (NULL for background backfill/dream)
	}
	if promptTokens > 0 {
		entry.PromptTokens = promptTokens
	}
	if len(chain) > 0 {
		entry.Metadata = map[string]any{"chain": chain}
	}
	if served != nil {
		entry.Model = served.ModelFor(role).Model
		entry.Host = served.Host
		entry.BackendName = served.Name
		entry.BackendTrust = string(served.Trust)
		entry.BackendLocality = served.Locality
	}
	applyDispatchTelemetry(&entry, chain, class)
	return entry
}

// retryAfterOf extracts the parsed Retry-After of a 429 (zero otherwise).
func retryAfterOf(err error) time.Duration {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

// dropExternal removes external-locality backends from a resolved chain.
// Slog (not a client error) — the row passed the trust gate, the locality
// border is an additional constraint of THIS call class.
func dropExternal(chain []backends.Backend, role string) []backends.Backend {
	kept := chain[:0:0]
	for i := range chain {
		if chain[i].Locality == backends.LocalityExternal {
			slog.Warn("llm: local-only call dropped external backend",
				"role", role, "backend", chain[i].Name)
			continue
		}
		kept = append(kept, chain[i])
	}
	return kept
}

// PoolReporter wires ChatChain attempt outcomes back into the pool's health
// state (the same closure every chained call site needs — extracted in P3).
func PoolReporter(bpool *backends.Pool) ReportFunc {
	return func(id string, class backends.ErrClass, ra time.Duration) {
		if class == backends.ClassOK {
			bpool.ReportSuccess(id)
		} else {
			bpool.ReportFailure(id, class, ra)
		}
	}
}

// ChainCall is one chained Q-only chat operation (translate, temporal) with
// its llmlog SLIM row: full backend/trust/locality/required/attempt telemetry
// + optional block_ids, NO prompt bodies — closes the llm.md §5 coverage gap
// (translate/temporal/rerank/embed were unlogged) at ~0 storage cost. One row
// per WIRE-call sequence; an empty chain writes nothing (no wire contact).
type ChainCall struct {
	Pool *backends.Pool
	Role string
	// Tenant bounds Chain() to the caller's visible backends (04-W2/T34).
	// TENANT-DECISION(chaincall-tenant): the translate/temporal/rerank/classify
	// callers leave it "" — they are NOT in 04-W2's 5-site request-path scope,
	// so they see only shared '_global' backends (behavior-identical today, all
	// backends are '_global'; fail-closed — never a foreign tenant-private one).
	// Threading ar.HomeScope here is a later refinement if those Q-only roles
	// should reach a tenant's own private backend.
	Tenant     string
	Required   backends.Sensitivity
	Pipeline   string // llmlog pipeline name, e.g. "query-translate"
	System     string
	User       string
	Opts       Options
	Format     string // "" | "json"
	DefTimeout time.Duration
	BlockIDs   []string
	// LocalOnly drops external backends AFTER the trust gate (G41 classify:
	// audit prompts carry unclassified block content — the locality border
	// holds even against a full-trust external row a psql edit smuggled past
	// the 422 validation).
	LocalOnly bool
	// APIKeyID is the caller attribution for the slim llmlog row (04-W3/T35b,
	// design/04 §4.3). Foreground callers (query translate/temporal/rerank)
	// pass ar.ApiKeyID; background callers (sensitivity-audit classify) leave
	// it "" — nullUUID (llmlog.go:168) drops an empty key to NULL, keeping the
	// background invariant (Dream/Scheduler carry no caller).
	APIKeyID string
	// OnServed, when set, reports WHICH backend produced the response — the
	// provenance the *ChatResponse alone cannot carry (ServedModel is filled by
	// OpenRouter only; a local backend leaves it empty). Purely additive and
	// purely observational: it fires once, after the walk, only when a backend
	// answered, and no existing call site sets it.
	//
	// Added for the cluster-label pipeline (Cluster-Topic-Map W6), which
	// persists label_model per topic as label PROVENANCE — "which model named
	// this" has to be the model that actually answered, not the one the pool
	// would have picked.
	OnServed func(backend, model string)
}

// Do resolves the chain (the trust gate: an excluded backend does not exist
// for this call), walks it via ChatChain and records the slim llmlog row.
// An empty chain returns *backends.ErrNoEligibleBackend — the call site
// decides its role's fail-open/fail-hard semantics (design 03 §2.4).
// adm is the caller-bound dispatch admission (MW3): a walk that ends by
// acquire failure WITHOUT any wire contact writes no regular llmlog row
// (doctrine §4.3) — only the K9 rejection line where it applies (MW10).
func (c ChainCall) Do(ctx context.Context, db *pgxpool.Pool, adm Admission) (*ChatResponse, error) {
	chain, err := c.Pool.Chain(c.Role, c.Required, c.Tenant)
	if err != nil {
		return nil, err
	}
	if c.LocalOnly {
		chain = dropExternal(chain, c.Role)
		if len(chain) == 0 {
			return nil, &backends.ErrNoEligibleBackend{
				Role: c.Role, Required: c.Required,
				Excluded: []backends.ExclusionReason{{Backend: "*", Reason: "local-only call, all eligible backends external"}},
			}
		}
	}

	resp, served, attempts, err := ChatChain(ctx, chain, c.Role,
		c.System, c.User, c.Opts, c.Format, c.DefTimeout, PoolReporter(c.Pool), adm)

	if err != nil && len(attempts) == 0 && IsAdmissionError(err) {
		// Never admitted, no wire contact: no regular llmlog row
		// (acquire-error doctrine §4.3) — only the K9 rejection line for
		// background acquire_expired/queue_full (MW10, design/05 §3.2).
		RecordRejection(db, c.Pipeline, err, adm.Class)
		return nil, err
	}

	entry := llmlog.Entry{
		Pipeline:            c.Pipeline,
		Err:                 err,
		BlockIDs:            c.BlockIDs,
		RequiredSensitivity: string(c.Required),
		Attempt:             len(attempts),
		Metadata:            map[string]any{"chain": attempts},
		APIKeyID:            c.APIKeyID, // T35b: caller attribution (NULL for background)
	}
	if served != nil {
		entry.Model = served.ModelFor(c.Role).Model
		entry.Host = served.Host
		entry.BackendName = served.Name
		entry.BackendTrust = string(served.Trust)
		entry.BackendLocality = served.Locality
		if c.OnServed != nil {
			c.OnServed(served.Name, entry.Model)
		}
	}
	if resp != nil {
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
		applyProviderTelemetry(&entry, resp)
	}
	// MW10: queue_wait_ms/class/abort plus the wait-free Duration from the
	// row-defining attempt (§4.4a — replaces the old chain-walk span).
	applyDispatchTelemetry(&entry, attempts, adm.Class)
	llmlog.Record(db, entry)

	return resp, err
}

// applyProviderTelemetry copies the OpenRouter provenance of a response into
// the llmlog row (G29, design 03 §2.7.4): cost_usd from usage.cost, the model
// column overwritten with the model that ACTUALLY answered (models-fallback),
// response.id as metadata.provider_request_id for async audit. Local backends
// carry zero values — the entry stays untouched.
func applyProviderTelemetry(entry *llmlog.Entry, resp *ChatResponse) {
	entry.CostUSD = resp.CostUSD
	if resp.ServedModel != "" {
		entry.Model = resp.ServedModel
	}
	if resp.ProviderRequestID != "" {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["provider_request_id"] = resp.ProviderRequestID
	}
}

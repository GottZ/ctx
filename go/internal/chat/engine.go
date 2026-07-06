// Package chat — engine.go is the server-side harness loop (design 06 §3.6).
//
// RunTurn drives the tool loop headless: it claims the session turn, persists
// the user message, then iterates model call → tool execution → next call,
// raising the session sensitivity HWM in lock-step so every backend pick is
// gated on max(request, history) sensitivity (§2.3). Events flow out through the
// Sink interface — the loop knows nothing about HTTP, so a later headless agent
// runner can drive it with a log/collector sink (Workflow-Engine line, §2.2).
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

// ErrTurnBusy is returned by RunTurn when the session already has an active
// turn (busy_until in the future). No sink event is emitted before it, so the
// handler can still answer a clean 409 if it has not started the stream.
var ErrTurnBusy = errors.New("chat: session has an active turn")

// Sink receives the turn's events. The HTTP handler adapts an SSE writer onto
// it; tests use a collector. An Event error (client gone) aborts the turn.
type Sink interface {
	Event(typ string, data any) error
}

// BackendProvider is the engine's narrow view of the F3 pool: the trust-ordered
// chain of chat backends that may receive content of the given sensitivity,
// best first. An empty chain is *backends.ErrNoEligibleBackend (never a silent
// escalation). PoolProvider wraps *backends.Pool; tests inject a fake.
type BackendProvider interface {
	// tenant is the session OWNER's scope (sess.Scope, NOT sess.ReadScopes —
	// routing by readability would expose a foreign tenant's private backends).
	// It bounds the chat chain to the tenant's visible backends (04-W2/T34).
	ChatChain(ctx context.Context, required backends.Sensitivity, tenant string) ([]backends.Backend, error)
}

// Config carries the web-chat knobs (filled from the F1 snapshot by the handler;
// tests fill it directly). The engine is config-package agnostic.
type Config struct {
	MaxIterations      int
	MaxTokens          int
	CompletionBudget   int
	ToolResultMaxChars int
	HistoryBudgetChars int
	LLMTimeout         time.Duration
	BusyTTL            time.Duration
	Timezone           string
}

func (c Config) withDefaults() Config {
	if c.MaxIterations <= 0 {
		c.MaxIterations = 6
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = 2048
	}
	if c.CompletionBudget <= 0 {
		c.CompletionBudget = 8192
	}
	if c.HistoryBudgetChars <= 0 {
		c.HistoryBudgetChars = 60000
	}
	if c.LLMTimeout <= 0 {
		c.LLMTimeout = 900 * time.Second
	}
	if c.BusyTTL <= 0 {
		c.BusyTTL = 15 * time.Minute
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	return c
}

// Engine runs chat turns against a session store, a backend provider and the
// tool executor.
type Engine struct {
	pool     *pgxpool.Pool
	provider BackendProvider
	exec     *Executor
	cfg      Config
	now      func() time.Time
	// admitter is the ONE process-wide dispatch admission layer (MW5, I-D1);
	// every stream wire call of the turn loop acquires through it. nil is
	// tolerated for tests that never reach a stream attempt — the acquire
	// then fails loudly (no unadmitted wire call), never silently passes.
	admitter dispatch.Admitter
}

// NewEngine builds the harness. exec carries the tool registry + executor.
// admitter is the dispatch admission layer (MW5, design/01 §4.6 N2) — a
// mandatory positional parameter like the query handler's (a call site
// without an admitter does not compile).
func NewEngine(pool *pgxpool.Pool, provider BackendProvider, exec *Executor, cfg Config, admitter dispatch.Admitter) *Engine {
	return &Engine{pool: pool, provider: provider, exec: exec, cfg: cfg.withDefaults(), now: time.Now, admitter: admitter}
}

// admission binds the turn's dispatch class (design/01 §4.6 N2): a web-chat
// turn is interactive — a human waits on the stream. The principal is NOT
// bound here (MW4, design/03 §4.1.1): the dispatcher derives it from the
// request ctx that flows into the acquire; a ctx without an AuthResult
// yields the empty principal, which downgrades fail-closed to background (B8).
func (e *Engine) admission() llm.Admission {
	return llm.Admission{Admitter: e.admitter, Class: dispatch.ClassInteractive}
}

// RunTurn drives one user turn to completion, emitting events through sink.
// toolsEnabled is the request's tool toggle (the per-backend trust gate applies
// on top). It returns ErrTurnBusy when the session is busy; all other terminal
// conditions are reported via the sink (error / done events) and RunTurn
// returns nil — or the Sink's error if the client vanished.
func (e *Engine) RunTurn(ctx context.Context, ar *auth.AuthResult, sess *store.ChatSession, userMsg string, reqSens backends.Sensitivity, toolsEnabled bool, sink Sink) error {
	// E6 session-suspend-cut (T05c): re-check the session OWNER's tenant status
	// every turn, BEFORE any work. sess.ReadScopes is frozen at session start
	// (store/chat.go), so a tenant suspended mid-session would otherwise keep
	// running tools under that snapshot (R-LEAK6) — the auth-time ctx_auth gate
	// (060) never re-fires for an already-open session. A non-active owner ⇒ the
	// turn is rejected outright: no claim, no persisted message, no tool, no
	// corpus hit ("suspended = fully silent", addendum §6.4).
	if active, serr := e.ownerActive(ctx, sess); serr != nil {
		return fmt.Errorf("chat: owner tenant status: %w", serr)
	} else if !active {
		return sink.Event("error", map[string]any{"code": "tenant_suspended", "retryable": false})
	}

	claimed, err := store.ClaimTurn(ctx, e.pool, sess.ID, e.cfg.BusyTTL)
	if err != nil {
		return fmt.Errorf("chat: claim turn: %w", err)
	}
	if !claimed {
		return ErrTurnBusy
	}
	// Release over a cancel-immune context: after a client abort r.Context() is
	// already canceled and the reset would be lost (the query-path trap, §3.6).
	defer func() {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := store.ReleaseTurn(rc, e.pool, sess.ID); err != nil {
			slog.Warn("chat: release turn", "session", sess.ID, "error", err)
		}
	}()

	apiKeyID := ""
	if ar != nil {
		apiKeyID = ar.ApiKeyID
	}
	start := e.now()
	reqSens = normalizeSens(reqSens)
	turnMax := reqSens

	// Persist the user message; the HWM rises in the same TX (store layer).
	userMsg = strings.TrimSpace(userMsg)
	userRow, sessHWM, err := store.AppendMessage(ctx, e.pool, sess.ID, store.NewMessage{Role: "user", Content: userMsg, Sensitivity: reqSens})
	if err != nil {
		return fmt.Errorf("chat: persist user message: %w", err)
	}
	if userRow.Seq == 1 {
		_ = store.SetTitleIfDefault(ctx, e.pool, sess.ID, store.DeriveTitle(userMsg))
	}
	if err := sink.Event("session", map[string]any{"session_id": sess.ID, "user_seq": userRow.Seq}); err != nil {
		return err
	}

	history, err := store.ListMessages(ctx, e.pool, sess.ID, 0, 0)
	if err != nil {
		return fmt.Errorf("chat: load history: %w", err)
	}
	msgs := append([]llm.ChatMsg{{Role: "system", Content: e.systemPrompt()}}, e.buildHistory(history)...)

	budgetLeft := e.cfg.CompletionBudget
	finalReason := "tool_limit"
	adm := e.admission()

	for iter := 1; iter <= e.cfg.MaxIterations; iter++ {
		required := backends.MaxSensitivity(reqSens, sessHWM)
		chain, cerr := e.provider.ChatChain(ctx, required, sess.Scope)
		if cerr != nil || len(chain) == 0 {
			return emitNoEligible(sink)
		}

		so := e.runStream(ctx, chain, msgs, toolsEnabled, budgetLeft, iter, sess.ID, apiKeyID, required, adm, sink)
		if !so.served {
			return e.finishUnserved(ctx, sess, so, turnMax, start, iter, sink)
		}
		res := so.result
		budgetLeft -= res.CompletionTokens
		if serr := sink.Event("usage", map[string]any{
			"iteration": iter, "prompt_tokens": res.PromptTokens,
			"completion_tokens": res.CompletionTokens, "draft_accept": res.DraftAccept,
		}); serr != nil {
			return serr
		}

		if res.FinishReason != "tool_calls" {
			arow, _, perr := store.AppendMessage(ctx, e.pool, sess.ID, store.NewMessage{
				Role: "assistant", Content: res.Content, Sensitivity: turnMax,
				Backend: so.backend.Name, Model: so.model,
				PromptTokens: intPtr(res.PromptTokens), CompletionTokens: intPtr(res.CompletionTokens),
				DurationMs: intPtr(int(so.durationMs)),
			})
			if perr != nil {
				return fmt.Errorf("chat: persist assistant: %w", perr)
			}
			return sink.Event("done", map[string]any{
				"finish_reason": doneReason(res.FinishReason),
				"assistant_seq": arow.Seq, "iterations": iter,
				"total_ms": e.now().Sub(start).Milliseconds(),
			})
		}

		// tool_calls: persist the assistant call, run each tool, append results.
		if _, _, perr := store.AppendMessage(ctx, e.pool, sess.ID, store.NewMessage{
			Role: "assistant", Content: res.Content, ToolCalls: marshalToolCalls(res.ToolCalls), Sensitivity: turnMax,
			Backend: so.backend.Name, Model: so.model,
			PromptTokens: intPtr(res.PromptTokens), CompletionTokens: intPtr(res.CompletionTokens), DurationMs: intPtr(int(so.durationMs)),
		}); perr != nil {
			return fmt.Errorf("chat: persist assistant tool_calls: %w", perr)
		}
		msgs = append(msgs, llm.ChatMsg{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls})

		for _, call := range res.ToolCalls {
			if serr := sink.Event("tool_call", map[string]any{
				"iteration": iter, "id": call.ID, "name": call.Function.Name,
				"arguments": json.RawMessage(call.Function.Arguments),
			}); serr != nil {
				return serr
			}
			outcome := e.exec.Run(ctx, sess.ReadScopes, apiKeyID, call)
			toolRow, hwm, perr := store.AppendMessage(ctx, e.pool, sess.ID, store.NewMessage{
				Role: "tool", Content: outcome.Content, Sensitivity: outcome.Sensitivity,
				ToolCallID: call.ID, ToolName: call.Function.Name,
				DurationMs: intPtr(int(outcome.DurationMs)),
			})
			if perr != nil {
				return fmt.Errorf("chat: persist tool result: %w", perr)
			}
			sessHWM = hwm
			turnMax = backends.MaxSensitivity(turnMax, outcome.Sensitivity)
			msgs = append(msgs, llm.ChatMsg{Role: "tool", Content: outcome.Content, ToolCallID: call.ID})
			_ = toolRow
			if serr := sink.Event("tool_result", map[string]any{
				"iteration": iter, "id": call.ID, "name": call.Function.Name,
				"ok": outcome.OK, "duration_ms": outcome.DurationMs, "chars": outcome.Chars,
				"truncated": outcome.Truncated, "summary": outcome.Summary, "blocks": outcome.Blocks,
				// staged is the D-W6b ConfirmCard payload (nil → null for every
				// read tool; the SPA treats null as "no card").
				"staged": outcome.Staged,
			}); serr != nil {
				return serr
			}
		}

		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = store.RefreshTurn(rc, e.pool, sess.ID, e.cfg.BusyTTL)
		cancel()

		if budgetLeft <= 0 {
			finalReason = "budget"
			break
		}
	}

	// Iteration cap or budget reached: ONE closing call WITHOUT a tools array
	// (E4: never tool_choice:"none" — it emits tool syntax as plain text).
	return e.finalCall(ctx, sess, msgs, reqSens, sessHWM, turnMax, start, apiKeyID, finalReason, adm, sink)
}

// ownerActive resolves the session owner's tenant status fresh (the E6 per-turn
// suspend-cut, T05c). The owner is sess.Scope — the home_scope of the creating
// key (store/chat.go) — which Modell C maps to exactly one tenant. A scope with
// no tenant mapping (single-tenant transition) has nothing to suspend ⇒ active.
//
// TENANT-DECISION(session-suspend-cut): per-turn owner-status (re-)check at the
// engine turn entry via sess.Scope → context_tenant_scopes → context_tenants
// (addendum §6.4). Alternative A: gate only at session start (frozen
// sess.ReadScopes would keep running = R-LEAK6). Alternative B: re-auth via
// ctx_auth (gates the CALLER's tenant, not the session OWNER's). Umentscheidbar
// weil ein Status-Lookup pro Turn — eine Naht, keine Architektur-Änderung. The
// fail-closed scope-intersection hardening (design/01 §5.2-a) + RequireScopes
// wiring (§5.4) are T07's defense-in-depth behind this primary cut, not T05c.
func (e *Engine) ownerActive(ctx context.Context, sess *store.ChatSession) (bool, error) {
	status, found, err := store.TenantStatusForScope(ctx, e.pool, sess.Scope)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	return status == "active", nil
}

// streamOutcome is one iteration's stream result plus failover bookkeeping.
type streamOutcome struct {
	result     *llm.StreamResult
	backend    backends.Backend
	model      string
	served     bool
	partial    string
	durationMs int64
	err        error
	canceled   bool
}

// runStream runs the chain for one iteration: it tries each trust-eligible
// backend, failing over to the next ONLY before the first emitted byte and only
// on a Next()-class error (F3 doctrine). After the first token a dead stream is
// terminal — no silent re-run / double tool execution.
//
// queuedEventInterval is the "queued" SSE keepalive cadence while a stream
// acquire waits in the dispatcher queue (design/03 §4.4 V2b / C6). Well under
// 30 s so it clears a typical ~60 s reverse-proxy idle timeout with margin;
// a package var so tests can shrink it.
var queuedEventInterval = 20 * time.Second

// acquireQueued wraps the blocking stream acquire with a periodic "queued"
// keepalive event (Muster S8): without a byte during a long queue wait the
// turn dies silently in the proxy (C6 — at target scale the HÄUFIGE case, one
// foreign stream lease holds the 1-slot target while the proxy idle-kills the
// waiting turn). The acquire runs on the caller ctx, so a client disconnect
// cancels it and vacates the wait slot exactly as before (design/01 §4.2); the
// goroutine always terminates when Acquire returns. The event is generic — no
// target, no depth, no wait estimate (same C2/B6 doctrine as the rejection
// body). Only the select loop touches the sink (the acquire goroutine never
// does), so there is no concurrent sink access. An immediate admission (slot
// free) emits NO event — the first tick is one interval away.
func (e *Engine) acquireQueued(ctx context.Context, adm llm.Admission, b *backends.Backend, iter int, sink Sink) (*dispatch.Lease, context.Context, error) {
	type acqResult struct {
		lease  *dispatch.Lease
		runCtx context.Context
		err    error
	}
	done := make(chan acqResult, 1)
	go func() {
		lease, runCtx, err := adm.Acquire(ctx, b, "chat", e.cfg.LLMTimeout)
		done <- acqResult{lease, runCtx, err}
	}()
	t := time.NewTicker(queuedEventInterval)
	defer t.Stop()
	for {
		select {
		case r := <-done:
			return r.lease, r.runCtx, r.err
		case <-t.C:
			// Keepalive only — a dead sink surfaces at the next real event, and
			// the acquire goroutine owns termination via ctx cancel.
			_ = sink.Event("queued", map[string]any{"iteration": iter})
		}
	}
}

// MW5 admission (design/01 §4.6 N2): each attempt's wire call runs under its
// own lease on the attempt's origin, held over the WHOLE stream (ChatStream
// consumes the SSE body before returning — the longest GPU occupancy case)
// and released at stream end, error included. The stream-end usage charges at
// Release (C1: stream usage only exists at the end); an aborted/preempted
// stream without a result stays uncharged. A failed acquire is TERMINAL
// (doctrine §4.3): no wire contact, no Classify, no health signal, no llmlog
// row (recordLLM is skipped), no failover onto the next chain link.
func (e *Engine) runStream(ctx context.Context, chain []backends.Backend, msgs []llm.ChatMsg, toolsEnabled bool, budgetLeft, iter int, sessID, apiKeyID string, required backends.Sensitivity, adm llm.Admission, sink Sink) streamOutcome {
	var lastErr error
	var lastBackend backends.Backend
	for i := range chain {
		b := chain[i]
		lastBackend = b
		model := b.ModelFor("chat").Model
		if model == "" {
			lastErr = fmt.Errorf("backend %q has no chat model", b.Name)
			continue
		}
		toolsActive := toolsEnabled && b.Trust == backends.TrustFull
		var tools []llm.ToolDef
		if toolsActive {
			tools = e.exec.Defs()
		}
		if serr := sink.Event("backend", map[string]any{
			"backend": b.Name, "model": model, "trust": string(b.Trust),
			"tools_active": toolsActive, "fallback": i > 0,
		}); serr != nil {
			return streamOutcome{err: serr}
		}

		var partial strings.Builder
		started := false
		opts := e.chatOptions(b, e.tokenClamp(b, budgetLeft))

		// Acquire immediately before the wire call; the resolved LLMTimeout
		// doubles as the admission-anchored deadline hint (ChatStream applies
		// the same value as its wire deadline — rule V1). The acquire can BLOCK
		// in the dispatcher queue behind a foreign stream lease (up to 900 s at
		// the 1-slot GPU target) while a reverse-proxy idle timeout is ~60 s —
		// so we emit a periodic "queued" keepalive event during the wait (C6).
		lease, runCtx, admErr := e.acquireQueued(ctx, adm, &b, iter, sink)
		if admErr != nil {
			if ctx.Err() != nil {
				// Client disconnect while queued: the wait slot is vacated,
				// the turn ends as a regular cancel (partial persist + done).
				return streamOutcome{backend: b, model: model, err: admErr, canceled: true}
			}
			return streamOutcome{backend: b, model: model, err: admErr}
		}

		// Lease wait of THIS attempt (admitted − enqueued, the single
		// queue_wait_ms source — design/05 §3.2, MW11). st below starts
		// AFTER admission, so elapsed is wait-free by construction (§4.4a)
		// and the wait travels as its own column.
		waitMs := lease.WaitDur().Milliseconds()

		st := e.now()
		res, err := func() (*llm.StreamResult, error) {
			// defer is the only allowed release form (B1: panic-safe) — it
			// fires after ChatStream drained the stream, so the lease spans
			// first byte to stream end.
			defer lease.Release()
			res, err := llm.ChatStream(runCtx, b.Host, b.APIKey, model, msgs, tools, opts, b.ExtraBody, e.cfg.LLMTimeout,
				func(ev llm.StreamEvent) error {
					started = true
					if ev.ContentDelta != "" {
						partial.WriteString(ev.ContentDelta)
						return sink.Event("delta", map[string]any{"text": ev.ContentDelta})
					}
					if ev.ToolCallName != "" {
						return sink.Event("tool_call_start", map[string]any{"iteration": iter, "name": ev.ToolCallName})
					}
					return nil
				})
			if res != nil {
				// MW22 meter feed: the stream-end usage, booked into the
				// fairness window at Release (C1). No result ⇒ uncharged.
				lease.ReportUsage(dispatch.Usage{PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens})
			}
			return res, err
		}()
		elapsed := e.now().Sub(st)
		dur := elapsed.Milliseconds()
		e.recordLLM(sessID, apiKeyID, required, b, model, i+1, iter, elapsed, waitMs, adm.Class, res, err)
		if err == nil {
			return streamOutcome{result: res, backend: b, model: model, served: true, partial: partial.String(), durationMs: dur}
		}
		if ctx.Err() != nil {
			return streamOutcome{backend: b, model: model, partial: partial.String(), durationMs: dur, err: err, canceled: true}
		}
		lastErr = err
		slog.Warn("chat: stream attempt failed", "backend", b.Name, "iteration", iter,
			"class", backends.Classify(err, b.ProviderClass).String(), "error", err)
		if backends.Classify(err, b.ProviderClass).Next() && !started {
			continue // failover, no bytes emitted yet
		}
		return streamOutcome{backend: b, model: model, partial: partial.String(), durationMs: dur, err: err}
	}
	// Chain exhausted (every backend failed a Next()-class error before its
	// first byte): keep the LAST attempted backend so the error event still
	// carries its name (§3.5 — code + backend name, never the raw URL).
	return streamOutcome{backend: lastBackend, err: lastErr}
}

// finishUnserved handles an iteration that produced no usable result: a client
// cancel persists the partial answer with a canceled marker (over a cancel-immune
// context) and ends with a canceled done; any other error is laundered into an
// error event (class + backend name only — never the raw URL-bearing error).
func (e *Engine) finishUnserved(ctx context.Context, sess *store.ChatSession, so streamOutcome, turnMax backends.Sensitivity, start time.Time, iter int, sink Sink) error {
	if llm.IsAdmissionError(so.err) && !so.canceled {
		// Dispatch rejection (§4.3): not an attempt — launderError's Classify
		// would mislabel the sentinel. Generic saturation event, retryable:true
		// (distinguishes it from the terminal retryable:false no_eligible_backend
		// — design/03 §4.5.2 webchat row). MW8 (D3-W4): the backend NAME is
		// DROPPED here — for a saturation event it is a C2 topology signal
		// (which target is busy = foreign-load oracle), unlike a regular error
		// event where §3.5 permits the name. Body carries neither target nor
		// depth (B6/§3.3).
		return sink.Event("error", map[string]any{"code": "saturated", "retryable": true})
	}
	if so.canceled {
		rc, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		arow, _, perr := store.AppendMessage(rc, e.pool, sess.ID, store.NewMessage{
			Role: "assistant", Content: so.partial, Sensitivity: turnMax,
			Backend: so.backend.Name, Model: so.model,
			Metadata: map[string]any{"canceled": true, "iteration": iter},
		})
		if perr != nil {
			slog.Warn("chat: persist canceled partial", "session", sess.ID, "error", perr)
		}
		seq := 0
		if arow != nil {
			seq = arow.Seq
		}
		return sink.Event("done", map[string]any{
			"finish_reason": "canceled", "assistant_seq": seq,
			"iterations": iter, "total_ms": e.now().Sub(start).Milliseconds(),
		})
	}
	return sink.Event("error", launderError(so.err, so.backend))
}

// finalCall is the closing answer after the tool budget/iteration cap: a single
// model call with NO tools array, prompted to answer from the gathered material.
func (e *Engine) finalCall(ctx context.Context, sess *store.ChatSession, msgs []llm.ChatMsg, reqSens, sessHWM, turnMax backends.Sensitivity, start time.Time, apiKeyID, reason string, adm llm.Admission, sink Sink) error {
	msgs = append(msgs, llm.ChatMsg{Role: "user", Content: "Tool budget exhausted — answer now directly from the material gathered so far."})
	required := backends.MaxSensitivity(reqSens, sessHWM)
	chain, cerr := e.provider.ChatChain(ctx, required, sess.Scope)
	if cerr != nil || len(chain) == 0 {
		return emitNoEligible(sink)
	}
	so := e.runStream(ctx, chain, msgs, false, e.cfg.MaxTokens, e.cfg.MaxIterations+1, sess.ID, apiKeyID, required, adm, sink)
	if !so.served {
		return e.finishUnserved(ctx, sess, so, turnMax, start, e.cfg.MaxIterations, sink)
	}
	arow, _, perr := store.AppendMessage(ctx, e.pool, sess.ID, store.NewMessage{
		Role: "assistant", Content: so.result.Content, Sensitivity: turnMax,
		Backend: so.backend.Name, Model: so.model,
		PromptTokens: intPtr(so.result.PromptTokens), CompletionTokens: intPtr(so.result.CompletionTokens),
		DurationMs: intPtr(int(so.durationMs)),
	})
	if perr != nil {
		return fmt.Errorf("chat: persist final assistant: %w", perr)
	}
	return sink.Event("done", map[string]any{
		"finish_reason": reason, "assistant_seq": arow.Seq,
		"iterations": e.cfg.MaxIterations, "total_ms": e.now().Sub(start).Milliseconds(),
	})
}

func emitNoEligible(sink Sink) error {
	return sink.Event("error", map[string]any{"code": "no_eligible_backend", "retryable": false})
}

// launderError builds the error event: class code + backend NAME only. The raw
// error (it carries the backend URL) never reaches the client (§3.5).
func launderError(err error, b backends.Backend) map[string]any {
	class := backends.Classify(err, b.ProviderClass)
	return map[string]any{"code": class.String(), "backend": b.Name, "retryable": class.Next()}
}

// recordEntry is the llmlog write seam (pattern: the dream chatJSON seam) —
// the MW11 stream telemetry probe captures the entry to assert the wait-free
// duration/queue_wait split without a DB. Production value: llmlog.Record.
var recordEntry = llmlog.Record

// recordLLM logs one physical model call to context_llm_log, METADATA-ONLY:
// RequestSystem/RequestUser/ResponseContent stay EMPTY by construction (§3.6/R9).
// Every chat call carries the WHOLE msgs history, and context_llm_log is
// un-scoped + has no retention + lives outside the session CASCADE — full bodies
// would lay whole conversations there N-fold, breaking the DELETE promise (§3.3).
// One row per physical attempt (failover provenance: Host = the backend that
// actually answered). Fire-and-forget; a nil pool (unit tests) is a no-op.
// MW11 (design/05 §4.4b stream row): waitMs is the attempt's lease wait —
// persisted via pointer, 0 is a real measurement (pass-through admission,
// B-R4) — and class is the caller-bound admission class (constant
// interactive on this engine). No dispatch_abort here by the class
// invariant: the dispatcher never cancels interactive leases (I-D1).
func (e *Engine) recordLLM(sessID, apiKeyID string, required backends.Sensitivity, b backends.Backend, model string, attempt, iter int, elapsed time.Duration, waitMs int64, class dispatch.Class, res *llm.StreamResult, err error) {
	entry := llmlog.Entry{
		Pipeline:            "web-chat",
		Model:               model,
		Host:                b.Host,
		Duration:            elapsed,
		Err:                 err,
		BackendName:         b.Name,
		BackendTrust:        string(b.Trust),
		BackendLocality:     b.Locality,
		RequiredSensitivity: string(required),
		Attempt:             attempt,
		APIKeyID:            apiKeyID,
		QueueWaitMs:         &waitMs,
		DispatchClass:       class.String(),
		Metadata:            map[string]any{"session_id": sessID, "iteration": iter},
	}
	if res != nil {
		entry.PromptTokens = res.PromptTokens
		entry.CompletionTokens = res.CompletionTokens
		entry.Metadata["tool_calls"] = len(res.ToolCalls)
	}
	recordEntry(e.pool, entry)
}

func (e *Engine) tokenClamp(b backends.Backend, budgetLeft int) int {
	v := e.cfg.MaxTokens
	if bm := backendMaxTokens(b); bm > 0 && bm < v {
		v = bm
	}
	if budgetLeft > 0 && budgetLeft < v {
		v = budgetLeft
	}
	if v < 1 {
		v = 1
	}
	return v
}

func (e *Engine) chatOptions(b backends.Backend, numPredict int) llm.Options {
	opts := llm.Options{Temperature: 0.7, NumPredict: numPredict}
	if spec := b.ModelFor("chat"); spec.Params != nil {
		if t, ok := spec.Params["temperature"].(float64); ok {
			opts.Temperature = t
		}
		if tp, ok := spec.Params["top_p"].(float64); ok {
			opts.TopP = tp
		}
	}
	return opts
}

// buildHistory converts stored messages to wire form within the char budget:
// older tool contents condense to 400 chars, then oldest messages drop, and any
// orphaned leading tool/assistant message is trimmed so the first history turn
// starts on a user message (the Qwen template tolerates the tail, not the head).
func (e *Engine) buildHistory(msgs []store.ChatMessage) []llm.ChatMsg {
	wire := make([]llm.ChatMsg, len(msgs))
	for i, m := range msgs {
		wire[i] = toWire(m)
	}
	budget := e.cfg.HistoryBudgetChars
	if budget <= 0 || totalChars(wire) <= budget {
		return wire
	}
	for i := 0; i < len(wire)-1; i++ {
		if wire[i].Role == "tool" {
			if r := []rune(wire[i].Content); len(r) > 400 {
				wire[i].Content = string(r[:400]) + "…"
			}
		}
	}
	for len(wire) > 1 && totalChars(wire) > budget {
		wire = wire[1:]
	}
	for len(wire) > 1 && wire[0].Role != "user" {
		wire = wire[1:]
	}
	return wire
}

func (e *Engine) systemPrompt() string {
	loc, err := time.LoadLocation(e.cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	today := e.now().In(loc).Format("2006-01-02")
	storeLine := ""
	access := "read-only tool access"
	if e.exec.HasStage() {
		access = "tool access"
		storeLine = "\n- ctx_store: save a NEW knowledge block. The write is STAGED, never immediate: the user must approve a confirmation card in the UI. After staging, ask the user to confirm or dismiss the card; never call ctx_store twice for the same content." +
			"\n- ctx_update: change an EXISTING block (partial: only passed fields change). Also STAGED behind a confirmation card; if the block changes before the user confirms, the confirmation is rejected — re-stage then. Never call ctx_update twice for the same change."
	}
	return fmt.Sprintf(`You are the ctx assistant with %s to the user's knowledge store.
Today is %s (%s). Answer in the user's language.
- ctx_query: hybrid retrieval for content questions — returns ranked blocks with snippets.
- ctx_search: browse/list by keywords, category or tags (titles + previews).
- ctx_get: read one full block by id (or unique id prefix).
- ctx_recent: list recently saved/updated blocks.%s
Cite used blocks inline as [title](ctx:<id>). If retrieval finds nothing relevant, say so instead of guessing. Never invent block ids.`, access, today, e.cfg.Timezone, storeLine)
}

func toWire(m store.ChatMessage) llm.ChatMsg {
	w := llm.ChatMsg{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
	if len(m.ToolCalls) > 0 {
		var tcs []llm.ToolCall
		if err := json.Unmarshal(m.ToolCalls, &tcs); err == nil {
			w.ToolCalls = tcs
		}
	}
	return w
}

func marshalToolCalls(tcs []llm.ToolCall) []byte {
	if len(tcs) == 0 {
		return nil
	}
	b, err := json.Marshal(tcs)
	if err != nil {
		return nil
	}
	return b
}

func totalChars(msgs []llm.ChatMsg) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n
}

func backendMaxTokens(b backends.Backend) int {
	if b.Limits == nil {
		return 0
	}
	switch v := b.Limits["chat_max_tokens"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func normalizeSens(s backends.Sensitivity) backends.Sensitivity {
	if !backends.ValidSensitivity(s) {
		return backends.SensCredentials
	}
	return s
}

func doneReason(finish string) string {
	if finish == "" {
		return "stop"
	}
	return finish
}

func intPtr(v int) *int { return &v }

// PoolProvider is the production BackendProvider over the F3 pool.
type PoolProvider struct {
	pool   *backends.Pool
	gaming func() backends.GamingState
}

// NewPoolProvider wraps the pool; gaming may be nil (no gaming exclusion).
func NewPoolProvider(pool *backends.Pool, gaming func() backends.GamingState) *PoolProvider {
	return &PoolProvider{pool: pool, gaming: gaming}
}

// ChatChain returns the trust-ordered chat chain for the required sensitivity,
// bounded to the caller tenant's visible backends (04-W2/T34).
func (p *PoolProvider) ChatChain(_ context.Context, required backends.Sensitivity, tenant string) ([]backends.Backend, error) {
	var g backends.GamingState
	if p.gaming != nil {
		g = p.gaming()
	}
	return p.pool.Chain("chat", required, g, tenant)
}

package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ChainAttempt is one tried backend for llmlog's metadata.chain — the full
// provenance of a chained call (the row's backend_name alone only names the
// winner).
type ChainAttempt struct {
	Backend string `json:"backend"`
	Class   string `json:"err_class"`
	Ms      int64  `json:"ms"`
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
	defTimeout time.Duration, report ReportFunc,
) (*ChatResponse, *backends.Backend, []ChainAttempt, error) {
	return ChatChainVia(ctx, func(ctx context.Context, b backends.Backend, sys, usr string, opts Options, timeout time.Duration) (*ChatResponse, error) {
		return chatWithFormat(ctx, string(b.Protocol), b.Host, b.APIKey,
			b.Model, b.Think.Ptr(), sys, usr, opts, format, timeout)
	}, chain, role, systemPrompt, userPrompt, baseOpts, defTimeout, report)
}

// ChatChainVia is ChatChain with an injected wire call (G28: the dream
// package walks its chains through the chatJSON test seam). The attempt loop
// — model resolution, params merge, Classify-driven continuation, health
// reporting — lives ONLY here so the failover error doctrine has one home.
func ChatChainVia(ctx context.Context, call ChatFunc, chain []backends.Backend, role string,
	systemPrompt, userPrompt string, baseOpts Options,
	defTimeout time.Duration, report ReportFunc,
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

		start := time.Now()
		resp, err := call(ctx, resolved, systemPrompt, userPrompt, opts, timeout)
		elapsed := time.Since(start)

		if err == nil {
			attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: "ok", Ms: elapsed.Milliseconds()})
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

		class := backends.Classify(err, b.ProviderClass)
		attempts = append(attempts, ChainAttempt{Backend: b.Name, Class: class.String(), Ms: elapsed.Milliseconds()})
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

// applyModelParams merges ModelSpec.Params field-wise over the code-default
// options and resolves the think toggle (params.think wins over the row's
// legacy Think field). NumCtx always comes from the row.
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
		}
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
func LogEmbedWire(db *pgxpool.Pool, pipeline, role string, required backends.Sensitivity,
	served *backends.Backend, attempts int, duration time.Duration, blockIDs []string, err error,
) {
	entry := llmlog.Entry{
		Pipeline:            pipeline,
		Duration:            duration,
		Err:                 err,
		BlockIDs:            blockIDs,
		RequiredSensitivity: string(required),
		Attempt:             attempts,
	}
	if served != nil {
		entry.Model = served.ModelFor(role).Model
		entry.Host = served.Host
		entry.BackendName = served.Name
		entry.BackendTrust = string(served.Trust)
		entry.BackendLocality = served.Locality
	}
	llmlog.Record(db, entry)
}

// retryAfterOf extracts the parsed Retry-After of a 429 (zero otherwise).
func retryAfterOf(err error) time.Duration {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
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
	Pool       *backends.Pool
	Role       string
	Required   backends.Sensitivity
	Pipeline   string // llmlog pipeline name, e.g. "query-translate"
	System     string
	User       string
	Opts       Options
	Format     string // "" | "json"
	DefTimeout time.Duration
	BlockIDs   []string
}

// Do resolves the chain (the trust gate: an excluded backend does not exist
// for this call), walks it via ChatChain and records the slim llmlog row.
// An empty chain returns *backends.ErrNoEligibleBackend — the call site
// decides its role's fail-open/fail-hard semantics (design 03 §2.4).
func (c ChainCall) Do(ctx context.Context, db *pgxpool.Pool) (*ChatResponse, error) {
	chain, err := c.Pool.Chain(c.Role, c.Required, backends.GamingState{})
	if err != nil {
		return nil, err
	}

	start := time.Now()
	resp, served, attempts, err := ChatChain(ctx, chain, c.Role,
		c.System, c.User, c.Opts, c.Format, c.DefTimeout, PoolReporter(c.Pool))

	entry := llmlog.Entry{
		Pipeline:            c.Pipeline,
		Duration:            time.Since(start),
		Err:                 err,
		BlockIDs:            c.BlockIDs,
		RequiredSensitivity: string(c.Required),
		Attempt:             len(attempts),
		Metadata:            map[string]any{"chain": attempts},
	}
	if served != nil {
		entry.Model = served.ModelFor(c.Role).Model
		entry.Host = served.Host
		entry.BackendName = served.Name
		entry.BackendTrust = string(served.Trust)
		entry.BackendLocality = served.Locality
	}
	if resp != nil {
		entry.CompletionTokens = resp.EvalCount
		entry.PromptTokens = resp.PromptTokens
	}
	llmlog.Record(db, entry)

	return resp, err
}

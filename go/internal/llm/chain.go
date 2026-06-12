package llm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
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

// ChatChain tries the chain in order: per attempt its own role timeout
// (httpx.DoRetryOnce stays INSIDE the attempt — transport blip on the same
// connection), Classify decides continuation, the parent context aborts
// immediately. Returns the ANSWERING backend for llmlog provenance plus all
// attempts for metadata.chain.
//
// Per backend, the request derives from the row: ModelFor(role) names the
// model, ModelSpec.Params merge field-wise over the role's code-default
// options (think included), NumCtx comes from the row.
func ChatChain(ctx context.Context, chain []backends.Backend, role string,
	systemPrompt, userPrompt string, baseOpts Options, format string,
	report ReportFunc,
) (*ChatResponse, *backends.Backend, []ChainAttempt, error) {
	if len(chain) == 0 {
		return nil, nil, nil, fmt.Errorf("llm: chain for role %q is empty", role)
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

		opts, think := applyModelParams(baseOpts, spec.Params, b)
		timeout := b.TimeoutFor(role, ChatTimeout)

		start := time.Now()
		resp, err := chatWithFormat(ctx, string(b.Protocol), b.Host, b.APIKey,
			spec.Model, think, systemPrompt, userPrompt, opts, format, timeout)
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

// retryAfterOf extracts the parsed Retry-After of a 429 (zero otherwise).
func retryAfterOf(err error) time.Duration {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

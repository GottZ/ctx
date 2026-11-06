package oai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
)

// The stream half of the chat route (wave W02-08; design/02 §4.6, §4.7,
// §4.11.3): the attempt relays the upstream stream through the relay writer
// (sse.go), the outcome ends it with [DONE] or an error frame, and the row
// books the backend's usage or — a stream without a usage chunk — the
// estimate (design/06 §4.2 B-06-8).

// errChunkRewrite wraps a chunk the relay cannot render (no JSON, choices
// of the wrong kind): the attempt fails like a malformed upstream answer.
var errChunkRewrite = errors.New("oai: upstream stream chunk could not be rewritten")

// streamAttempt is one wire attempt of a stream under its lease: the body
// for this row, the attempt's request id, the one stream wire site, every
// chunk rewritten and relayed. It reports committed once a data frame
// reached the client — the walker fails over only before that (§4.11.1).
func (c *chatCall) streamAttempt(st *walkState, spec llm.UpstreamSpec, lane string, runCtx context.Context, b *backends.Backend,
	lease *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
	st.n++
	st.lease = lease
	if st.admitted.IsZero() {
		st.admitted = time.Now()
		// Admitted (the acquire returned): phase B. The commit comes with
		// the first data frame or at entry + grace_eff, never earlier than
		// now (K7, design/03 §4.9 step 6).
		c.sse.arm(c.entry.Add(c.grace))
	}
	body, err := llm.BuildUpstreamBody(spec, b)
	if err != nil {
		st.last = failedAttempt{b: b, err: err}
		st.failed = append(st.failed, st.last)
		return dispatch.Usage{}, false, err
	}
	runCtx = fedhop.WithRequestID(runCtx, attemptRequestID(c.id, st.n))
	// As non-stream: a run that ended while the lease was acquired does not
	// dial (Fremd-Review 4 R4-1 of W02-07).
	c.syncLifecycle()
	if runCtx.Err() != nil {
		err := fmt.Errorf("oai: the run ended before the wire call: %w", context.Cause(runCtx))
		st.last = failedAttempt{b: b, err: err}
		st.failed = append(st.failed, st.last)
		return dispatch.Usage{}, false, err
	}
	// As non-stream: st.wired for the census, c.wired — the meter's wire
	// contact — from the attempt's trace (wireContext; ChatRawStream goes
	// through httpx.DoRetryOnce as ChatRaw does). A stream marks it at the
	// latest with the backend's first response byte, before any frame.
	st.wired++
	st.stream, st.relayed, st.runes, st.usage, st.promptN, st.streamErr = nil, 0, 0, nil, 0, nil
	id, created, reasoning, include := "chatcmpl-"+c.id, st.admitted.Unix(), reasoningOff(c.alias), c.req.includeUsage
	actx, done := c.wireContext(runCtx, timeout)
	res, err := llm.ChatRawStream(actx, *b, body, timeout, c.s.MaxStreamFrameBytes, func(payload []byte) error {
		chunk, rerr := rewriteChunk(payload, id, created, c.alias.Name, lane, reasoning, include)
		if rerr != nil {
			st.streamErr = fmt.Errorf("%w: %w", errChunkRewrite, rerr)
			return st.streamErr
		}
		st.runes += chunk.runes
		if chunk.usage != nil {
			st.usage = chunk.usage
		}
		if chunk.promptN > 0 {
			st.promptN = chunk.promptN
		}
		if chunk.skip {
			return nil
		}
		if werr := c.sse.frame(chunk.out); werr != nil {
			return werr
		}
		st.relayed++
		return nil
	})
	done(err)
	st.stream = res
	var usage dispatch.Usage
	if res != nil {
		usage = dispatch.Usage{PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens}
	}
	committed := st.relayed > 0
	if err != nil {
		st.last = failedAttempt{b: b, err: err}
		st.failed = append(st.failed, st.last)
		return usage, committed, err
	}
	return usage, committed, nil
}

// streamTokens is the usage of a stream the backend began to answer (its
// 200 arrived): the usage chunk's counts (backend), else the estimate
// (design/06 §4.2 B-06-8: prompt = llama.cpp's timings.prompt_n or
// TokenEstimate over the request's runes, completion = TokenEstimate over
// the runes the relay read, at most the max_tokens that went upstream — no
// backend generates more; Delta-Review 4 F4-1). The estimate is meant to
// err high (1.8 characters per token) but is not a bound: text with fewer
// characters per token (CJK, emoji) and tokens an engine generated after the
// relay stopped reading are not in it (N-2). A last attempt the backend
// refused before its stream (a 400, a 502, a dial error) books nothing, as
// non-stream: the estimate is for a stream cut short, not for a prompt no
// engine processed (Fremd-Review 1 F-2). The meter books the same (book):
// the usage chunk by Settle, the estimate by Settle while it is below what
// the reservation holds, else Keep — a stream cut short generated tokens, a
// full refund would hand them out free (B-06-8), and its estimate never
// charges more than the reservation — and a stream without a 200 after wire
// contact keeps its reservation. The usage chunk's counters are summed
// saturated (usageTotal): an int sum of two reported counters can overflow
// and turn the backend's usage into an estimate.
func (c *chatCall) streamTokens(st *walkState) {
	if st.wired == 0 || st.stream == nil {
		return
	}
	if st.usage != nil && usageTotal(st.usage[0], st.usage[1]) > 0 {
		st.tokens = dispatch.Usage{PromptTokens: st.usage[0], CompletionTokens: st.usage[1]}
		st.usageKind = llmlog.UsageBackend
		return
	}
	prompt := st.promptN
	if prompt <= 0 {
		prompt = promptguard.TokenEstimate(utf8.RuneCount(c.req.messages))
	}
	st.tokens = dispatch.Usage{PromptTokens: prompt, CompletionTokens: min(promptguard.TokenEstimate(st.runes), c.maxTokens)}
	st.usageKind = llmlog.UsageEstimated
}

// bucketCapacityHeader is the token bucket's capacity as the request's lane
// sees it (design/06 §4.4), one of the admission's x-ratelimit-* headers.
const bucketCapacityHeader = "x-ratelimit-limit-tokens"

// reservedTokens lives here, not in chat.go: the promptguard call-site
// census (internal/promptguard) reads any promptguard call in the file of a
// Pipeline: site as a guard, and chat.go holds the route's unguarded one.
//
// reservedTokens is what an admitted chat reservation holds — the tokens
// Keep books (Delta-Review 4 F4-1): the Estimate the limiter charged, by the
// rule Estimate documents (design/06 §4.2 "Reservierung je op": TokenEstimate
// over the prompt's runes plus the effective max_tokens when the client named
// one, else gateway.tpm_reserve_default capped by it), at most the bucket's
// capacity the admission reports in x-ratelimit-limit-tokens (B-06-28: a
// larger reservation is charged at the capacity). The reserve comes from the
// request's settings snapshot; the production limiter reads the same key
// from the same snapshot source at its Admit (cmd/ctxd meterLimiter), so a
// hot change in between moves the result by that change. A limiter that
// reports no capacity leaves the estimate uncapped.
func reservedTokens(promptRunes, maxTokens int, clientMax bool, reserve int64, h http.Header) int {
	share := max(maxTokens, 0)
	if !clientMax && (share == 0 || reserve < int64(share)) {
		share = int(min(max(reserve, 0), math.MaxInt))
	}
	n := usageTotal(promptguard.TokenEstimate(promptRunes), share)
	if capacity, err := strconv.Atoi(h.Get(bucketCapacityHeader)); err == nil && capacity > 0 {
		n = min(n, capacity)
	}
	return n
}

// answerStream ends a stream: the timer frozen first (no commit between
// the check and the write), then the outcome — [DONE], an error frame after
// the commit, a status before it — and the one row. A stream whose client
// stopped reading is aborted (the connection closed, never a clean end).
func (c *chatCall) answerStream(plan Plan, served *backends.Backend, attempts []llm.ChainAttempt, err error, st *walkState) {
	committed := c.sse.freeze()
	// A panic the timer caught (a writer of the chain or the header source
	// in its commit or keepalive) runs on here, on the handler's goroutine:
	// close() ends the reservation (Keep after wire contact), oai.Recover
	// answers it (Delta-Review 4 F4-2).
	c.sse.rethrow()
	c.streamTokens(st)
	c.leaseStats(st)
	c.rec.PromptTokens, c.rec.CompletionTokens, c.rec.Usage = st.tokens.PromptTokens, st.tokens.CompletionTokens, st.usageKind
	out := c.streamOutcome(plan, attempts, err, st)
	c.book(plan, served, attempts, err, st, out)
	if committed && c.sse.failed() {
		// The client stopped reading (D4) or a write broke: no frame can
		// reach it, and ending the response normally would hand it a body
		// that ends like a complete one. A deliberate abort goes through
		// abortCommitted (W02-02 handover), never through the house
		// Recovery.
		abortCommitted(c.w)
	}
}

// streamOutcome answers the walk of a stream: the watcher's, the shutdown's
// and the re-auth's verdict and a gone client first, then success ([DONE]),
// the admission writer's domain (before the commit its status; after it
// the same envelope as an error frame — Next after a grace commit, K6:
// server_busy) and the route's attempt mapping (§4.10.3 codes after the
// commit).
func (c *chatCall) streamOutcome(plan Plan, attempts []llm.ChainAttempt, err error, st *walkState) chatOutcome {
	if ended, code := c.verdict(); ended {
		return chatOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String(), Code: code}}
	}
	if err == nil {
		if derr := c.sse.done(); derr != nil {
			slog.Warn("gateway: failed to end the stream", "error", derr, "request_id", c.w.Header().Get("X-Request-ID"))
			return chatOutcome{finish: llmlog.FinishAborted, redacted: &llmlog.RedactedErr{Class: backends.ClassCanceled.String()}}
		}
		finish := ""
		if st.stream != nil {
			finish = finishOf(st.stream.FinishReason)
		}
		return chatOutcome{finish: finish}
	}
	return c.outcomeError(plan, attempts, err, st)
}

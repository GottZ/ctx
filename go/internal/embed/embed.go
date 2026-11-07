// Package embed handles text embedding generation.
// Supports two wire protocols:
//   - "ollama": /api/embed (supports num_ctx via options)
//   - "openai": /v1/embeddings (standard, works with any provider)
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
)

// httpClients is this family's plain/guarded pool pair; every wire call picks
// its client with httpClients.For(b.EgressGuarded()) — tenant rows dial
// through ssrfguard (GHSA 2026-10-31 §3), operator rows do not.
var httpClients = httpx.NewClients()

const (
	TargetDims   = 1024
	minNorm      = 0.99
	maxNorm      = 1.01
	minVariance  = 0.0001
)

// Prefix determines the asymmetric instruction prefix for embeddings.
type Prefix int

const (
	PrefixQuery    Prefix = iota
	PrefixDocument
)

func (p Prefix) text() string {
	switch p {
	case PrefixQuery:
		return "Instruct: Represent the query for retrieving relevant documents\nContent: "
	case PrefixDocument:
		return "Instruct: Represent the document for retrieval\nContent: "
	default:
		return ""
	}
}

// --- Ollama wire format ---.

type ollamaEmbedRequest struct {
	Model string `json:"model"`
	// Input is the bare string of the internal path's one input (the body
	// stays byte-identical to the single-string wire, W02-09 G2) or the list
	// of a gateway sub-block (wireInput.value).
	Input   any         `json:"input"`
	Options *ollamaOpts `json:"options,omitempty"`
}

type ollamaOpts struct {
	NumCtx int `json:"num_ctx,omitempty"`
}

// ollamaEmbedResponse decodes the vectors straight into the element type the
// caller holds: float64 for the internal 1024-d path (its normalization stays
// bit-identical), float32 for the gateway (design/02 §4.2.1: the vectors of
// an embeddings answer are held as float32 from the wire on).
type ollamaEmbedResponse[T floatElem] struct {
	Embeddings [][]T `json:"embeddings"`
	// PromptEvalCount is ollama's token count of the embedded input — the
	// MW22/C1 charge dimension of an embed wire call (prompt side only).
	// Absent field ⇒ 0 ⇒ the call site charges nothing (uncharged, never an
	// estimate).
	PromptEvalCount int `json:"prompt_eval_count"`
}

// --- OpenAI wire format ---.

type openAIEmbedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // see ollamaEmbedRequest.Input
}

type openAIEmbedResponse[T floatElem] struct {
	Data []struct {
		// Index is the position of the input the vector belongs to: the
		// answer is re-aligned by it (alignVectors), never read in order.
		Index     int `json:"index"`
		Embedding []T `json:"embedding"`
	} `json:"data"`
	// Usage carries the standard OpenAI-wire token accounting (same charge
	// role as ollama's prompt_eval_count above). Voyage AI reports
	// total_tokens instead of prompt_tokens on the embed endpoint; the
	// fallback in embedOpenAI handles both wire formats.
	Usage openAIUsage `json:"usage"`
}

// floatElem is the element type a wire answer decodes into.
type floatElem interface{ ~float32 | ~float64 }

// wireInput is the input of one embed wire call: the texts and whether the
// one text of the internal path goes out as a bare string. Only Embed sets
// bare — its body is byte-identical to the single-string wire every
// deployed backend has seen (design/02 §4.8 "Wire"); a gateway sub-block is
// always a list, also with one input.
type wireInput struct {
	texts []string
	bare  bool
}

// value is the JSON value of the body's "input" key.
func (in wireInput) value() any {
	if in.bare {
		return in.texts[0]
	}
	return in.texts
}

// marshal encodes the request body. The internal path keeps json.Marshal —
// its body stays byte-identical to the single-string wire (W02-09 G2,
// TestInternalEmbedBitIdenticalToBase). A gateway list goes out without
// HTML escaping: json.Marshal turns each <, > and & into six bytes, and a
// batch of HTML would grow six-fold on its way upstream, past every reserve
// (W02-09 hardening H-2); the backend reads JSON, not HTML. The gateway
// body is one allocation of its exact length (FOLGE-E hardening 1, S-1,
// wire_body.go): wireBodyLen sizes it from the model, the texts and the
// options before a byte is written, appendWireBody writes the encoder's
// bytes into that capacity — the json.Encoder it replaces allocated up to
// 7.105 × the body (its own buffer's growth and a copy), which the route
// booked as eight times the body. A length the writer does not meet is an
// error, never a second allocation.
func (in wireInput) marshal(v any) ([]byte, error) {
	if in.bare {
		return json.Marshal(v)
	}
	var model string
	var opts *ollamaOpts
	switch r := v.(type) {
	case ollamaEmbedRequest:
		model, opts = r.Model, r.Options
	case openAIEmbedRequest:
		model = r.Model
	default:
		return nil, fmt.Errorf("embed: no gateway body for %T", v)
	}
	n := wireBodyLen(model, in.texts, opts)
	body := appendWireBody(make([]byte, 0, n), model, in.texts, opts)
	if len(body) != n || cap(body) != n {
		return nil, fmt.Errorf("embed: gateway body of %d bytes, sized %d", len(body), n)
	}
	return body, nil
}

// Embed generates an embedding via the backend tuple, truncates to 1024d,
// and L2-normalizes. Host and Protocol arrive as one backends.Backend value
// (F1-W5) — the wire path (/api/embed vs /v1/embeddings) can never diverge
// from the host it was configured with. Any protocol other than "openai"
// takes the ollama path (pre-W5 dispatch semantics, incl. empty).
// promptTokens is the backend-reported input token count (MW5: embeds charge
// prompt tokens into the dispatch usage meter); 0 when the backend reports
// none — the call site then charges nothing instead of estimating (C1).
func Embed(ctx context.Context, b backends.Backend, text string, prefix Prefix) (vec []float32, promptTokens int, err error) {
	in := wireInput{texts: []string{prefix.text() + text}, bare: true}

	raw, promptTokens, err := embedWire[float64](ctx, b, b.Model, in, httpx.MaxResponseBytes, rawAnswer{})
	if err != nil {
		return nil, 0, err
	}

	if len(raw[0]) < TargetDims {
		return nil, 0, fmt.Errorf("embed: embedding too short: got %d, need >= %d", len(raw[0]), TargetDims)
	}

	vec = l2Normalize(raw[0][:TargetDims])

	if err := qualityGate(vec); err != nil {
		return nil, 0, err
	}

	return vec, promptTokens, nil
}

// EmbedRaw embeds the inputs of one gateway sub-block (design/02 §4.8,
// §4.13; wave W02-09): the texts as the client sent them — no instruct
// prefix (an embedding client prepends its own, a second one would move the
// vector space), no 1024-d cut, no normalization — one vector per input, in
// input order (re-aligned by data[].index), at the row's native length.
// model is the row's model for the role. dims > 0 is the expected length
// (the alias's embedding.native_dims); 0 lets the first vector set it, and
// every other vector must match. The quality check (rawQuality) is finite
// components, the expected length and a norm > 0. maxBytes caps the
// answer the wire reads (the caller sizes it from the block and reserves the
// decode transient for it, W02-09 hardening H-3); ≤ 0 or above
// httpx.MaxResponseBytes is that transport cap. An answer past the cap
// fails with httpx.ErrResponseTooLarge (a malformed answer for the failover
// classifier: stop the chain). The answer is read under the strict wire
// contract (raw_answer.go, W02-09 hardening 6): an entry past the inputs is
// refused before it is decoded, every entry carries an index, no component
// is null, and every vector has the expected length before it is
// allocated. The wire sites and the client choice are Embed's:
// httpClients.For(b.EgressGuarded()).
func EmbedRaw(ctx context.Context, b backends.Backend, model string, inputs []string, dims int, maxBytes int64) (vecs [][]float32, promptTokens int, err error) {
	if len(inputs) == 0 {
		return nil, 0, errors.New("embed: no input")
	}
	if maxBytes <= 0 || maxBytes > httpx.MaxResponseBytes {
		maxBytes = httpx.MaxResponseBytes
	}
	vecs, promptTokens, err = embedWire[float32](ctx, b, model, wireInput{texts: inputs}, maxBytes, rawAnswer{strict: true, dims: dims})
	if err != nil {
		return nil, 0, err
	}
	if err := rawQuality(vecs, dims); err != nil {
		return nil, 0, err
	}
	return vecs, promptTokens, nil
}

// embedWire is the protocol choice of both embed paths: the row's wire
// (/v1/embeddings for "openai", /api/embed for every other protocol — the
// pre-W5 dispatch semantics, incl. empty) with the row's egress client;
// ra is how the answer is read.
func embedWire[T floatElem](ctx context.Context, b backends.Backend, model string, in wireInput, limit int64, ra rawAnswer) ([][]T, int, error) {
	// The hop target comes from the row alone and goes down with the client:
	// both wires stamp the request and observe the answer with it.
	hop := b.HopTarget(fedhop.WireEmbed)
	if b.Protocol == backends.ProtocolOpenAI {
		return embedOpenAI[T](ctx, httpClients.For(b.EgressGuarded()), hop, b.Host, b.APIKey, model, in, limit, ra)
	}
	return embedOllama[T](ctx, httpClients.For(b.EgressGuarded()), hop, b.Host, b.APIKey, model, in, b.NumCtx, limit, ra)
}

// rawQuality is EmbedRaw's quality check: every component finite, every
// vector of the expected length (dims, or the first vector's when dims is
// 0) and of a norm above zero. The internal gate (qualityGate: norm ≈ 1,
// variance floor) is a property of the normalized 1024-d space and does not
// apply to raw vectors.
func rawQuality(vecs [][]float32, dims int) error {
	if dims <= 0 && len(vecs) > 0 {
		dims = len(vecs[0])
	}
	for i, v := range vecs {
		if len(v) != dims {
			return fmt.Errorf("embed: raw quality: vector %d has %d dimensions, want %d", i, len(v), dims)
		}
		var sumSq float64
		for _, x := range v {
			f := float64(x)
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return fmt.Errorf("embed: raw quality: vector %d has a non-finite component", i)
			}
			sumSq += f * f
		}
		if !(sumSq > 0) {
			return fmt.Errorf("embed: raw quality: vector %d has norm 0", i)
		}
	}
	return nil
}

func embedOllama[T floatElem](ctx context.Context, client *http.Client, hop fedhop.Target, host, apiKey, model string, in wireInput, numCtx int, limit int64, ra rawAnswer) ([][]T, int, error) {
	reqBody := ollamaEmbedRequest{Model: model, Input: in.value()}
	if numCtx > 0 {
		reqBody.Options = &ollamaOpts{NumCtx: numCtx}
	}

	body, err := in.marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: marshal: %w", err)
	}

	// No artificial deadline: embed-call latency is data-dependent (chunk-size,
	// queue-depth on shared GPU at high parallelism). Parent ctx provides the
	// real timeout (cycle-timeout for backfill, request-timeout for query path).
	// Was: WithTimeout(ctx, 30s) — fired under parallel=16 pressure (Welle-49).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("embed: create request: %w", err)
	}
	// No row extra_headers on the embed wires; the stamp, then the core.
	fedhop.Stamp(ctx, req.Header, hop)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpx.DoRetryOnce(client, req, body)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := fedhop.Observe(ctx, hop, resp.StatusCode, resp.Header); err != nil {
		return nil, 0, fmt.Errorf("embed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("embed: %w", httpx.NewStatusError(resp, errBody))
	}

	if ra.strict {
		return decodeRawOllama[T](resp, httpx.CapBody(resp.Body, limit), limit, len(in.texts), ra.dims)
	}
	var result ollamaEmbedResponse[T]
	if err := json.NewDecoder(httpx.CapBody(resp.Body, limit)).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("embed: decode: %w", err)
	}

	// /api/embed answers in input order and carries no index.
	if err := checkVectors(result.Embeddings, len(in.texts)); err != nil {
		return nil, 0, err
	}
	return result.Embeddings, result.PromptEvalCount, nil
}

func embedOpenAI[T floatElem](ctx context.Context, client *http.Client, hop fedhop.Target, host, apiKey, model string, in wireInput, limit int64, ra rawAnswer) ([][]T, int, error) {
	reqBody := openAIEmbedRequest{Model: model, Input: in.value()}

	body, err := in.marshal(reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: marshal: %w", err)
	}

	// No artificial deadline (see Ollama-path note above).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("embed: create request: %w", err)
	}
	fedhop.Stamp(ctx, req.Header, hop)
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpx.DoRetryOnce(client, req, body)
	if err != nil {
		return nil, 0, fmt.Errorf("embed: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := fedhop.Observe(ctx, hop, resp.StatusCode, resp.Header); err != nil {
		return nil, 0, fmt.Errorf("embed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, 0, fmt.Errorf("embed: %w", httpx.NewStatusError(resp, errBody))
	}

	var vecs [][]T
	var usage openAIUsage
	if ra.strict {
		vecs, usage, err = decodeRawOpenAI[T](resp, httpx.CapBody(resp.Body, limit), limit, len(in.texts), ra.dims)
		if err != nil {
			return nil, 0, err
		}
	} else {
		var result openAIEmbedResponse[T]
		if err := json.NewDecoder(httpx.CapBody(resp.Body, limit)).Decode(&result); err != nil {
			return nil, 0, fmt.Errorf("embed: decode: %w", err)
		}
		vecs, err = alignVectors(result.Data, len(in.texts), func(i int) (int, []T) {
			return result.Data[i].Index, result.Data[i].Embedding
		})
		if err != nil {
			return nil, 0, err
		}
		usage = result.Usage
	}

	// Voyage AI reports total_tokens; OpenAI-compatible servers report
	// prompt_tokens. Prefer prompt_tokens; fall back to total_tokens.
	promptTokens := usage.PromptTokens
	if promptTokens <= 0 && usage.TotalTokens > 0 {
		promptTokens = usage.TotalTokens
		// This flips the MW22 meter semantics for total_tokens-only
		// backends from "uncharged" to a real measured charge — same
		// one-time visibility as the rerank pendant (4970856): embeds
		// run per query and per backfill batch, per-call INFO would
		// be noise.
		embedTotalTokensFallbackOnce.Do(func() {
			slog.Info("embed: backend reports usage.total_tokens only; charging it as prompt tokens (previously uncharged)")
		})
	}

	return vecs, promptTokens, nil
}

// alignVectors re-aligns the n entries of an OpenAI-wire answer by their
// index (the pattern of rerank's result resolve): exactly n entries, every
// index in [0, n) exactly once, no empty vector. An answer that misses,
// repeats or invents an index is refused — a vector in the wrong place is a
// silently wrong embedding.
func alignVectors[E any, T floatElem](data []E, n int, at func(i int) (int, []T)) ([][]T, error) {
	if len(data) == 0 {
		return nil, errors.New("embed: empty embedding returned")
	}
	if len(data) != n {
		return nil, fmt.Errorf("embed: %d embeddings returned for %d inputs", len(data), n)
	}
	out := make([][]T, n)
	for i := range data {
		idx, v := at(i)
		if idx < 0 || idx >= n || out[idx] != nil {
			return nil, fmt.Errorf("embed: embedding index %d outside [0, %d) or repeated", idx, n)
		}
		if len(v) == 0 {
			return nil, errors.New("embed: empty embedding returned")
		}
		out[idx] = v
	}
	return out, nil
}

// checkVectors is the in-order twin for the ollama wire: n non-empty vectors.
func checkVectors[T floatElem](vecs [][]T, n int) error {
	if len(vecs) == 0 {
		return errors.New("embed: empty embedding returned")
	}
	if len(vecs) != n {
		return fmt.Errorf("embed: %d embeddings returned for %d inputs", len(vecs), n)
	}
	for _, v := range vecs {
		if len(v) == 0 {
			return errors.New("embed: empty embedding returned")
		}
	}
	return nil
}

var embedTotalTokensFallbackOnce sync.Once

func l2Normalize(v []float64) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += x * x
	}
	norm := math.Sqrt(sumSq)
	result := make([]float32, len(v))
	if norm > 0 {
		for i, x := range v {
			result[i] = float32(x / norm)
		}
	}
	return result
}

func qualityGate(vec []float32) error {
	if len(vec) != TargetDims {
		return fmt.Errorf("embed: quality gate: dimension mismatch: got %d, want %d", len(vec), TargetDims)
	}
	var sumSq float64
	for _, x := range vec {
		sumSq += float64(x) * float64(x)
	}
	norm := math.Sqrt(sumSq)
	if norm < minNorm || norm > maxNorm {
		return fmt.Errorf("embed: quality gate: norm %.6f outside [%.2f, %.2f]", norm, minNorm, maxNorm)
	}
	var sum float64
	for _, x := range vec {
		sum += float64(x)
	}
	mean := sum / float64(len(vec))
	var varSum float64
	for _, x := range vec {
		d := float64(x) - mean
		varSum += d * d
	}
	variance := varSum / float64(len(vec))
	if variance < minVariance {
		return fmt.Errorf("embed: quality gate: variance %.8f below minimum %.4f", variance, minVariance)
	}
	return nil
}

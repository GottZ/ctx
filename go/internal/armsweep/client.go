package armsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/rrf"
)

// maxResponseSize caps a single API response. An arm_ranks body over a 1M-block
// corpus is thousands of rows of numbers, so the cap is generous — but a cap
// there must be, or a misrouted response streams into memory unbounded.
const maxResponseSize = 64 * 1024 * 1024

// ErrRetryable marks a failure the retry budget may spend an attempt on:
// transport faults, timeouts and 5xx (the class the embed chain fails in). A
// 4xx is NOT retryable — a rejected gate or a bad key answers the same way
// three times and the run should stop on the first one, loudly.
var ErrRetryable = errors.New("retryable")

// ErrGateRefused is the fail-fast class: the seam refused the request. Almost
// always a non-admin key, a missing synthesize:false, or an instance that does
// not carry the B-W2 seam yet (v5.5.0 does not).
var ErrGateRefused = errors.New("measurement seam refused the request")

// Client is the driver's view of one ctx instance.
type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

// NewClient builds a client with a per-request timeout wide enough for an
// exact-mode selector decision over a large corpus.
func NewClient(baseURL, key string, timeout time.Duration) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: timeout},
	}
}

// QueryRequest is the measurement request body (design 04 §4.4). Pins are
// pointers because the EMPTY STRING is a meaningful value for both — "no
// translation change" and "explicitly no temporal expansion" — so absence and
// emptiness must stay distinguishable on the wire.
type QueryRequest struct {
	Query             string  `json:"query"`
	Synthesize        bool    `json:"synthesize"`
	ArmRanks          bool    `json:"arm_ranks"`
	Limit             *int    `json:"limit,omitempty"`
	PinnedTranslation *string `json:"pinned_translation,omitempty"`
	PinnedTemporal    *string `json:"pinned_temporal,omitempty"`
	// ShadowTypes is the M-W2 measurement widening (design/05 §4.2): the named
	// types join p_types_visible for this request's two SQL statements only.
	// omitempty is load-bearing — without it every ordinary dump would send a
	// key that a pre-M-W2 instance has never seen, and the request body of a
	// normal run would stop being byte-identical to the one that was measured
	// before the wave.
	ShadowTypes []string `json:"shadow_types,omitempty"`
}

// ArmRanksBlock mirrors handler.armRanksBlock on the wire.
type ArmRanksBlock struct {
	Rows                 []rrf.ArmRow `json:"rows"`
	FusionOrder          []string     `json:"fusion_order"`
	EffectiveQuery       string       `json:"effective_query"`
	EffectiveQuerySpaced string       `json:"effective_query_spaced"`
	EffectiveTemporal    string       `json:"effective_temporal"`
	EmbedModel           string       `json:"embed_model"`
	EmbedCacheHit        bool         `json:"embed_cache_hit"`
	Selector             Selector     `json:"selector"`
}

// QueryResponse is the retrieval-only response plus the measurement block.
type QueryResponse struct {
	Success  bool           `json:"success"`
	Error    string         `json:"error"`
	Sources  []SourceRef    `json:"sources"`
	ArmRanks *ArmRanksBlock `json:"arm_ranks"`
}

// SourceRef is one delivered source. Only the id is read: titles and content
// have no business in a sweep artefact.
type SourceRef struct {
	ID string `json:"id"`
}

// Measure runs one arm_ranks query.
func (c *Client) Measure(ctx context.Context, req QueryRequest) (*QueryResponse, error) {
	body, status, err := c.do(ctx, http.MethodPost, "/api/query", req)
	if err != nil {
		return nil, err
	}
	if status == http.StatusForbidden || status == http.StatusBadRequest {
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrGateRefused, status, apiError(body))
	}
	if status >= 500 {
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrRetryable, status, apiError(body))
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("query: HTTP %d: %s", status, apiError(body))
	}
	var resp QueryResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("query: decode response: %w", err)
	}
	if resp.ArmRanks == nil {
		return nil, fmt.Errorf("%w: response carries no arm_ranks block — does this instance run the B-W2 seam?", ErrGateRefused)
	}
	return &resp, nil
}

// Drift takes one census over the admin-gated stats surface. goldIDs may be
// empty, in which case the response carries no gold section.
func (c *Client) Drift(ctx context.Context, goldIDs []string) (DriftStamp, error) {
	payload := map[string]any{"action": "stats", "data": map[string]any{"drift": true, "gold_ids": goldIDs}}
	body, status, err := c.do(ctx, http.MethodPost, "/api/manage", payload)
	if err != nil {
		return DriftStamp{}, err
	}
	if status != http.StatusOK {
		return DriftStamp{}, fmt.Errorf("drift census: HTTP %d: %s", status, apiError(body))
	}
	var resp struct {
		Drift *DriftStamp `json:"drift"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return DriftStamp{}, fmt.Errorf("drift census: decode: %w", err)
	}
	if resp.Drift == nil {
		return DriftStamp{}, fmt.Errorf("drift census: stats response carries no drift block — admin key required, and the instance must carry B-W5")
	}
	return *resp.Drift, nil
}

// MigrationsMax reads the applied schema generation off the status surface, so
// a report pins the SQL the measurement ran against rather than the SQL the
// repository happens to hold.
func (c *Client) MigrationsMax(ctx context.Context) (int, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/api/status", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("status: HTTP %d: %s", status, apiError(body))
	}
	var resp struct {
		DB *struct {
			MigrationsMax int `json:"migrations_max"`
		} `json:"db"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, fmt.Errorf("status: decode: %w", err)
	}
	if resp.DB == nil {
		return 0, fmt.Errorf("status: response carries no db section (server-admin key required)")
	}
	return resp.DB.MigrationsMax, nil
}

// EfSearchEffective reads hnsw.ef_search off the status surface — the value the
// MEASURED instance's backends run under, marked "(default)" when it is the
// compiled-in one (handler/status_db.go:422-455).
//
// Read from the instance rather than from a ctx setting because no ctx setting
// drives it on the query path: the arm statement sets iterative_scan and
// max_scan_tuples per call (142:216-220) and leaves ef_search to the server. It
// is stamped so a campaign whose halves ran under different ANN windows can be
// refused instead of compared (design/05 §4.4b, F-23).
func (c *Client) EfSearchEffective(ctx context.Context) (string, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/api/status", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("status: HTTP %d: %s", status, apiError(body))
	}
	var resp struct {
		DB *struct {
			HNSW struct {
				EfSearchEffective string `json:"ef_search_effective"`
			} `json:"hnsw"`
		} `json:"db"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("status: decode: %w", err)
	}
	if resp.DB == nil {
		return "", fmt.Errorf("status: response carries no db section (server-admin key required)")
	}
	return resp.DB.HNSW.EfSearchEffective, nil
}

// Setting reads ONE effective setting value. The post-fusion stage state is
// read this way and not from the driver's environment: the stages that touch a
// delivered ranking are configured in the database, and an env variable on the
// measuring host says nothing about the instance being measured.
func (c *Client) Setting(ctx context.Context, key string) (any, error) {
	body, status, err := c.do(ctx, http.MethodGet, "/api/settings/"+key, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("setting %s: HTTP %d: %s", key, status, apiError(body))
	}
	var resp struct {
		Setting struct {
			Value any `json:"value"`
		} `json:"setting"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("setting %s: decode: %w", key, err)
	}
	return resp.Setting.Value, nil
}

// PostStageKeys are the four settings whose state decides whether a DELIVERED
// ranking can be compared to a fusion at all (§4.8).
var PostStageKeys = []string{"cluster.enabled", "cluster.inject_max", "graph.enabled", "rerank.enabled"}

// PostStageState reads all four in one call each, in a fixed order.
func (c *Client) PostStageState(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range PostStageKeys {
		v, err := c.Setting(ctx, k)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// do issues one request and classifies transport failures as retryable.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Context-Key", c.Key)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: request to %s: %w", ErrRetryable, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%w: read response: %w", ErrRetryable, err)
	}
	return respBody, resp.StatusCode, nil
}

// apiError extracts the error string of an envelope, falling back to a bounded
// slice of the raw body. Bounded because an unexpected body could be an HTML
// error page from a proxy, and a whole page in a log line helps nobody.
func apiError(body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		return env.Error
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

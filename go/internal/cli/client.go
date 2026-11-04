package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponseSize is the maximum number of bytes read from API responses (10 MB).
const maxResponseSize = 10 * 1024 * 1024

// Client is the HTTP client for the context store API.
type Client struct {
	BaseURL    string
	Key        string
	HTTPClient *http.Client

	// maxResponse caps Post/Do/Get. It is a per-client PARAMETER, not an
	// inherited constant: the statusline reads at most 1 MB on a 500 ms
	// budget, the main client 10 MB (design/03 §5.6). Zero means "the 10 MB
	// default", so a bare &Client{…} literal keeps reading responses instead
	// of silently returning empty bodies.
	maxResponse int64
}

// newHTTPClient builds the transport half of a ctx-owned client. The three
// timeouts stay DIFFERENT on purpose (design/03 §5.6): 120 s carries LLM answer
// times, 5 s is the init probe budget, 500 ms is the statusline's per-prompt
// budget. Transport stays nil = http.DefaultTransport, which is what all four
// hand-written constructions used — so proxy resolution (ProxyFromEnvironment)
// and HTTP/2 negotiation are unchanged.
//
// The response cap is NOT a field of http.Client: it belongs to whoever reads
// the body. Client carries it (maxResponse), and postIngest — which streams its
// own decode off c.HTTPClient (ingest.go) — keeps reading uncapped as before.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}

// NewClient creates a new API client from config.
func NewClient(cfg Config) *Client {
	return NewClientWithTimeout(cfg, 120*time.Second, maxResponseSize)
}

// NewClientWithTimeout creates an API client with an explicit call budget and
// an explicit read cap. The statusline uses it for its 500 ms / 1 MB pair; the
// cap travels with the client so moving a call onto Client cannot silently
// widen it (design/03 §5.6, third point).
func NewClientWithTimeout(cfg Config, timeout time.Duration, maxResponse int64) *Client {
	return &Client{
		BaseURL:     cfg.BaseURL,
		Key:         cfg.Key,
		HTTPClient:  newHTTPClient(timeout),
		maxResponse: maxResponse,
	}
}

// readLimit is the byte cap this client applies to a response body.
func (c *Client) readLimit() int64 {
	if c.maxResponse <= 0 {
		return maxResponseSize
	}
	return c.maxResponse
}

// Post sends a POST request to BaseURL/endpoint with JSON body.
// BaseURL points to the API root, endpoint is e.g. "query", "store".
func (c *Client) Post(endpoint string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	url := strings.TrimSuffix(c.BaseURL, "/") + "/api/" + endpoint
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Context-Key", c.Key)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, c.readLimit()))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return respBody, nil
}

// Do sends a request with an arbitrary method to BaseURL/path (path starts
// with "/api/…"). body may be nil. Unlike Post (endpoint-POST only), Do
// serves the REST-shaped settings/secrets surface (GET/PUT/DELETE on
// path-addressed resources). Returns body and status code — the caller
// parses the envelope (success:false ⇒ stderr + exit 1, never a silent 0).
func (c *Client) Do(method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(data)
	}

	url := strings.TrimSuffix(c.BaseURL, "/") + path
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Context-Key", c.Key)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, c.readLimit()))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// Get sends a GET request to the given path URL.
func (c *Client) Get(path string) ([]byte, error) {
	url := path
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("X-Context-Key", c.Key)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, c.readLimit()))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return respBody, nil
}

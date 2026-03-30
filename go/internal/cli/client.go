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
}

// NewClient creates a new API client from config.
func NewClient(cfg Config) *Client {
	return &Client{
		BaseURL: cfg.BaseURL,
		Key:     cfg.Key,
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return respBody, nil
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return respBody, nil
}

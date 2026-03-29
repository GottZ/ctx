package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
// BaseURL already includes /webhook, so endpoint is e.g. "context-agent".
func (c *Client) Post(endpoint string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal body: %w", err)
	}

	url := c.BaseURL + "/" + endpoint
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
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return respBody, nil
}

// Get sends a GET request to BaseURL (with path override for non-webhook endpoints).
func (c *Client) Get(path string) ([]byte, error) {
	// For health endpoint, we strip /webhook from BaseURL and use the raw path
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
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return respBody, nil
}

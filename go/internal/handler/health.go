package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthHandler provides health check endpoints.
type HealthHandler struct {
	pool      *pgxpool.Pool
	embedHost string
	chatHost  string
	dreamHost string // optional, empty means not configured separately
}

// NewHealthHandler creates a new HealthHandler.
func NewHealthHandler(pool *pgxpool.Pool, embedHost, chatHost, dreamHost string) *HealthHandler {
	return &HealthHandler{
		pool:      pool,
		embedHost: embedHost,
		chatHost:  chatHost,
		dreamHost: dreamHost,
	}
}

type healthResponse struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
}

// Health checks database connectivity and Ollama availability for all configured hosts.
//
// Status logic:
//   - DB down → 503 "unhealthy"
//   - Embed down → 503 "unhealthy" (embeddings are mandatory for store+search)
//   - Chat down → 200 "degraded" (queries broken, but store works)
//   - Dream down → 200 "ok" (background process, not critical)
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	services := make(map[string]string)

	// Database ping
	if err := h.pool.Ping(ctx); err != nil {
		services["database"] = "error"
		slog.Error("health check: database ping failed", "error", err)
	} else {
		services["database"] = "ok"
	}

	// Embed host ping (critical — embeddings are mandatory)
	if err := h.pingHost(ctx, h.embedHost); err != nil {
		services["embed"] = "error"
		slog.Error("health check: embed host ping failed", "error", err, "host", h.embedHost)
	} else {
		services["embed"] = "ok"
	}

	// Chat host ping (degraded if down — queries won't work)
	if err := h.pingHost(ctx, h.chatHost); err != nil {
		services["chat"] = "error"
		slog.Error("health check: chat host ping failed", "error", err, "host", h.chatHost)
	} else {
		services["chat"] = "ok"
	}

	// Dream host ping (optional — background process)
	if h.dreamHost != "" {
		if err := h.pingHost(ctx, h.dreamHost); err != nil {
			services["dream"] = "error"
			slog.Warn("health check: dream host ping failed", "error", err, "host", h.dreamHost)
		} else {
			services["dream"] = "ok"
		}
	}

	// Determine overall status
	statusCode := http.StatusOK
	status := "ok"

	if services["database"] != "ok" || services["embed"] != "ok" {
		statusCode = http.StatusServiceUnavailable
		status = "unhealthy"
	} else if services["chat"] != "ok" {
		status = "degraded"
	}

	resp := healthResponse{
		Status:   status,
		Services: services,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("health check: failed to encode response", "error", err)
	}
}

func (h *HealthHandler) pingHost(ctx context.Context, host string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to host: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close response body", "error", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

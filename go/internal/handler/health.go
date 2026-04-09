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
}

// NewHealthHandler creates a new HealthHandler.
func NewHealthHandler(pool *pgxpool.Pool, embedHost string) *HealthHandler {
	return &HealthHandler{
		pool:      pool,
		embedHost: embedHost,
	}
}

type healthResponse struct {
	Status  string            `json:"status"`
	Checks  map[string]string `json:"checks"`
}

// Health checks database connectivity and Ollama availability.
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	checks := make(map[string]string)

	// Database ping
	if err := h.pool.Ping(ctx); err != nil {
		checks["database"] = "error"
		slog.Error("health check: database ping failed", "error", err)
	} else {
		checks["database"] = "ok"
	}

	// Ollama ping (non-critical: does not cause 503)
	if err := h.pingOllama(ctx); err != nil {
		checks["ollama"] = "error"
		slog.Error("health check: ollama ping failed", "error", err)
	} else {
		checks["ollama"] = "ok"
	}

	dbHealthy := checks["database"] == "ok"
	statusCode := http.StatusOK
	resp := healthResponse{
		Status: "ok",
		Checks: checks,
	}

	if !dbHealthy {
		statusCode = http.StatusServiceUnavailable
		resp.Status = "unhealthy"
	} else if checks["ollama"] != "ok" {
		resp.Status = "degraded"
		// statusCode remains 200 — Ollama down is not a hard failure
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("health check: failed to encode response", "error", err)
	}
}

func (h *HealthHandler) pingOllama(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.embedHost, nil)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connecting to ollama: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close ollama response body", "error", closeErr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

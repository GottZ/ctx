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

// HealthHandler provides health check endpoints. It holds no host copies —
// the ping targets come from one config snapshot per request (F1-W7), so
// after a config replace the health check probes the backends the pipelines
// actually use, never a stale boot copy.
type HealthHandler struct {
	pool *pgxpool.Pool
	cfg  ConfigStore
}

// NewHealthHandler creates a new HealthHandler.
func NewHealthHandler(pool *pgxpool.Pool, cfg ConfigStore) *HealthHandler {
	return &HealthHandler{pool: pool, cfg: cfg}
}

// healthResponse is the public wire shape: role names mapped to ok|error,
// nothing else. The route is unauthenticated and proxied to the public
// internet — host strings or model names in any field would leak internal
// topology, so the shape stays name-free by invariant (pinned by
// TestHealthShapeInvariant, design §3.5).
type healthResponse struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
}

// Health checks database connectivity and inference-backend availability for
// all hosts in the current config snapshot.
//
// Status logic:
//   - DB down → 503 "unhealthy"
//   - Embed down → 503 "unhealthy" (embeddings are mandatory for store+search)
//   - Chat down → 200 "degraded" (queries broken, but store works)
//   - Dream down → 200 "ok" (background process, not critical)
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// One snapshot per request: ping targets and pipeline consumers read the
	// same generation (until F2 ships Replace callers this is byte-identical
	// to the boot config — Delta 2).
	cfg := h.cfg.Snapshot()

	services := make(map[string]string)

	// Database ping
	if err := h.pool.Ping(ctx); err != nil {
		services["database"] = "error"
		slog.Error("health check: database ping failed", "error", err)
	} else {
		services["database"] = "ok"
	}

	// Embed host ping (critical — embeddings are mandatory)
	if err := pingHost(ctx, cfg.Embed.Host); err != nil {
		services["embed"] = "error"
		slog.Error("health check: embed host ping failed", "error", err, "host", cfg.Embed.Host)
	} else {
		services["embed"] = "ok"
	}

	// Chat host ping (degraded if down — queries won't work)
	if err := pingHost(ctx, cfg.Chat.Host); err != nil {
		services["chat"] = "error"
		slog.Error("health check: chat host ping failed", "error", err, "host", cfg.Chat.Host)
	} else {
		services["chat"] = "ok"
	}

	// Dream host ping (optional — background process)
	if cfg.Dream.Host != "" {
		if err := pingHost(ctx, cfg.Dream.Host); err != nil {
			services["dream"] = "error"
			slog.Warn("health check: dream host ping failed", "error", err, "host", cfg.Dream.Host)
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

func pingHost(ctx context.Context, host string) error {
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

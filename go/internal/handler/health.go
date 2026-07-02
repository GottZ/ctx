package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthHandler provides health check endpoints. Since F3-P2 the
// service-class aggregation feeds from the backend pool (role embed has ≥1
// reachable backend? role synthesis?) instead of the three startup hosts —
// the second deliberate evolution of the health source (K9); the invariant
// stays identical both times: the response shape is host-, name- and
// gaming-free (the route is unauthenticated and public).
type HealthHandler struct {
	pool        *pgxpool.Pool
	cfg         ConfigStore
	backendPool *backends.Pool
	blocktypes  BlocktypeHealth
}

// BlocktypeHealth reports the block-type registry degradation state for the
// /health field blocktype_registry (WF T3, design 01 §4.3): "ok" or
// "builtin-fallback". Implemented by *blocktype.Registry; an interface here
// keeps handler free of the blocktype import (and lets tests fake the
// degraded state without a registry).
type BlocktypeHealth interface {
	Health() string
}

// NewHealthHandler creates a new HealthHandler. blocktypes may be nil
// (tests): the field then reports "ok" — the single-binary boot always
// wires the registry.
func NewHealthHandler(pool *pgxpool.Pool, cfg ConfigStore, backendPool *backends.Pool, blocktypes BlocktypeHealth) *HealthHandler {
	return &HealthHandler{pool: pool, cfg: cfg, backendPool: backendPool, blocktypes: blocktypes}
}

// blocktypeHealthValue maps a possibly-nil BlocktypeHealth to the wire value.
func blocktypeHealthValue(bt BlocktypeHealth) string {
	if bt == nil {
		return "ok"
	}
	return bt.Health()
}

// healthResponse is the public wire shape: role names mapped to ok|error,
// nothing else. The route is unauthenticated and proxied to the public
// internet — host strings, backend names, model names, cooldown detail or
// the gaming flag (a presence oracle) in any field would leak internal
// topology, so the shape stays name-free by invariant (pinned by
// TestHealthShapeInvariant, design §2.9). The per-backend detail lives
// exclusively behind the admin-gated backend-list.
type healthResponse struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
	// BlocktypeRegistry is "ok" or "builtin-fallback" (WF T3, design 01
	// §4.3 b): the visibility-policy registry failed its DB load and runs on
	// the compiled-in builtin set — loud, because the fallback REVERTS
	// operator visibility narrowings. Not a services entry: services carry
	// ping-based role reachability, this is process-internal degradation
	// state. Name-free like everything else in this body.
	BlocktypeRegistry string `json:"blocktype_registry"`
}

// HealthStatus computes the service-class health aggregate from ONE pool
// snapshot: DB ping + per-role reachability (embed, chat=synthesis, optionally
// dream). It is shared by the public /health handler and the admin status
// collector (G33) so the two can never drift — same source, same shape. The
// result is name-free by invariant (TestHealthShapeInvariant); the caller maps
// the status string to an HTTP code.
//
// Status logic (unchanged):
//   - DB down → "unhealthy" (503 at the /health edge)
//   - Embed down → "unhealthy" (embeddings are mandatory for store+search)
//   - Chat down → "degraded" (queries broken, but store works)
//   - Dream down → "ok" (background process, not critical)
//   - Blocktype registry on builtin-fallback → "degraded" (WF T3: reads work,
//     but the visibility policy runs on compiled-in defaults)
func HealthStatus(ctx context.Context, pool *pgxpool.Pool, snap []backends.Backend, dreamEnabled bool, blocktypeRegistry string) healthResponse {
	services := make(map[string]string)

	// Database ping
	if err := pool.Ping(ctx); err != nil {
		services["database"] = "error"
		slog.Error("health check: database ping failed", "error", err)
	} else {
		services["database"] = "ok"
	}

	// Per-role reachability from the given snapshot. The service keys keep
	// their historical names ("chat" = the synthesis-serving class).
	services["embed"] = roleReachable(ctx, snap, backends.RoleEmbed)
	services["chat"] = roleReachable(ctx, snap, backends.RoleSynthesis)
	if dreamEnabled {
		services["dream"] = roleReachable(ctx, snap, backends.RoleDream)
	}

	return healthResponse{
		Status:            aggregateHealthStatus(services, blocktypeRegistry),
		Services:          services,
		BlocktypeRegistry: blocktypeRegistry,
	}
}

// aggregateHealthStatus folds the per-service results and the blocktype
// registry state into the overall status (pure — unit-testable without a DB).
func aggregateHealthStatus(services map[string]string, blocktypeRegistry string) string {
	switch {
	case services["database"] != "ok" || services["embed"] != "ok":
		return "unhealthy"
	case services["chat"] != "ok" || blocktypeRegistry != "ok":
		return "degraded"
	default:
		return "ok"
	}
}

// Health serves the public /health endpoint, wrapping HealthStatus with the
// HTTP status-code mapping (unhealthy → 503, else 200).
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: /health is public/anonymous — no tenant in reach; dream.enabled is server-global.
	snap := h.backendPool.Snapshot()
	resp := HealthStatus(ctx, h.pool, snap, cfg.Dream.Enabled, blocktypeHealthValue(h.blocktypes))

	statusCode := http.StatusOK
	if resp.Status == "unhealthy" {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("health check: failed to encode response", "error", err)
	}
}

// roleReachable reports ok when at least one enabled backend of the role
// answers its base URL. Errors log the backend name to slog ONLY — the
// response carries the aggregate.
func roleReachable(ctx context.Context, snap []backends.Backend, role string) string {
	candidates := 0
	for i := range snap {
		b := &snap[i]
		if !b.Enabled || !b.HasRole(role) {
			continue
		}
		candidates++
		if err := pingHost(ctx, b.Host); err != nil {
			slog.Warn("health check: backend ping failed", "backend", b.Name, "role", role, "error", err)
			continue
		}
		return "ok"
	}
	if candidates == 0 {
		slog.Warn("health check: no enabled backend for role", "role", role)
	}
	return "error"
}

func pingHost(ctx context.Context, host string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host, nil) //nolint:gosec // G704: host is a context_backends pool row (admin-gated, locality-validated), not free user input — same false positive as the Do() below. The G34 SSE path (HandleEvents → Snapshot(r.Context()) → HealthStatus → pingHost) lets gosec's taint analysis reach this line too.
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: pool rows are admin-gated runtime data (locality-validated)
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

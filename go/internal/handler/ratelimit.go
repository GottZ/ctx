package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// rateLimitBlocked is the ONE rate-limit preamble of the read surfaces: count
// this key's rows in the action bucket, fail closed on a store error, refuse
// with 429 once the count has reached the limit. It answers on w and reports
// whether it did, so a caller's whole job is `if rateLimitBlocked(…) { return }`.
//
// limit is a VALUE, not a config lookup inside: six callers budget against
// Query.RateLimitRead, POST /api/digest against Query.RateLimitWrite (it
// rebuilds a corpus-wide artefact). Reading the field here would move digest
// onto the read budget silently. limit <= 0 is "disabled" and never touches the
// store — the same convention the seven inline preambles spelled out.
//
// bucket is the action argument, noun the 429 sentence ("reads" / "graph reads"
// / "digest runs"), logMsg the slog sentence. The surfaces differ in exactly
// those three and agree on everything else. The request id comes from ctx,
// which is where every caller read it.
//
// NOT a caller: meterBlobWrite (blob_core.go) books the ActionBlobWrite WRITE
// bucket and answers through *writeReject instead of w.
func rateLimitBlocked(w http.ResponseWriter, ctx context.Context, pool *pgxpool.Pool,
	apiKeyID, bucket, logMsg, noun string, limit int,
) bool {
	if limit <= 0 {
		return false
	}
	count, err := store.CheckRateLimitByAction(ctx, pool, apiKeyID, bucket)
	if err != nil {
		slog.Error(logMsg, "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "Internal server error"})
		return true
	}
	if count >= limit {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("Rate limit exceeded: max %d %s per 60 seconds", limit, noun),
		})
		return true
	}
	return false
}

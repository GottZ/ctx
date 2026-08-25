// W02-8: the ONE blob write path, shared by POST /api/blob/store and the MCP
// blob_store tool.
//
// It is the blob twin of stage_gates.go, and it exists for the reason that
// file states in its own header: when two transports carry the same write,
// the second one grows its own subset of the first one's gates. The blob
// surface already paid that bill once — /api/blob/store used to resolve its
// write scope through a narrower formula of its own, so a key that could
// write BLOCKS in a scope was refused every BLOB in it (Gap-C0-c, wave B3).
// Adding a second transport by copying the handler would have reopened
// exactly that split, one gate at a time.
//
// The split is by ORDER, not by convenience: the surfaces call the three
// steps below in the same sequence the REST handler has always run them in
// (required fields + name caps → payload decode + size caps → scope gate →
// budget → upsert). A surface therefore cannot answer a different rejection
// for the same request merely by checking things in a different order.
package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Payload ceilings of the blob surface, verbatim the values the REST handler
// carried inline: a base64 pre-check that refuses before a decode buffer is
// allocated (70 MB of base64 ≈ 52.5 MB decoded), and the decoded cap itself.
const (
	blobBase64MaxBytes = 70 * 1024 * 1024
	blobDataMaxBytes   = 50 * 1024 * 1024
)

// blobWriteInput is the surface-independent form of one blob write: what the
// gates decide over and what the upsert writes. Data is DECODED — how a
// transport spells its payload (base64 on the wire, text on the MCP tool) is
// the transport's business and ends at decodeBlobFile/blobTextPayload.
type blobWriteInput struct {
	Category string
	Title    string
	Filename string
	MimeType string
	Scope    string // explicit scope; empty = the key's home scope
	Data     []byte
	Tags     []string
	Metadata map[string]any
}

// blobReject is a blob-write rejection: the shared writeReject plus the
// class-23 detail fields the REST envelope has carried since B1 (sqlstate,
// constraint). The extras ride BESIDE the verdict rather than inside the
// message, so the MCP surface can render code + prose and ignore them without
// either surface owning a second rejection vocabulary.
type blobReject struct {
	*writeReject
	Extra map[string]any
}

// blobRejection wraps a plain verdict for the surfaces that only ever see one.
func blobRejection(rej *writeReject) *blobReject { return &blobReject{writeReject: rej} }

// writeBlobReject answers a REST blob rejection: writeJSONReject's envelope,
// plus the class-23 detail fields when the verdict carries them.
func writeBlobReject(w http.ResponseWriter, rej *blobReject) {
	if len(rej.Extra) == 0 {
		writeJSONReject(w, rej.writeReject)
		return
	}
	body := map[string]any{"success": false, "error": rej.Msg}
	for k, v := range rej.Extra {
		body[k] = v
	}
	writeJSONCode(w, rej.Status, rej.Code, body)
}

// blobFieldGates is step 1: the required fields and the two name caps, over
// the WIRE form — hasPayload says whether the transport received a payload
// field at all, because "no payload" is a missing field and not an empty one
// (a zero-byte blob has never been storable on this surface).
//
// The message names `file` on every surface deliberately: it is the blob
// payload's name in the vocabulary both transports share, and a per-surface
// wording would be the first crack in a single verdict.
func blobFieldGates(hasPayload bool, filename, category, title, mimeType string) *writeReject {
	if !hasPayload || filename == "" || category == "" || title == "" || mimeType == "" {
		return classMissingFields.reject("Missing required fields: file, filename, category, title, mime_type")
	}
	if len(category) > 100 {
		return classSizeCap.reject("Category exceeds 100 characters")
	}
	if len(title) > 500 {
		return classSizeCap.reject("Title exceeds 500 characters")
	}
	return nil
}

// decodeBlobFile is step 2 for a base64 payload: OOM pre-check, decode, size
// cap. The pre-check runs BEFORE the decode so an oversized upload never
// allocates its own decode buffer.
func decodeBlobFile(fileB64 string) ([]byte, *writeReject) {
	if len(fileB64) > blobBase64MaxBytes {
		return nil, classSizeCap.reject("File exceeds 50MB limit")
	}
	data, err := base64.StdEncoding.DecodeString(fileB64)
	if err != nil {
		return nil, classInvalidEncoding.reject("Invalid base64 encoding in 'file' field")
	}
	if len(data) > blobDataMaxBytes {
		return nil, classSizeCap.reject("File exceeds 50 MB limit")
	}
	return data, nil
}

// blobTextPayload is step 2 for a payload handed over as text: the same
// decoded cap, no base64 stage. The pre-check has no counterpart here —
// there is no second, larger encoded form to refuse ahead of.
func blobTextPayload(text string) ([]byte, *writeReject) {
	if len(text) > blobDataMaxBytes {
		return nil, classSizeCap.reject("File exceeds 50 MB limit")
	}
	return []byte(text), nil
}

// blobWriteGate is step 3: the scope gate, then the write budget.
//
// The ORDER is the mechanism, not a preference. The scope gate resolves
// through writableBlockScopes — the SINGLE eval point of the write gate
// (078/E4b), shared verbatim with the block write path, which is what keeps
// the blob surface from re-deriving its own narrower formula (Gap-C0-c). It
// stays AHEAD of the budget so a scope the key may not write is refused
// before any budget is booked: spraying foreign scopes must not drain a
// legitimate key's quota.
//
// The returned logID is the booked intent's audit row — the caller attributes
// the stored blob to it (direct path) or leaves it unattributed (staged path,
// where no blob exists yet).
func blobWriteGate(ctx context.Context, pool *pgxpool.Pool, cfg ConfigStore, ar *auth.AuthResult, in blobWriteInput, reqID string) (writeScope, logID string, rej *writeReject) {
	writeScope = ar.HomeScope
	if in.Scope != "" {
		if !contains(writableBlockScopes(ar), in.Scope) {
			return "", "", classScopeDenied.reject("Cannot write to requested scope")
		}
		writeScope = in.Scope
	}

	logID, rej = meterBlobWrite(ctx, pool, cfg, ar, reqID)
	if rej != nil {
		return "", "", rej
	}
	return writeScope, logID, nil
}

// meterBlobWrite applies the blob write budget and books the row that pays
// into it, returning that row's id for the later blob attribution.
//
// Gate and booking live in ONE function because they are one mechanism: the
// gate counts exactly the rows the booking writes (store.ActionBlobWrite), and
// a caller that ran one without the other would either meter an ungated surface
// or gate an unfed counter. The budget is the blob's OWN
// (pool.blob_rate_limit_write, with Config.BlobWriteLimit owning the fallback
// to query.rate_limit_write; 0 = disabled), NOT the block-write budget this
// path used to borrow. MT 06-C5: per-tenant via the request context (tenant's
// own override, else _global).
//
// The booking is SYNCHRONOUS and precedes the upsert, on purpose. A write that
// dies in a constraint violation has already cost the decode, the checksum and
// an INSERT attempt of up to 50 MB, so it must cost budget too — otherwise the
// surface is free to hammer with payloads that never commit. And the row IS the
// budget: booked after the fact, the gate reads a counter that lags its own
// writes and N concurrent requests all pass an empty one. A booking failure is
// fail-closed (500) for the same reason: an unbookable write is an ungated one.
//
// A nil cfg is a test wiring without a config snapshot and reads as "no
// limit" — it never skips the BOOKING, because a surface that books nothing
// is a surface the gate can never see.
func meterBlobWrite(ctx context.Context, pool *pgxpool.Pool, cfg ConfigStore, ar *auth.AuthResult, reqID string) (string, *writeReject) {
	limit := 0
	if cfg != nil {
		limit = cfg.SnapshotForRequest(ctx).BlobWriteLimit()
	}
	if limit > 0 {
		writeCount, err := store.CheckRateLimitByAction(ctx, pool, ar.ApiKeyID, store.ActionBlobWrite)
		if err != nil {
			slog.Error("blob-store: rate limit check error", "error", err, "request_id", reqID)
			return "", classInternal.reject("Internal server error")
		}
		if writeCount >= limit {
			// Same CLASS as the block-write budget, deliberately its own
			// wording (Gap-C6-c): the code is what a client branches on, the
			// sentence is what a human reads.
			return "", classRateLimit.reject(
				fmt.Sprintf("Rate limit exceeded: max %d blob writes per 60 seconds", limit))
		}
	}

	logID, err := store.LogAccessRef(ctx, pool, ar.ApiKeyID, "", store.ActionBlobWrite)
	if err != nil {
		slog.Error("blob-store: write metering error", "error", err, "request_id", reqID)
		return "", classInternal.reject("Internal server error")
	}
	return logID, nil
}

// executeBlobWrite is the whole write: gate → budget → upsert → attribution.
// Steps 1 and 2 (fields, decode) belong to the transport, which is where the
// payload's encoding lives; everything from the scope gate on is identical for
// every surface and lives here.
func executeBlobWrite(ctx context.Context, pool *pgxpool.Pool, cfg ConfigStore, ar *auth.AuthResult, in blobWriteInput, reqID string) (*store.BlobMeta, *blobReject) {
	writeScope, logID, rej := blobWriteGate(ctx, pool, cfg, ar, in, reqID)
	if rej != nil {
		return nil, blobRejection(rej)
	}

	blob, err := store.UpsertBlob(ctx, pool, in.Category, in.Title, in.Filename, in.MimeType, writeScope, in.Data, in.Tags, in.Metadata)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23") {
			status, reason := blobConstraintError(pgErr)
			slog.Warn("blob-store: constraint violation", "error", err,
				"sqlstate", pgErr.Code, "constraint", pgErr.ConstraintName, "request_id", reqID)
			// Keeps the class-23 detail fields (sqlstate, constraint) the B1
			// wave added — the code is one more field beside them, not a
			// replacement.
			return nil, &blobReject{
				writeReject: classConstraint.rejectStatus(status, reason),
				Extra: map[string]any{
					"sqlstate": pgErr.Code, "constraint": pgErr.ConstraintName,
				},
			}
		}
		slog.Error("blob-store: upsert error", "error", err, "request_id", reqID)
		return nil, blobRejection(classInternal.reject("Internal server error"))
	}

	// Attribute the stored blob to the row booked above (fire-and-forget). Only
	// the ATTRIBUTION is async — the metering row itself already exists, so a
	// lost goroutine costs the audit trail one reference, never a budget entry.
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if logErr := store.AttributeAccessBlob(bgCtx, pool, logID, blob.ID); logErr != nil {
			slog.Error("blob-store: write attribution error", "error", logErr, "request_id", reqID)
		}
	}()

	return blob, nil
}

// blobConstraintError maps a Postgres integrity-constraint violation (SQLSTATE
// class 23) onto an HTTP status and a named reason. Such a violation is a
// property of the REQUEST, not of the server: a uniqueness collision is the
// caller's to resolve (409), every other class-23 violation means the payload
// itself cannot be stored as sent (422). Callers outside class 23 keep the
// opaque 500 — those are server faults and must not leak SQL detail.
func blobConstraintError(pgErr *pgconn.PgError) (int, string) {
	name := pgErr.ConstraintName
	if name == "" {
		name = "unknown"
	}
	switch pgErr.Code {
	case "23505":
		return http.StatusConflict, fmt.Sprintf("Blob violates unique constraint %q", name)
	case "23503":
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob violates foreign key constraint %q", name)
	case "23502":
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob is missing a required value for column %q", pgErr.ColumnName)
	case "23514":
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob violates check constraint %q", name)
	default:
		return http.StatusUnprocessableEntity, fmt.Sprintf("Blob violates constraint %q", name)
	}
}

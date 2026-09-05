// Gap-C6-c: the machine-readable rejection vocabulary of the WRITE surfaces.
//
// B5/Gap-C6-a unified the gate CHAIN (runStageWriteGates) across REST,
// MCP-direct and MCP-staged, so the three surfaces now decide identically —
// but the verdict travelled as free text only. A client that wants to branch
// on "budget exhausted" had to string-match server prose, and the same class
// is worded differently per surface ("Missing required fields: category,
// title, content" vs "category, title, and content are required"; "max N
// writes per 60 seconds" vs "max N blob writes per 60 seconds"). This file
// adds the code that rides ALONGSIDE the prose — the wording is untouched.
//
// One table, no per-surface copies: a rejection is built from a rejectClass,
// which owns BOTH the HTTP status and the code. REST renders it via
// writeJSONReject, MCP via errResultReject/errResult, and runStageWriteGates
// returns it verbatim — so a class can never carry two different codes.
//
// SCOPE (deliberately narrow, the boundary is the wave's): the write ENTRY
// points — POST /api/store, POST /api/blob/store (including its metering
// gate) and the MCP `store` tool (both arms) — plus the unknown-action
// verdict of the /api/blob/manage dispatcher, which routes before any action
// is chosen. Read handlers, the manage dispatchers' per-action handlers and
// the `confirm`/`update` tools keep their uncoded envelopes; coding them is a
// separate sweep, not this one.
package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rejectClass binds an HTTP status to a machine-readable code. The set below
// is CLOSED and derived from the rejections the write paths actually emit —
// there are no reserve codes waiting for a future caller.
type rejectClass struct {
	status int
	code   string
}

// The write surfaces' rejection classes. Codes are stable API: a client may
// branch on them, so a value here is renamed only with the same care as a
// route.
var (
	// classUnauthorized — no valid key/identity resolved for the write.
	classUnauthorized = rejectClass{http.StatusUnauthorized, "unauthorized"}
	// classInvalidBody — the request body is not decodable JSON.
	classInvalidBody = rejectClass{http.StatusBadRequest, "invalid_body"}
	// classMissingFields — a required field of the write is absent.
	classMissingFields = rejectClass{http.StatusBadRequest, "missing_fields"}
	// classInvalidSensitivity — `sensitivity` is not one of the four levels.
	classInvalidSensitivity = rejectClass{http.StatusBadRequest, "invalid_sensitivity"}
	// classInvalidEncoding — the blob payload is not valid base64.
	classInvalidEncoding = rejectClass{http.StatusBadRequest, "invalid_encoding"}
	// classScopeDenied — the key may not write to the requested scope.
	classScopeDenied = rejectClass{http.StatusForbidden, "scope_denied"}
	// classSizeCap — a field or payload exceeds its size cap.
	classSizeCap = rejectClass{http.StatusRequestEntityTooLarge, "size_cap"}
	// classUnknownType — explicit `type` is not in the block-type registry.
	classUnknownType = rejectClass{http.StatusUnprocessableEntity, "unknown_type"}
	// classReservedType — explicit `type` names a DERIVED type (I7/S1,
	// design D-01 §4.3.1). 422 like unknown_type, because the client's
	// assertion about the entity is what is unprocessable — but a separate
	// code: the type exists, it is simply not the client's to claim, and a
	// client that branches on "typo" must not treat this as one.
	classReservedType = rejectClass{http.StatusUnprocessableEntity, "reserved_type"}
	// classReservedCategory — the write targets a category reserved for the
	// derived layer (I7/S2). 403, not 422: the payload is well-formed, the
	// caller is simply not authorised for that namespace.
	classReservedCategory = rejectClass{http.StatusForbidden, "reserved_category"}
	// classProvenanceProtected — the write would overwrite a block carrying a
	// derived provenance object (I7/S3). 403 for the same reason.
	classProvenanceProtected = rejectClass{http.StatusForbidden, "provenance_protected"}
	// classReservedMetadata — the write carries the derived layer's provenance
	// key in its own metadata (I7/S3, second half). 403, and deliberately NOT a
	// silent strip: a client that believes it wrote provenance and got a 200
	// would be told a lie about the block's standing.
	classReservedMetadata = rejectClass{http.StatusForbidden, "reserved_metadata"}
	// classUnknownAction — the dispatcher has no handler for `action`.
	classUnknownAction = rejectClass{http.StatusBadRequest, "unknown_action"}
	// classConstraint — the payload violates a DB integrity constraint. The
	// only class whose status varies per case (409 uniqueness, else 422), so
	// its call sites use rejectStatus.
	classConstraint = rejectClass{http.StatusUnprocessableEntity, "constraint"}
	// classRateLimit — the key's write budget for this surface is exhausted.
	classRateLimit = rejectClass{http.StatusTooManyRequests, "rate_limit"}
	// classInternal — a server-side fault; the message stays opaque.
	classInternal = rejectClass{http.StatusInternalServerError, "internal"}
)

// writeReject is one write-surface rejection: the HTTP status the direct path
// answers with, the machine code, and the human message. It is what
// runStageWriteGates hands back (a pre-card verdict — the staged write would
// fail at execute time, so it must never reach a ConfirmCard, D1-M2) and what
// the REST write handlers render; MCP/Chat surfaces map it into their result.
type writeReject struct {
	Status int
	Code   string
	Msg    string
}

// reject builds this class's rejection with a surface-specific message. The
// message may differ per surface — the code may not.
func (c rejectClass) reject(msg string) *writeReject {
	return &writeReject{Status: c.status, Code: c.code, Msg: msg}
}

// prefixed returns the same rejection (status AND code unchanged) with a
// prefix on the message. It exists for the batch surface: /api/ingest refuses
// the whole request for one bad chunk and has to say WHICH chunk, without
// inventing a second code for the class it is re-rendering.
func (r *writeReject) prefixed(prefix string) *writeReject {
	return &writeReject{Status: r.Status, Code: r.Code, Msg: prefix + r.Msg}
}

// rejectStatus is reject for the one class whose HTTP status depends on the
// concrete violation (classConstraint: 409 for uniqueness, 422 otherwise).
// The code stays the class's — that is the point of splitting status off.
func (c rejectClass) rejectStatus(status int, msg string) *writeReject {
	return &writeReject{Status: status, Code: c.code, Msg: msg}
}

// errResult is this class's MCP error result: the unchanged prose in
// Content[0].Text plus the code in StructuredContent.
func (c rejectClass) errResult(msg string) *mcp.CallToolResult {
	return errResultCoded(c.code, msg)
}

// writeJSONCode is writeJSON with a machine-readable code folded into the
// payload. An EMPTY code writes NOTHING — an answer without a code keeps
// exactly the envelope writeJSON has always produced, never an empty "code"
// field. data is written in place, so callers pass a fresh map (every one
// does; they are map literals at the call site).
func writeJSONCode(w http.ResponseWriter, status int, code string, data map[string]any) {
	if code != "" {
		data["code"] = code
	}
	writeJSON(w, status, data)
}

// writeJSONReject answers a REST write rejection: the historical
// {"success":false,"error":…} envelope PLUS the code. Status and code both
// come from the rejection, so a call site cannot pair one class's code with
// another class's status.
func writeJSONReject(w http.ResponseWriter, rej *writeReject) {
	writeJSONCode(w, rej.Status, rej.Code, map[string]any{
		"success": false,
		"error":   rej.Msg,
	})
}

// mcpErrorEnvelope is the MCP-side carrier of the code. It marshals to a JSON
// object, as the spec requires of structuredContent.
type mcpErrorEnvelope struct {
	Code string `json:"code"`
}

// errResultCoded is errResult plus the machine code. The code rides in
// StructuredContent — a SIBLING of the text, never inside it: Content[0].Text
// stays byte-identical to the pre-B7 wording, which every existing client
// reads. Safe against the SDK overwriting the field: it only populates
// StructuredContent when the handler returns a non-nil typed output, and the
// store tool is registered as ToolHandlerFor[storeInput, any] returning nil
// (mcp.AddTool derives no output schema for `any`, server.go).
//
// An empty code yields a plain errResult — used where IsError=true does NOT
// mean rejection (the D3-C3 "STAGED" answer: the gates PASSED, the write is
// merely unconfirmed). A client can therefore tell a staged write from a
// refused one by the presence of the code alone.
func errResultCoded(code, msg string) *mcp.CallToolResult {
	r := errResult(msg)
	if code != "" {
		r.StructuredContent = mcpErrorEnvelope{Code: code}
	}
	return r
}

// errResultReject maps a gate-chain verdict onto the MCP surface. It is the
// MCP twin of writeJSONReject: same rejection value in, same code out — which
// is what makes "one rejection class, one code across surfaces" a property of
// construction rather than of discipline.
func errResultReject(rej *writeReject) *mcp.CallToolResult {
	return errResultCoded(rej.Code, rej.Msg)
}

// internalError is the ONE way a REST handler answers a server-side failure it
// wants in the log: it writes the log line (message, cause, request id) and
// then the generic 500 envelope through writeInternal — never a second body of
// its own. Two helpers exist for this class and their difference is nameable:
// writeInternal is the SILENT writer (the caller already logged, or has nothing
// to add), internalError is its LOGGING wrapper. It replaced four per-file
// copies that differed only in receiver and, in two of them, in the prose they
// put on the wire.
//
// The cause never reaches the client: it lives in the log line alone, and the
// wire text is the fixed one writeInternal owns.
func internalError(w http.ResponseWriter, ctx context.Context, logMsg string, err error) {
	slog.Error(logMsg, "error", err, "request_id", RequestIDFromContext(ctx))
	writeInternal(w)
}

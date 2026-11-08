package oai

import (
	"fmt"
	"net/http"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
)

// The scan cap (RC-D cluster C8, gateway.scan_max_request_bytes): the
// sensitivity scan of a request to an alias below credentials books 23 × its
// body + 128 KiB + its nesting state (scanBooking) and holds it until the
// request ends — at the 8-MiB body cap 185 MiB of a 256-MiB budget for
// 8.5–13.8 s (bau report RCD-c8-hardening-S1 §Budget). The cap bounds the
// body that scan reads; a longer one is refused before anything of the scan
// is booked. Requests at credentials are not scanned (federation.ScanRaise
// returns at once there) and keep the body cap.

// EffectiveScanCap is the largest body the scan reads: scanMax, or the body
// cap bodyCap where that is smaller — above it the setting has no effect,
// the body cap refuses first (V58 couples the two only through this min, so
// that lowering gateway.max_request_bytes alone stays a valid write). A
// scanMax of 0 or less (a snapshot without the key; the configuration
// never yields one, V58) is the body cap. The configuration's budget floor
// (V39) books the scanned class at this cap.
func EffectiveScanCap(scanMax, bodyCap int64) int64 {
	if scanMax <= 0 || scanMax > bodyCap {
		return bodyCap
	}
	return scanMax
}

// overScanCap refuses a body the scan would read past its cap: for an alias
// below credentials (required, the request's sensitivity before the scan)
// and a body longer than EffectiveScanCap it answers 413 request_too_large
// — the access record's reason, the refusal ledger's capacity /
// request_too_large (refuseTooLarge's declaration), an envelope that names
// the cap and the way out — and reports true; the caller returns before
// the scan's booking, the scan, the meter's admission and any wire call.
func overScanCap(rec *AccessRecord, w http.ResponseWriter, required backends.Sensitivity, body []byte, s Settings) bool {
	if required.Rank() >= backends.SensCredentials.Rank() {
		return false
	}
	limit := EffectiveScanCap(s.ScanMaxRequestBytes, s.MaxRequestBytes)
	if int64(len(body)) <= limit {
		return false
	}
	if rec != nil {
		rec.Reason = CodeRequestTooLarge
	}
	rec.NoteRejection(keyquota.RejectCapacity, keyquota.DetailRequestTooLarge)
	Write(w, Error{Status: http.StatusRequestEntityTooLarge, Type: TypeInvalidRequest, Code: CodeRequestTooLarge,
		Message: fmt.Sprintf("The request body exceeds the limit of %d bytes for this model: its requests are scanned for sensitive content, and the scan reads at most that much. A model of sensitivity credentials is not scanned and accepts bodies up to %d bytes.", limit, s.MaxRequestBytes)})
	return true
}

package handler

// Issue #505, review of the linked pull request: an admin write answers a
// value the database cannot store with 400 and no line, and only those: the
// codes of class 22 a request's value can cause (a NUL character in text or
// JSON, a value longer than its column, a number out of its column's range).
// 22P02 — a text that is no literal of its column's type — is not among them:
// no admin route was found where a request's value reaches such a cast, while
// a server value does (the audit stamp casts the actor key id to uuid). The
// rest of class 22 stays a server fault: 500 with its ERROR line and its
// write_outcome. The 400 answer carries the database's message, never its
// DETAIL, which can quote the refused row.

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAdminFailureAnswersOnlyTheCallersDataAs400(t *testing.T) {
	const detail = "Failing row contains (i505-detail-secret)."
	for _, c := range []struct {
		code   string
		status int
	}{
		{"22001", http.StatusBadRequest},          // value too long for its column
		{"22003", http.StatusBadRequest},          // numeric value out of range
		{"22021", http.StatusBadRequest},          // NUL character in text
		{"22P05", http.StatusBadRequest},          // NUL character in JSON
		{"22P02", http.StatusInternalServerError}, // invalid text representation: a server value's cast
		{"22012", http.StatusInternalServerError}, // division by zero
		{"22000", http.StatusInternalServerError}, // data exception (pgvector's wrong dimension)
		{"22023", http.StatusInternalServerError}, // invalid parameter value
	} {
		t.Run(c.code, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			rec := httptest.NewRecorder()
			err := &pgconn.PgError{Severity: "ERROR", Code: c.code, Message: "i505 message of " + c.code, Detail: detail}
			writeAdminFailure(rec, httptest.NewRequest(http.MethodPost, "/api/manage", nil), "manage: i505-write failed", err, writeUnknown, "i505 write failed")
			if rec.Code != c.status {
				t.Fatalf("SQLSTATE %s answered %d, want %d", c.code, rec.Code, c.status)
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if strings.Contains(rec.Body.String(), "i505-detail-secret") {
				t.Errorf("SQLSTATE %s: the answer carries the database's DETAIL: %s", c.code, rec.Body.String())
			}
			lines := logs.withMsg("manage: i505-write failed")
			if c.status == http.StatusBadRequest {
				if len(lines) != 0 || len(logs.all()) != 0 {
					t.Errorf("SQLSTATE %s: lines %+v, want none for the caller's value", c.code, logs.all())
				}
				if msg, _ := body["error"].(string); !strings.Contains(msg, "i505 message of "+c.code) {
					t.Errorf("SQLSTATE %s: answer %v, want the database's message", c.code, body)
				}
				return
			}
			if len(lines) != 1 || lines[0].level != slog.LevelError || lines[0].attrs["write_outcome"] != writeUnknown {
				t.Errorf("SQLSTATE %s: lines %+v, want one ERROR with write_outcome=%s", c.code, logs.all(), writeUnknown)
			}
			if body["error"] != "i505 write failed" {
				t.Errorf("SQLSTATE %s: answer %v, want the route's text", c.code, body)
			}
		})
	}
}

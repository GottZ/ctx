package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// TestTypeStoreError565_ConfigChanged409: the store's lost-update refusal is a
// 409 on the shared error mapper of both transports.
func TestTypeStoreError565_ConfigChanged409(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBlockTypeStoreError(rec, "type-update", store.ErrBlockTypeConfigChanged, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "config changed") {
		t.Fatalf("ErrBlockTypeConfigChanged → %d %s, want 409 naming the change", rec.Code, rec.Body.String())
	}
}

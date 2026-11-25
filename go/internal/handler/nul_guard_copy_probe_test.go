// Issue #555, point 8 (a measurement): what the NUL guard allocates for a
// large JSON body, with and without the escape text `\u0000` in it (escaped
// itself, so the body carries no NUL — as a source file in a block would).
// The body is held once as chunks either way; a body that may carry the
// escape is joined into one more copy for the walk (joinChunks).
//
//	CTX_P555_NUL_PROBE=1 go test -short ./internal/handler/ -run TestNULGuardJoinCopyProbe -v
package handler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestNULGuardJoinCopyProbe(t *testing.T) {
	if os.Getenv("CTX_P555_NUL_PROBE") == "" {
		t.Skip("measurement: set CTX_P555_NUL_PROBE=1")
	}
	const size = 64 << 20
	esc := `\\` + `u0000` // a backslash, then the text u0000: the escape's text, no NUL
	build := func(withEscape bool) string {
		filler := strings.Repeat("x", size-64)
		if withEscape {
			filler = filler[:len(filler)/2] + esc + filler[len(filler)/2+len(esc):]
		}
		return `{"category":"c","title":"t","content":"` + filler + `"}`
	}
	guard := RefuseNUL(NULBodyJSON, 2*size)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	measure := func(body string) (alloc uint64, status int) {
		req := httptest.NewRequest(http.MethodPost, "/api/blob/store", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		old := debug.SetGCPercent(-1)
		defer debug.SetGCPercent(old)
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		guard.ServeHTTP(rec, req)
		runtime.ReadMemStats(&b)
		return b.TotalAlloc - a.TotalAlloc, rec.Code
	}
	for round := range 3 {
		plain, s1 := measure(build(false))
		escaped, s2 := measure(build(true))
		t.Logf("round %d: a %d MB JSON body: guard + replay allocated %.1f MB without the escape text (status %d), %.1f MB with it (status %d)",
			round, size>>20, float64(plain)/(1<<20), s1, float64(escaped)/(1<<20), s2)
		if escaped > plain+plain/4 {
			t.Errorf("a body that only quotes the escape cost %.1f MB against %.1f MB without it: copied once more", float64(escaped)/(1<<20), float64(plain)/(1<<20))
		}
	}
}

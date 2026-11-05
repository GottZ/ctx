package oai

import (
	"net/http/httptest"
	"testing"
)

// TestStreamWriterEndsOnce is L-1's writer half (Fremd-Review 2 of W02-08):
// a stream ends exactly once. After its error frame a second error frame and
// a [DONE] write nothing; after [DONE] an error frame writes nothing. Red:
// errorFrame or done without the streamEnded check (a second terminal frame
// on the wire).
func TestStreamWriterEndsOnce(t *testing.T) {
	const chunk = `{"x":1}`
	t.Run("error frame, then error frame and [DONE]", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s := newStreamWriter(rec, Settings{}, func(error) {})
		if err := s.frame([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if err := s.errorFrame([]byte(`{"error":{"code":"first"}}` + "\n")); err != nil {
			t.Fatal(err)
		}
		if err := s.errorFrame([]byte(`{"error":{"code":"second"}}`)); err != nil {
			t.Errorf("second error frame: %v, want a no-op", err)
		}
		if err := s.done(); err != nil {
			t.Errorf("[DONE] after the error frame: %v, want a no-op", err)
		}
		if got, want := rec.Body.String(), "data: "+chunk+"\n\ndata: {\"error\":{\"code\":\"first\"}}\n\n"; got != want {
			t.Errorf("wire %q, want %q", got, want)
		}
	})
	t.Run("[DONE], then error frame and [DONE]", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s := newStreamWriter(rec, Settings{}, func(error) {})
		if err := s.frame([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
		if err := s.done(); err != nil {
			t.Fatal(err)
		}
		if err := s.errorFrame([]byte(`{"error":{"code":"late"}}`)); err != nil {
			t.Errorf("error frame after [DONE]: %v, want a no-op", err)
		}
		if err := s.done(); err != nil {
			t.Errorf("second [DONE]: %v, want a no-op", err)
		}
		if got, want := rec.Body.String(), "data: "+chunk+"\n\n"+string(doneLine); got != want {
			t.Errorf("wire %q, want %q", got, want)
		}
	})
}

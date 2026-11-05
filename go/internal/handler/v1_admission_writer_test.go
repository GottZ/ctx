package handler

// Review 2 H-1 (A03-W8): the admission writer stays silent ONLY when the
// request's own context ended (the client is gone). Every other context
// error inside the acquire hull — the route's own deadline firing during a
// Continue (a continuation has no wait budget), a Continue on the run
// context of a lease the route already released — reaches a live client
// as a 503 with a body, never as an empty 200. Driven over real net/http
// (httptest.NewServer) against a real dispatcher; the control is a client
// that hangs up while First waits: nothing is written.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

// wireOutcome is what the server side of one probe saw.
type wireOutcome struct {
	reason string
	err    error
	wrote  bool
}

// writeWatch records whether the handler wrote a status or a byte.
type writeWatch struct {
	http.ResponseWriter
	mu    sync.Mutex
	wrote bool
}

func (w *writeWatch) WriteHeader(c int) {
	w.mu.Lock()
	w.wrote = true
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(c)
}

func (w *writeWatch) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.wrote = true
	w.mu.Unlock()
	return w.ResponseWriter.Write(p)
}

// wireServer serves one admission per request on the contract: Plan, the
// scripted doors on ctx (the route's context: the request's, narrowed by
// routeDeadline when > 0), on an error the writer with the REQUEST context.
func wireServer(t *testing.T, r *v1Rig, chain []backends.Backend, routeDeadline time.Duration, units, subBatch int,
	doors func(ctx context.Context, plan oai.Plan) error, out chan<- wireOutcome,
) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ww := &writeWatch{ResponseWriter: w}
		ca := urgentConsumer()
		req = req.WithContext(withConsumerAuth(req.Context(), ca))
		ctx := req.Context()
		if routeDeadline > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, routeDeadline)
			defer cancel()
		}
		plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: chain, Units: units, SubBatch: subBatch})
		if err == nil {
			err = doors(ctx, plan)
		}
		if err == nil {
			ww.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(ww, "ok")
			out <- wireOutcome{wrote: true}
			return
		}
		reason := admissionWrite(r, req, ww, err)
		ww.mu.Lock()
		wrote := ww.wrote
		ww.mu.Unlock()
		out <- wireOutcome{reason: reason, err: err, wrote: wrote}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// getWire sends one GET and returns status, headers and body.
func getWire(t *testing.T, ctx context.Context, url string) (*http.Response, []byte, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body, nil
}

// assertWireInternal: a 503 internal_error envelope with x-should-retry
// false — the answer to an A-internal fault (masterplan K5, K29).
func assertWireInternal(t *testing.T, resp *http.Response, body []byte) {
	t.Helper()
	var env struct {
		Error struct {
			Type string  `json:"type"`
			Code *string `json:"code"`
		} `json:"error"`
	}
	if resp.StatusCode != http.StatusServiceUnavailable || json.Unmarshal(body, &env) != nil || env.Error.Code == nil ||
		*env.Error.Code != oai.CodeInternalError || env.Error.Type != oai.TypeServiceUnavailable || resp.Header.Get("x-should-retry") != "false" {
		t.Fatalf("wire answer %d %q (x-should-retry %q), want 503 internal_error with a body", resp.StatusCode, body, resp.Header.Get("x-should-retry"))
	}
}

func TestWriterAnswersNonClientContextErrorsOnTheWire(t *testing.T) {
	row := v1Row("a", v1HostA)
	t.Run("route deadline during Continue", func(t *testing.T) {
		r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(2, 1, v1HostA)) // E = 1
		out := make(chan wireOutcome, 1)
		doors := func(ctx context.Context, plan oai.Plan) error {
			l, _, err := plan.Acquirer.First(ctx, &plan.Chain[0], "embed", time.Minute)
			if err != nil {
				return err
			}
			l.Release()
			occupy(t, r.disp.Dispatcher, v1HostA, 1) // block two never gets its unit
			l2, _, err := plan.Acquirer.Continue(ctx, &plan.Chain[0], "embed", time.Minute)
			if l2 != nil {
				l2.Release()
			}
			return err
		}
		srv := wireServer(t, r, []backends.Backend{row}, 300*time.Millisecond, 2, 1, doors, out)
		resp, body, err := getWire(t, context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		res := <-out
		if !errors.Is(res.err, context.DeadlineExceeded) {
			t.Fatalf("server error %v, want the route deadline inside the acquire hull", res.err)
		}
		assertWireInternal(t, resp, body)
		if res.reason != oai.CodeInternalError {
			t.Fatalf("reason %q, want internal_error", res.reason)
		}
	})
	t.Run("Continue on the released lease's run context", func(t *testing.T) {
		r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
		out := make(chan wireOutcome, 1)
		doors := func(ctx context.Context, plan oai.Plan) error {
			l, runCtx, err := plan.Acquirer.First(ctx, &plan.Chain[0], "embed", time.Minute)
			if err != nil {
				return err
			}
			l.Release() // cancels runCtx
			l2, _, err := plan.Acquirer.Continue(runCtx, &plan.Chain[0], "embed", time.Minute)
			if l2 != nil {
				l2.Release()
			}
			return err
		}
		srv := wireServer(t, r, []backends.Backend{row}, 0, 2, 1, doors, out)
		resp, body, err := getWire(t, context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		res := <-out
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("server error %v, want a canceled context inside the acquire hull", res.err)
		}
		assertWireInternal(t, resp, body)
	})
	t.Run("control: the client hangs up while First waits", func(t *testing.T) {
		r, row := fullTargetRig(t, 20*time.Second)
		out := make(chan wireOutcome, 1)
		doors := func(ctx context.Context, plan oai.Plan) error {
			l, _, err := plan.Acquirer.First(ctx, &plan.Chain[0], "chat", time.Minute)
			if l != nil {
				l.Release()
			}
			return err
		}
		srv := wireServer(t, r, []backends.Backend{row}, 0, 1, 0, doors, out)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if _, _, err := getWire(t, ctx, srv.URL); err == nil {
			t.Fatal("the client's request completed although it hung up")
		}
		select {
		case res := <-out:
			if res.reason != "" || res.wrote {
				t.Fatalf("client gone: reason %q, wrote %v — want nothing written", res.reason, res.wrote)
			}
			if !errors.Is(res.err, context.Canceled) {
				t.Fatalf("server error %v, want the request context's cancel", res.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the server never finished the abandoned request")
		}
	})
}

package handler

// Issue #453 point 1: net/http cancels a request's context with the same
// plain cancellation when the handler returns as when the client goes away
// (server.go: w.cancelCtx after ServeHTTP, conn.cancelCtx on a read or write
// error — both context.WithCancel functions, so context.Cause is
// context.Canceled either way). A goroutine that outlives its handler and
// reports a failure of its call on the request context is therefore no
// client abort when the client got its answer: Logger fixes its verdict when
// the handler returns, and every later question gets that verdict.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/types"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const lateLineMsg = "probe: late goroutine failed"

// lateLogger starts a goroutine that outlives the handler: it waits until
// the request context ends — which, after a return with the client still
// there, only net/http's own cancellation does — and then writes the error
// line of a call that failed on that context.
func lateLogger(r *http.Request, logged chan<- struct{}) {
	ctx := r.Context()
	go func() {
		defer close(logged)
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		logRequestError(ctx, lateLineMsg, "error", fmt.Errorf("store: probe: %w", ctx.Err()))
	}()
}

func waitLogged(t *testing.T, logged <-chan struct{}) {
	t.Helper()
	select {
	case <-logged:
	case <-time.After(15 * time.Second):
		t.Fatal("the late goroutine never wrote its line")
	}
}

// TestGoroutineAfterTheHandlerReturnedIsNoClientAbort — the client got its
// 200 in full; the goroutine's line about the cancellation net/http applied
// after the return is an ERROR (a call on a context that ended with the
// handler is a server defect), never INFO with client_gone=true.
func TestGoroutineAfterTheHandlerReturnedIsNoClientAbort(t *testing.T) {
	logs := installRejectLogCapture(t)
	logged := make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		lateLogger(r, logged)
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	}), nil)

	resp, err := http.Post(srv.URL+"/api/store", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"success":true`) {
		t.Fatalf("client: want the whole 200 answer, got %d %q %v", resp.StatusCode, body, err)
	}
	waitDone(t, done)
	waitLogged(t, logged)

	if got := statusOf(t, requestLogOf(t, logs)); got != http.StatusOK {
		t.Errorf("request log status = %d, want 200 (the client got its answer)", got)
	}
	recs := logs.withMsg(lateLineMsg)
	if len(recs) != 1 {
		t.Fatalf("want one late line, got %+v", logs.all())
	}
	if recs[0].level != slog.LevelError || recs[0].attrs["client_gone"] != nil {
		t.Errorf("late line after a return with the client present = level %v attrs %+v, want ERROR without client_gone", recs[0].level, recs[0].attrs)
	}
}

// TestGoroutineAfterAClientAbortStaysClientGone — the client left while the
// handler ran; a goroutine that reports the same cancellation after the
// return keeps the verdict of the request: INFO with client_gone=true, like
// the request log's 499.
func TestGoroutineAfterAClientAbortStaysClientGone(t *testing.T) {
	logs := installRejectLogCapture(t)
	logged := make(chan struct{})
	started := make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
		lateLogger(r, logged)
		w.WriteHeader(http.StatusInternalServerError)
	}), nil)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/store", strings.NewReader(`{}`))
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		errc <- err
	}()
	<-started
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("client: want its own cancellation, got %v", err)
	}
	waitDone(t, done)
	waitLogged(t, logged)

	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
	recs := logs.withMsg(lateLineMsg)
	if len(recs) != 1 || recs[0].level >= slog.LevelError || recs[0].attrs["client_gone"] != true {
		t.Errorf("late line after a client abort = %+v, want one INFO line with client_gone=true", recs)
	}
}

// goroutineErrorLineCensus: error-line helper calls that a goroutine reaches
// (a go statement, or a function literal handed to a Go or AfterFunc call,
// followed through the package's own functions and methods). Each such call
// asks clientGone after the handler may have returned; the verdict Logger
// fixes at the return answers it
// (TestGoroutineAfterTheHandlerReturnedIsNoClientAbort). A new one fails
// this test: name it here with the reason it runs on the request context,
// or on none.
var goroutineErrorLineCensus = map[string]int{}

// TestNoErrorLineHelperInAGoroutine — issue #453 point 1: the census of
// error-line helper calls inside goroutines. On the type-checked package
// (typedHandlerPackage) a call resolves exactly when it names a function or a
// method of this package (on a receiver of this package's types); a call
// through an interface or a function value is not followed.
func TestNoErrorLineHelperInAGoroutine(t *testing.T) {
	tp := typedHandlerPackage(t)
	fset, files, info, pkg := tp.fset, tp.files, tp.info, tp.pkg
	bodies := map[*types.Func]*ast.BlockStmt{}
	helpers := map[*types.Func]bool{}
	for _, file := range files {
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn, ok := info.Defs[fd.Name].(*types.Func)
			if !ok {
				continue
			}
			bodies[fn] = fd.Body
			if _, ok := errorLineHelpers[fd.Name.Name]; ok && fd.Recv == nil {
				helpers[fn] = true
			}
		}
	}
	if len(helpers) != len(errorLineHelpers) {
		t.Fatalf("found %d of the %d error-line helpers — did their names change?", len(helpers), len(errorLineHelpers))
	}
	callee := func(call *ast.CallExpr) *types.Func {
		if fn := tp.callee(call); fn != nil && fn.Pkg() == pkg {
			return fn
		}
		return nil
	}
	// reaches answers whether n calls a helper, directly or through the
	// package's functions; a function on the current path counts as not
	// reaching (a cycle adds nothing).
	memo := map[*types.Func]bool{}
	onPath := map[*types.Func]bool{}
	var reaches func(n ast.Node) bool
	reaches = func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if found || !ok {
				return !found
			}
			fn := callee(call)
			switch {
			case fn == nil || onPath[fn]:
			case helpers[fn]:
				found = true
			default:
				v, ok := memo[fn]
				if !ok && bodies[fn] != nil {
					onPath[fn] = true
					v = reaches(bodies[fn])
					delete(onPath, fn)
					memo[fn] = v
				}
				found = v
			}
			return !found
		})
		return found
	}
	got := map[string]int{}
	goroutines := 0
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			var target ast.Node
			switch g := n.(type) {
			case *ast.GoStmt:
				target = g.Call
			case *ast.CallExpr:
				// wg.Go(func(){…}) and context.AfterFunc(ctx, func(){…}) start
				// goroutines as well.
				if sel, ok := g.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Go" || sel.Sel.Name == "AfterFunc") && len(g.Args) > 0 {
					target = g.Args[len(g.Args)-1]
				}
			}
			if target == nil {
				return true
			}
			goroutines++
			if reaches(target) {
				pos := fset.Position(n.Pos())
				got[filepath.Base(pos.Filename)]++
				t.Logf("%s: a goroutine reaches an error-line helper", pos)
			}
			return true
		})
	}
	if goroutines == 0 || len(bodies) == 0 {
		t.Fatalf("saw %d goroutine starts and %d function bodies — did the walk break?", goroutines, len(bodies))
	}
	for _, f := range slices.Sorted(maps.Keys(got)) {
		if want := goroutineErrorLineCensus[f]; got[f] > want {
			t.Errorf("%s: %d goroutines reach an error-line helper, census allows %d — a goroutine that outlives its handler asks clientGone after the return (issue #453); name it in goroutineErrorLineCensus with its reason",
				f, got[f], want)
		}
	}
	for _, f := range slices.Sorted(maps.Keys(goroutineErrorLineCensus)) {
		if got[f] < goroutineErrorLineCensus[f] {
			t.Errorf("%s: %d goroutines reach an error-line helper, census says %d — lower the census", f, got[f], goroutineErrorLineCensus[f])
		}
	}
	t.Logf("checked %d goroutine starts over %d function bodies", goroutines, len(bodies))
}

// TestEncodeErrorThroughTheQueryKeepaliveIsClientGone — issue #453 point 5:
// a keepalive:true query whose client read the first keepalive byte and left;
// the answer, too large for the buffer, fails to reach it. writeJSON's line
// finds the request through the keepalive writer, which guards the writer
// under it and does not unwrap: INFO with client_gone=true, like the same
// write on Logger's writer.
func TestEncodeErrorThroughTheQueryKeepaliveIsClientGone(t *testing.T) {
	shrinkHeartbeat(t, 50*time.Millisecond, 2*time.Second)
	logs := installRejectLogCapture(t)
	answered := make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(answered)
		_, _ = io.Copy(io.Discard, r.Body)
		kw := armQueryKeepalive(r.Context(), w, true)
		defer kw.close()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		writeJSON(kw, http.StatusOK, map[string]any{"success": true, "answer": strings.Repeat("x", 4<<20)})
	}), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/query", strings.NewReader(`{"query":"q","keepalive":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, buf); err != nil || buf[0] != ' ' {
		t.Fatalf("first keepalive byte %q, %v", buf, err)
	}
	cancel()
	resp.Body.Close()
	<-answered
	waitDone(t, done)

	if errs := errorLines(logs); len(errs) != 0 {
		t.Errorf("a client that left a keepalive query produced ERROR lines: %+v", errs)
	}
	if recs := logs.withMsg("failed to encode JSON response"); len(recs) != 1 || recs[0].attrs["client_gone"] != true {
		t.Errorf("encode line = %+v, want one line with client_gone=true (the write to the gone client failed)", recs)
	}
	if got := statusOf(t, requestLogOf(t, logs)); got != clientClosedStatus {
		t.Errorf("request log status = %d, want %d", got, clientClosedStatus)
	}
}

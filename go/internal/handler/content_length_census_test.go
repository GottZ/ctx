package handler

// Issue #453 (found on the way): a response with a declared Content-Length
// that the client received in full before its handler returned is logged
// 499 when the client closes the connection right after reading — the
// request logger sees the request's context end before the handler is back
// (TestCompleteContentLengthAnswerThenWorkIs499 below). Writing the whole
// body does not tell the logger the client has it: net/http keeps the tail in
// its buffers until the handler returns, unless the handler flushes.
//
// No ctxd writer reaches it. Only two declare a body length — the camo image
// proxy (camo.go, HandleFetch) and the SPA's static files (web.go,
// serveContent through http.ServeContent) — and both write the body as the
// last thing they do and flush nothing, so the handler returns right after
// the write; writeJSON, the MCP SDK and the streams declare no length. This
// census holds that: a new function that declares a body length, flushes one,
// or works on after writing it fails here. Such a writer needs the request
// logger to know the body was delivered (declared length written and
// flushed) before it may keep working.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// lengthWriters are the functions allowed to declare a body length, by file.
var lengthWriters = map[string][]string{
	"camo.go": {"HandleFetch"},
	"web.go":  {"serveContent"},
}

func TestContentLengthWritersEndWithTheirWrite(t *testing.T) {
	var files []string
	for _, glob := range []string{"*.go", "../../web/*.go"} {
		m, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	fset := token.NewFileSet()
	found := map[string][]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		base := filepath.Base(f)
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !declaresLength(fd.Body) {
				continue
			}
			found[base] = append(found[base], fd.Name.Name)
			if !slices.Contains(lengthWriters[base], fd.Name.Name) {
				t.Errorf("%s: %s declares a body length — a client that reads it all and closes before the handler returns is logged 499; see this file's comment", base, fd.Name.Name)
				continue
			}
			if flushes(fd.Body) {
				t.Errorf("%s: %s flushes a declared-length body: the client may have it all while the handler still runs", base, fd.Name.Name)
			}
			last := fd.Body.List[len(fd.Body.List)-1]
			if !writesBody(last) {
				t.Errorf("%s: %s does work after writing its body (its last statement %s writes no body)", base, fd.Name.Name, fset.Position(last.Pos()))
			}
		}
	}
	for file, funcs := range lengthWriters {
		for _, fn := range funcs {
			if !slices.Contains(found[file], fn) {
				t.Errorf("%s: %s no longer declares a body length — remove it from lengthWriters", file, fn)
			}
		}
	}
}

// declaresLength: a Content-Length header set, or http.ServeContent /
// ServeFile / ServeFileFS (which set it).
func declaresLength(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "http" && slices.Contains([]string{"ServeContent", "ServeFile", "ServeFileFS"}, sel.Sel.Name) {
			found = true
		}
		if (sel.Sel.Name == "Set" || sel.Sel.Name == "Add") && len(call.Args) > 0 {
			if lit, ok := call.Args[0].(*ast.BasicLit); ok {
				if s, err := strconv.Unquote(lit.Value); err == nil && strings.EqualFold(s, "Content-Length") {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func flushes(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Flush" || sel.Sel.Name == "FlushError" || sel.Sel.Name == "NewResponseController") {
			found = true
		}
		return !found
	})
	return found
}

// writesBody: the statement writes the body (Write, WriteString, ServeContent).
func writesBody(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && slices.Contains([]string{"Write", "WriteString", "ServeContent", "ServeFile", "ServeFileFS"}, sel.Sel.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestCompleteContentLengthAnswerThenWorkIs499 — the behaviour the census
// keeps out of reach: a handler writes and flushes a complete Content-Length
// answer, then keeps working; the client reads it all and closes. The request
// line says 499 although the client got its 200 — the logger cannot tell.
func TestCompleteContentLengthAnswerThenWorkIs499(t *testing.T) {
	logs := installRejectLogCapture(t)
	ended := make(chan struct{})
	srv, done := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"success":true}`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		_ = http.NewResponseController(w).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		close(ended)
	}), nil)
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	<-ended
	waitDone(t, done)
	if resp.StatusCode != http.StatusOK || string(got) != `{"success":true}` {
		t.Fatalf("fixture: the client got %d %q, want the whole 200", resp.StatusCode, got)
	}
	line := requestLogOf(t, logs)
	if statusOf(t, line) != clientClosedStatus || line.attrs["handler_status"] != int64(http.StatusOK) {
		t.Errorf("request line %+v — the rule changed: if a delivered answer is no longer 499, update this pin and the census above", line.attrs)
	}
}

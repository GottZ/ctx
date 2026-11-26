//go:build integration

// Issue #565, review round 2 (R2-4): the size cap of a type config is one
// measure on the write paths and on Reload.
package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// TestTypeWrite565_SizeCapOneMeasure: a config at the cap — 16 384 bytes as
// the database stores it, written compact — is accepted by both create and both
// update transports AND loads on the next Reload; one byte more is refused 422.
// A number the database writes longer than the request (1e-5 is stored as
// 0.00001) counts at its stored length: three bytes short of the cap as sent is
// at the cap as stored and loads, two bytes short is over it and is refused.
//
// RED on 9c701044: every write at the cap answered 200 and the Reload after it
// failed (`config exceeds 16384 bytes`) — the write measured the request's
// length, Reload the jsonb text with a blank after every ',' and ':'; and the
// exponent case was accepted at a stored size over the cap.
func TestTypeWrite565_SizeCapOneMeasure(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	ctx := context.Background()
	mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	creates := []struct {
		label string
		send  func(t *testing.T, name, cfg string) *httptest.ResponseRecorder
	}{
		{"PUT /api/types (create)", func(t *testing.T, name, cfg string) *httptest.ResponseRecorder {
			return c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/"+name, `{"config":`+cfg+`}`)
		}},
		{"manage type-create", func(t *testing.T, name, cfg string) *httptest.ResponseRecorder {
			body := fmt.Sprintf(`{"action":"type-create","data":{"name":%q,"config":%s}}`, name, cfg)
			req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(context.WithValue(req.Context(), authResultKey, serverAdmin))
			rec := httptest.NewRecorder()
			mh.HandleManage(rec, req)
			return rec
		}},
	}
	const damped = `"retrieval":{"policy":"damped","damping_factor":1e-5},`
	drop := func(t *testing.T, name string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `DELETE FROM context_block_types WHERE name = $1`, name); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, label string, rec *httptest.ResponseRecorder, accept bool) {
		t.Helper()
		if !accept {
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "exceeds 16384 bytes") {
				t.Fatalf("%s: %d %.200s — want 422 naming the cap", label, rec.Code, rec.Body.String())
			}
			return
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %.300s — want 200", label, rec.Code, rec.Body.String())
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("%s: accepted, and the Reload after it fails: %v", label, err)
		}
	}
	for i, c := range creates {
		for _, tc := range []struct {
			label  string
			cfg    string
			accept bool
		}{
			{"at the cap", t565Sized(t, 16384, ""), true},
			{"one byte over", t565Sized(t, 16385, ""), false},
			{"1e-5, at the cap as stored", t565Sized(t, 16384-3, damped), true},
			{"1e-5, one byte over as stored", t565Sized(t, 16384-2, damped), false},
		} {
			name := fmt.Sprintf("size565c%d", i)
			t.Run(c.label+" "+tc.label, func(t *testing.T) {
				defer drop(t, name)
				check(t, c.label+" "+tc.label, c.send(t, name, tc.cfg), tc.accept)
			})
		}
	}
	for _, tr := range t565Transports(pool, reg) {
		for _, tc := range []struct {
			label  string
			n      int
			accept bool
		}{{"at the cap", 16384, true}, {"one byte over", 16385, false}} {
			if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "size565u", Scope: store.GlobalScope,
				Config: []byte(`{"v":1,"classify":{"title_patterns":["a"]}}`)}, nil, ""); err != nil {
				t.Fatal(err)
			}
			t.Run(tr.label+" update "+tc.label, func(t *testing.T) {
				defer drop(t, "size565u")
				check(t, tr.label+" update "+tc.label, tr.send(t, serverAdmin, "size565u", t565Sized(t, tc.n, "")), tc.accept)
			})
		}
	}
}

// t565Sized is a valid operator config of exactly n bytes, compact as sent:
// prefix (a section and its comma, or "") and two classify arrays of
// 128-character entries, the last one cut to fit.
func t565Sized(t *testing.T, n int, prefix string) string {
	t.Helper()
	var tp, sp []string
	render := func() string {
		s := `{"v":1,` + prefix + `"classify":{"title_patterns":["` + strings.Join(tp, `","`) + `"]`
		if len(sp) > 0 {
			s += `,"source_prefixes":["` + strings.Join(sp, `","`) + `"]`
		}
		return s + `}}`
	}
	for i := 0; i < 64; i++ {
		tp = append(tp, fmt.Sprintf("t%02d", i)+strings.Repeat("x", 125))
	}
	for len(render()) < n {
		sp = append(sp, fmt.Sprintf("s%02d", len(sp))+strings.Repeat("y", 125))
	}
	last := sp[len(sp)-1]
	sp[len(sp)-1] = last[:len(last)-(len(render())-n)]
	if s := render(); len(s) != n {
		t.Fatalf("t565Sized(%d): %d bytes", n, len(s))
	}
	return render()
}

package pgxdb

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestNoteLockTimeoutMarksOnlyItsStatements — the pool notes a connection
// that ran a statement naming lock_timeout (a SET LOCAL, a set_config, a
// migration file) and no other; a closed connection leaves the marks.
func TestNoteLockTimeoutMarksOnlyItsStatements(t *testing.T) {
	a, b := &pgx.Conn{}, &pgx.Conn{}
	t.Cleanup(func() { ForgetLockTimeout(a); ForgetLockTimeout(b) })
	noteLockTimeout(a, `SELECT id FROM context_blocks WHERE id = $1`)
	noteLockTimeout(b, `SET LOCAL lock_timeout = '5s'`)
	noteLockTimeout(nil, `SET lock_timeout = 1`)
	if _, ok := lockTimeoutConns.Load(a); ok {
		t.Error("a connection that set no lock_timeout is marked")
	}
	if _, ok := lockTimeoutConns.Load(b); !ok {
		t.Error("a connection that set lock_timeout is not marked")
	}
	ForgetLockTimeout(b)
	if _, ok := lockTimeoutConns.Load(b); ok {
		t.Error("a closed connection keeps its mark")
	}
}

// BenchmarkNoteLockTimeout is what every statement of the pool pays: one
// substring search over its text (here 2 KB, a long query of the house).
func BenchmarkNoteLockTimeout(b *testing.B) {
	conn := &pgx.Conn{}
	sql := strings.Repeat("SELECT cb.id, cb.title FROM context_blocks cb WHERE cb.scope = ANY($1) AND ", 28)
	b.ReportAllocs()
	b.SetBytes(int64(len(sql)))
	for b.Loop() {
		noteLockTimeout(conn, sql)
	}
}

// BenchmarkPrepareUnmarked is what every acquire of a connection that set no
// lock_timeout pays: one lookup.
func BenchmarkPrepareUnmarked(b *testing.B) {
	conn := &pgx.Conn{}
	b.ReportAllocs()
	for b.Loop() {
		if ok, err := PrepareAfterLockTimeout(b.Context(), conn); !ok || err != nil {
			b.Fatal(ok, err)
		}
	}
}

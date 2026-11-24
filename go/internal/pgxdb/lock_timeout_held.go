package pgxdb

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A lock timer that fires at the grant of a lock wait at a commit — the
// notify lock a transaction takes that ran a NOTIFY, a deferred constraint's
// check — leaves its 55P03 held: no interrupt check follows that wait, the
// COMMIT answers success, and the backend raises the held error with the
// connection's next command (issue #519, executed with the pool in
// TestLockTimeoutHeldPastCommitOnThePool). On a pool that is the statement of
// whoever acquires the connection next, unrelated to the transaction. ctx
// sets lock_timeout on its pool's connections in a few places (the
// embedding cutover's transactions, the migration files, the migration
// runner's lock wait); the pool notes every connection that ran a statement
// naming lock_timeout and, before it hands that connection out again, sends
// it one empty statement that takes a held 55P03 (PrepareAfterLockTimeout).
// A connection that never set one pays a map lookup per acquire. The mark is
// per hand-out and needs the name in the statement's text: a session-wide SET
// (not SET LOCAL) would be covered at the first return of its connection
// only, and a lock_timeout from the role, the database or a function's SET
// clause not at all. ctx sets none of those — every lock_timeout it sets is
// transaction-local (SET LOCAL, set_config with is_local) in a statement whose
// text names it (review of PR #553).

// lockTimeoutConns holds the connections of the pool that ran a statement
// naming lock_timeout since they were last handed out.
var lockTimeoutConns sync.Map // *pgx.Conn → struct{}

// noteLockTimeout marks conn when sql names lock_timeout (a SET, a SET LOCAL,
// a set_config, a migration file that sets it).
func noteLockTimeout(conn *pgx.Conn, sql string) {
	if conn != nil && strings.Contains(sql, "lock_timeout") {
		lockTimeoutConns.Store(conn, struct{}{})
	}
}

// PrepareAfterLockTimeout is the pool's PrepareConn (store.NewPool): a
// connection that ran a statement naming lock_timeout gets one empty
// statement before it is handed out, which takes a 55P03 a lock timer left
// held — the next acquirer's statement then runs on a clean connection. A
// held 55P03 is absorbed and the connection kept; any other failure destroys
// the connection (false), and the pool hands out another.
func PrepareAfterLockTimeout(ctx context.Context, conn *pgx.Conn) (bool, error) {
	if _, marked := lockTimeoutConns.LoadAndDelete(conn); !marked {
		return true, nil
	}
	_, err := conn.Exec(ctx, ";")
	var pgErr *pgconn.PgError
	if err == nil || (errors.As(err, &pgErr) && pgErr.Code == "55P03") {
		return true, nil
	}
	return false, nil
}

// ForgetLockTimeout is the pool's BeforeClose: a closed connection leaves
// the marks.
func ForgetLockTimeout(conn *pgx.Conn) {
	lockTimeoutConns.Delete(conn)
}

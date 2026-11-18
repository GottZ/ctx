//go:build integration

package testdb

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// clientToolScript runs one Postgres client program of the container's own
// installation against one database over the container's loopback, with the
// credentials of runPostgres. $1 is the program, $2 the database, the rest
// its arguments. The version directory is appended to PATH for images that
// keep the client programs out of it.
const clientToolScript = `export PGPASSWORD=ctxtest PATH="$PATH:/usr/lib/postgresql/18/bin"; ` +
	`prog="$1"; db="$2"; shift 2; exec "$prog" -h 127.0.0.1 -U ctxtest -d "$db" "$@"`

// RunClientTool runs a Postgres client program (pg_dump, pg_restore, psql)
// inside the shared test container against the database of dsn (a DSN from
// SetupTestDBWithDSN) and returns its exit code and combined output. The
// program runs next to the server, so its version is the server's: a host
// pg_dump older than the server refuses to dump it, and a CI runner carries
// none of the server's version. Files the program writes stay inside the
// container, where a later call reads them.
func RunClientTool(t *testing.T, dsn, program string, args ...string) (int, string) {
	t.Helper()
	ensureShared(t)
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("testdb: parse dsn: %v", err)
	}
	db := strings.TrimPrefix(u.Path, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := append([]string{"sh", "-c", clientToolScript, "sh", program, db}, args...)
	code, r, err := shared.container.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("testdb: exec %s: %v", program, err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("testdb: read %s output: %v", program, err)
	}
	return code, string(out)
}

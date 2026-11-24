//go:build integration

package config_test

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestDSNConnectsThroughEveryHostSpelling (issue #519, review of PR #553):
// the DSN of each host spelling that names the loopback reaches the test
// database — the bracketed IPv6 spelling, the only one an IPv6 host connected
// with before #519, included. A spelling of an address family the host does
// not route to the container (no IPv6 loopback) is skipped, not judged.
func TestDSNConnectsThroughEveryHostSpelling(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	cc := pool.Config().ConnConfig
	t.Setenv("PGCONNECT_TIMEOUT", "")
	port := strconv.Itoa(int(cc.Port))
	for _, tc := range []struct{ host, dial string }{
		{"localhost", "localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"[::1]", "::1"},
		{"::1", "::1"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			if c, err := net.DialTimeout("tcp", net.JoinHostPort(tc.dial, port), 2*time.Second); err != nil {
				t.Skipf("the container is not reachable on %s: %v", tc.dial, err)
			} else {
				_ = c.Close()
			}
			cfg := &config.Config{Server: config.ServerConfig{DBUser: cc.User, DBPass: cc.Password, DBHost: tc.host, DBPort: int(cc.Port), DB: cc.Database, DBSSL: "disable"}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := pgx.Connect(ctx, cfg.DSN())
			if err != nil {
				t.Fatalf("CONTEXT_DB_HOST %q: %v", tc.host, err)
			}
			defer func() { _ = conn.Close(context.Background()) }()
			var db string
			if err := conn.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil || db != cc.Database {
				t.Errorf("CONTEXT_DB_HOST %q: connected to %q (%v), want %q", tc.host, db, err, cc.Database)
			}
		})
	}
}

package config

// Issue #519: the DSN ctxd builds its database pool and its listener
// connection from sets a connect timeout, so a connection attempt to a
// database that accepts and never answers ends instead of holding a pool slot
// for good. An operator's PGCONNECT_TIMEOUT (libpq's variable, which pgx reads
// as well) is kept — also 0, which means wait without end.

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDSNSetsAConnectTimeoutUnlessTheOperatorDid(t *testing.T) {
	cfg := &Config{Server: ServerConfig{DBUser: "u", DBPass: "p", DBHost: "db.example", DBPort: 5432, DB: "ctx", DBSSL: "disable"}}
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", 10 * time.Second},
		{"3", 3 * time.Second},
		{"0", 0},
	} {
		t.Run("PGCONNECT_TIMEOUT="+tc.env, func(t *testing.T) {
			t.Setenv("PGCONNECT_TIMEOUT", tc.env)
			pc, err := pgconn.ParseConfig(cfg.DSN())
			if err != nil {
				t.Fatalf("DSN %q: %v", cfg.DSN(), err)
			}
			if pc.ConnectTimeout != tc.want {
				t.Errorf("DSN %q: connect timeout %v, want %v", cfg.DSN(), pc.ConnectTimeout, tc.want)
			}
		})
	}
}

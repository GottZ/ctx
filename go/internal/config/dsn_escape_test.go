package config

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestDSNCarriesEveryValueAsItIs (issue #519, the review of #530 point 7):
// the DSN put the user and the password through the query string's escape
// (a space became "+", read back as a plus), the database name and the host
// through none (a "?" in the name cut the query and lost sslmode, a space or
// a bare IPv6 address made the DSN unparsable). Each value now goes into the
// part of the URL it belongs to, with that part's escape, and pgx reads back
// what the operator set.
func TestDSNCarriesEveryValueAsItIs(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	for _, tc := range []struct {
		name string
		srv  ServerConfig
	}{
		{"a password with a space", ServerConfig{DBUser: "u", DBPass: "p ss", DBHost: "db.example", DBPort: 5432, DB: "ctx", DBSSL: "require"}},
		{"a user and password with url delimiters", ServerConfig{DBUser: "user@x", DBPass: "p@ss:w/0rd?#%", DBHost: "db.example", DBPort: 5433, DB: "ctx", DBSSL: "require"}},
		{"a database name with a question mark", ServerConfig{DBUser: "u", DBPass: "p", DBHost: "db.example", DBPort: 5432, DB: "ctx?x", DBSSL: "require"}},
		{"a database name with a space", ServerConfig{DBUser: "u", DBPass: "p", DBHost: "db.example", DBPort: 5432, DB: "my ctx", DBSSL: "require"}},
		{"a bare IPv6 host", ServerConfig{DBUser: "u", DBPass: "p", DBHost: "fd00::5", DBPort: 5432, DB: "ctx", DBSSL: "disable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Server: tc.srv}
			pc, err := pgconn.ParseConfig(cfg.DSN())
			if err != nil {
				t.Fatalf("DSN %q: %v", cfg.DSN(), err)
			}
			if pc.User != tc.srv.DBUser || pc.Password != tc.srv.DBPass || pc.Host != tc.srv.DBHost ||
				int(pc.Port) != tc.srv.DBPort || pc.Database != tc.srv.DB {
				t.Errorf("DSN %q reads back user %q password %q host %q port %d database %q", cfg.DSN(), pc.User, pc.Password, pc.Host, pc.Port, pc.Database)
			}
			if tc.srv.DBSSL == "require" && pc.TLSConfig == nil {
				t.Errorf("DSN %q: sslmode require was lost", cfg.DSN())
			}
		})
	}
}

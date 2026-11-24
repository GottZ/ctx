package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// dsnReads is what pgx reads from a DSN: each host with its port (the first,
// then the fallbacks), the database, the user and the password.
func dsnReads(t *testing.T, dsn string) string {
	t.Helper()
	pc, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "error: " + err.Error()
	}
	hosts := []string{fmt.Sprintf("%s:%d", pc.Host, pc.Port)}
	for _, f := range pc.Fallbacks {
		if h := fmt.Sprintf("%s:%d", f.Host, f.Port); h != hosts[len(hosts)-1] {
			hosts = append(hosts, h)
		}
	}
	return fmt.Sprintf("hosts %s database %q user %q password %q", strings.Join(hosts, ","), pc.Database, pc.User, pc.Password)
}

// TestDSNReadsEveryHostForm (issue #519, review of PR #553): DSN joined the
// host and port with net.JoinHostPort, which puts brackets around any host
// with a colon — also around a host already in brackets: CONTEXT_DB_HOST
// "[fd00::5]" became "[[fd00::5]]:5432" and pgx refused the DSN, so ctxd did
// not start. The bracketed spelling was the ONLY one an IPv6 host connected
// with before (the bare one was read as an address with the port in it). The
// table is every host spelling with what the DSN before #519 made pgx read
// (base) where that connected, and what it reads now; a spelling the base
// could not connect with reads the address it names.
func TestDSNReadsEveryHostForm(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	t.Setenv("PGHOST", "")
	t.Setenv("PGPORT", "")
	for _, tc := range []struct {
		host, want string
		base       bool // the base connected with it, and read want
	}{
		{"db.example", "hosts db.example:5432", true},
		{"10.0.0.5", "hosts 10.0.0.5:5432", true},
		{"localhost", "hosts localhost:5432", true},
		{"[fd00::5]", "hosts fd00::5:5432", true},
		{"[::1]", "hosts ::1:5432", true},
		{"[fe80::1%25eth0]", "hosts fe80::1%eth0:5432", true}, // the zone as the URL spells it
		{"h1,h2", "hosts h1:5432,h2:5432", true},
		{"[fd00::5],[fd00::6]", "hosts fd00::5:5432,fd00::6:5432", true},
		// A list is read host by host (second review of PR #553): a bracketed
		// host after a name put the whole list in brackets, and a host with
		// its own port was read as a host name with a colon in it.
		{"h1,[fd00::5]", "hosts h1:5432,fd00::5:5432", true},
		{"localhost,[::1]", "hosts localhost:5432,::1:5432", true},
		{"h1,[fd00::5],h3", "hosts h1:5432,fd00::5:5432,h3:5432", true},
		{"h1:5433,h2", "hosts h1:5433,h2:5432", true},
		{"h1,fd00::5", "hosts h1:5432,fd00::5:5432", false},
		// The base refused these, or read another address.
		{"fd00::5", "hosts fd00::5:5432", false},
		{"::1", "hosts ::1:5432", false},
		{"fe80::1%eth0", "hosts fe80::1%eth0:5432", false},
		{"[fe80::1%eth0]", "hosts fe80::1%eth0:5432", false},
		{"/var/run/postgresql", "hosts /var/run/postgresql:5432", false},
	} {
		cfg := &Config{Server: ServerConfig{DBUser: "u", DBPass: "p", DBHost: tc.host, DBPort: 5432, DB: "ctx", DBSSL: "disable"}}
		want := tc.want + ` database "ctx" user "u" password "p"`
		if got := dsnReads(t, cfg.DSN()); got != want {
			t.Errorf("CONTEXT_DB_HOST %q (base connected: %v): DSN %q reads %s, want %s", tc.host, tc.base, cfg.DSN(), got, want)
		}
	}
}

// TestDSNDatabaseNameAsWritten (issue #519, review of PR #553): the DSN
// before #519 carried the database name unescaped, so a pre-encoded name
// ("my%20db") connected to the decoded one ("my db"). The name now reaches
// the database as written; V73 warns at boot with both readings, so an
// operator who encoded it by hand reads why the database is not found.
func TestDSNDatabaseNameAsWritten(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "")
	cfg := Defaults()
	cfg.Server.DB = "my%20db"
	if got := dsnReads(t, cfg.DSN()); !strings.Contains(got, `database "my%20db"`) {
		t.Errorf("DSN %q reads %s, want the name as written", cfg.DSN(), got)
	}
	var warned []Issue
	for _, is := range Validate(cfg) {
		if is.Field == "server.db" {
			warned = append(warned, is)
		}
	}
	if len(warned) != 1 || warned[0].Severity != SeverityWarn || !strings.Contains(warned[0].Msg, `"my db"`) || !strings.Contains(warned[0].Msg, `"my%20db"`) {
		t.Errorf("want one V73 warning naming both readings, got %+v", warned)
	}
}

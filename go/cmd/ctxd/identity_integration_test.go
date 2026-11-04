//go:build integration

// Gates of F-W2 (design/04 §4.2) in the boot and the production router:
//
//   - TestBootRefusesWithoutInstanceIdentity: the boot step
//     bootInstanceIdentity, re-executed in a child process against a real
//     database, exits 1 with "federation: instance identity missing — run
//     migrations" when the row is deleted, when the secret has 16 bytes (the
//     length CHECK of 163 dropped and its row guard disabled by the test —
//     the overlay of this gate) and
//     on a database at 162 (no table); on an intact row it returns and logs
//     the instance id. The child logs through the daemon's own handler
//     (daemonLogger, JSON), and no output of any case carries a rendering of
//     the secret. Red against the boot before F-W2, which loads no identity at all
//     (bootInstanceIdentity does not exist: compile error).
//   - TestRouterCarriesItsInstanceIdentity: two production routers in one
//     process, built on contexts carrying two identities — a handler on each
//     reads its own router's identity through fedhop.IdentityFrom; the first
//     root middleware is fedhop.IdentityMiddleware, the second
//     handler.SecurityHeaders.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestBootRefusesWithoutInstanceIdentity|TestRouterCarriesItsInstanceIdentity' ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	identityBootHelperEnv    = "CTX_TEST_IDENTITY_BOOT_HELPER"
	identityBootHelperDSNEnv = "CTX_TEST_IDENTITY_BOOT_DSN"
)

// TestInstanceIdentityBootHelperProcess is not a test: it is the child body
// of TestBootRefusesWithoutInstanceIdentity (the idiom of
// TestSchemaContractBootHelperProcess). It runs exactly the boot step whose
// os.Exit(1) is under test.
func TestInstanceIdentityBootHelperProcess(t *testing.T) {
	if os.Getenv(identityBootHelperEnv) == "" {
		t.Skip("helper process body — only meaningful when spawned by the identity-boot probe")
	}
	ctx := context.Background()
	pool, err := store.NewPool(ctx, os.Getenv(identityBootHelperDSNEnv))
	if err != nil {
		fmt.Fprintf(os.Stderr, "identity boot helper: pool: %v\n", err)
		os.Exit(9)
	}
	defer pool.Close()
	slog.SetDefault(daemonLogger(os.Stderr))
	ident := bootInstanceIdentity(ctx, pool)
	fmt.Printf("boot returned: %v\n", ident)
	os.Exit(0)
}

func runIdentityBootHelper(t *testing.T, dsn string) (output string, exitCode int) {
	t.Helper()
	env := filterEnv(os.Environ(), identityBootHelperEnv, identityBootHelperDSNEnv)
	env = append(env, identityBootHelperEnv+"=1", identityBootHelperDSNEnv+"="+dsn)
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceIdentityBootHelperProcess$") //nolint:gosec // re-exec of the own test binary
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return out.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.String(), exitErr.ExitCode()
	}
	t.Fatalf("running identity-boot helper: %v\noutput:\n%s", err, out.String())
	return "", -1
}

// identitySecretLeaks returns the renderings of secret found in out: hex,
// base64 (JSON's []byte form), fmt's byte-slice forms, the raw bytes and Go's
// quoted string form (slog's text form of a []byte).
func identitySecretLeaks(out string, secret []byte) []string {
	if len(secret) < 8 {
		return nil
	}
	h := hex.EncodeToString(secret)
	quoted := strconv.Quote(string(secret))
	var hits []string
	for _, r := range []string{h, strings.ToUpper(h), h[:16],
		base64.StdEncoding.EncodeToString(secret), base64.RawStdEncoding.EncodeToString(secret),
		base64.URLEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret),
		strings.Trim(fmt.Sprintf("%d", secret), "[]"), strings.Trim(fmt.Sprintf("%#v", secret), "[]byte{}"),
		fmt.Sprintf("% x", secret[:8]), string(secret[:8]), quoted[1 : len(quoted)/2]} {
		if strings.Contains(out, r) {
			hits = append(hits, r)
		}
	}
	return hits
}

func TestBootRefusesWithoutInstanceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	read := func(t *testing.T, pool *pgxpool.Pool) (id string, secret []byte) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT instance_id::text, instance_secret FROM context_instance`).Scan(&id, &secret); err != nil {
			t.Fatalf("read context_instance: %v", err)
		}
		return id, secret
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) (dsn, id string, secret []byte)
		abort bool
	}{
		{"intact", func(t *testing.T) (string, string, []byte) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			id, secret := read(t, pool)
			return dsn, id, secret
		}, false},
		{"row_deleted", func(t *testing.T) (string, string, []byte) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			id, secret := read(t, pool)
			if _, err := pool.Exec(ctx, `DELETE FROM context_instance`); err != nil {
				t.Fatalf("delete: %v", err)
			}
			return dsn, id, secret
		}, true},
		{"secret_16_bytes", func(t *testing.T) (string, string, []byte) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			if _, err := pool.Exec(ctx, `ALTER TABLE context_instance DROP CONSTRAINT ck_instance_secret_length;
			                             ALTER TABLE context_instance DISABLE TRIGGER trg_instance_guard;
			                             UPDATE context_instance SET instance_secret = gen_random_bytes(16)`); err != nil {
				t.Fatalf("overlay: %v", err)
			}
			id, secret := read(t, pool)
			return dsn, id, secret
		}, true},
		{"no_table_at_162", func(t *testing.T) (string, string, []byte) {
			_, dsn := testdb.SetupTestDBUpToWithDSN(t, 162)
			return dsn, "", nil
		}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dsn, id, secret := c.setup(t)
			out, code := runIdentityBootHelper(t, dsn)
			t.Logf("child exit %d, output:\n%s", code, out)
			const abort = "federation: instance identity missing — run migrations"
			switch {
			case c.abort && (code != 1 || !strings.Contains(out, abort) || strings.Contains(out, "boot returned")):
				t.Errorf("exit %d, output:\n%s\nwant exit 1 with %q before the boot returns", code, out, abort)
			case !c.abort && (code != 0 || !strings.Contains(out, "boot returned: "+id) || strings.Contains(out, abort)):
				t.Errorf("exit %d, output:\n%s\nwant exit 0 and the boot returning identity %s", code, out, id)
			}
			if hits := identitySecretLeaks(out, secret); len(hits) > 0 {
				t.Errorf("boot output carries the secret %v:\n%s", hits, out)
			}
		})
	}
}

func TestRouterCarriesItsInstanceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	backendPool := backends.NewPool(pool, nil)
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)

	a := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000a1", bytes.Repeat([]byte{0xa1}, 32))
	b := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000b2", bytes.Repeat([]byte{0xb2}, 32))
	// Both routers exist BEFORE either serves (review 2 FR2-1): a NewRouter
	// that set the process default and mounted a zero identity would pass a
	// build-then-serve loop, because the last-built default happens to be the
	// one asked for; serving A, B, A on routers built together does not.
	routers := map[string]http.Handler{}
	for _, ident := range []fedhop.Identity{a, b} {
		router := NewRouter(fedhop.WithIdentity(ctx, ident), pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d)
		mws := router.Middlewares()
		name := func(i int) string {
			if i >= len(mws) {
				return "<none>"
			}
			return runtime.FuncForPC(reflect.ValueOf(mws[i]).Pointer()).Name()
		}
		if !strings.HasPrefix(name(0), "github.com/GottZ/ctx/internal/fedhop.IdentityMiddleware") ||
			name(1) != "github.com/GottZ/ctx/internal/handler.SecurityHeaders" {
			t.Errorf("root middlewares start with %s, %s — want fedhop.IdentityMiddleware, then handler.SecurityHeaders", name(0), name(1))
		}
		router.Get("/zz-f-w2-identity", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, fedhop.IdentityFrom(r.Context()).ID)
		})
		routers[ident.ID] = router
	}
	for _, ident := range []fedhop.Identity{a, b, a} {
		rec := httptest.NewRecorder()
		routers[ident.ID].ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/zz-f-w2-identity", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != ident.ID {
			t.Errorf("router of %s: status %d, handler read %q — every router must carry its own identity, not the last one built", ident.ID, rec.Code, rec.Body.String())
		}
	}
}

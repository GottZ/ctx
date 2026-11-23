//go:build integration

package handler

// Issue #504: the lines /health writes per minute of the compose healthcheck
// (one request every 30 s) in the three states a server spends its time in —
// not seeded, seeded and healthy, one backend down for good — and the answer
// /health gives in each, which this issue does not change. A real database,
// the pool seeded by `ctx backends seed` through the production surfaces
// (seedHarness), the real handler.
//
//	go test -tags=integration ./internal/handler/ -run 'TestHealthLinesPerMinute|TestHealthLogsTheDatabaseOncePerChange' -count=1 -v

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/provenance"
	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

// healthCheckMinute is the compose healthcheck's cadence: two requests a
// minute (docker-compose.yml, interval 30s).
const healthCheckMinute = 2

// healthOperatorLines counts the /health records an operator reads (INFO and
// above — ctxd logs at INFO) in logs.
func healthOperatorLines(logs *rejectLogCapture) int {
	n := 0
	for _, r := range logs.all() {
		if r.level >= slog.LevelInfo && strings.HasPrefix(r.msg, "health check") {
			n++
		}
	}
	return n
}

func TestHealthLinesPerMinute(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, false)
	st := &swapStore{}
	st.p.Store(healthTestConfig()) // Dream on, as a new installation has it
	h := NewHealthHandler(sh.pool, st, sh.bp, nil)

	embed := slowBackend(t, 0, nil)
	var chatDown atomic.Bool
	chat := flakyBackend(t, &chatDown)

	// A known state first: the database answers and every role is served (the
	// state is process-wide).
	HealthStatus(context.Background(), sh.pool, seededBackends(operatorRow("i504-minute-base", embed, i504Roles...)), true, "ok")

	contract, has := schemacontract.LatestReport()
	contractValue := schemaContractHealthValue(contract, has)
	served := "ok"
	if contractValue == "attention" {
		served = "degraded"
	}

	const minutes = 10
	type phase struct {
		name     string
		prepare  func(t *testing.T)
		code     int
		status   string
		services map[string]string
		want     []int // operator lines in minute 1, 2, …; the rest 0
	}
	phases := []phase{
		{
			name: "not seeded", prepare: func(*testing.T) {},
			code: http.StatusServiceUnavailable, status: "unhealthy",
			services: map[string]string{"database": "ok", "embed": "error", "chat": "error", "dream": "error"},
			want:     []int{3}, // one WARN per role
		},
		{
			name: "seeded and healthy",
			prepare: func(t *testing.T) {
				spec := `{"chat":{"host":"` + chat + `","model":"i504-chat"},"embed":{"host":"` + embed + `","model":"i504-embed"}}`
				if out, err := sh.seed("--file", sh.specFile(spec)); err != nil {
					t.Fatalf("seed: %v (%s)", err, out)
				}
				if err := sh.bp.Reload(sh.ctx); err != nil {
					t.Fatalf("pool reload: %v", err)
				}
			},
			code: http.StatusOK, status: served,
			services: map[string]string{"database": "ok", "embed": "ok", "chat": "ok", "dream": "ok"},
			want:     []int{3}, // one INFO per role: served again
		},
		{
			name:    "one backend down for good",
			prepare: func(*testing.T) { chatDown.Store(true) },
			code:    http.StatusOK, status: "degraded",
			services: map[string]string{"database": "ok", "embed": "ok", "chat": "error", "dream": "error"},
			want:     []int{4}, // per role the chat row alone serves (synthesis, dream): its WARN and the role's
		},
	}
	for _, p := range phases {
		t.Run(p.name, func(t *testing.T) {
			p.prepare(t)
			var perMinute []int
			for range minutes {
				logs := installRejectLogCapture(t)
				for range healthCheckMinute {
					rec := httptest.NewRecorder()
					h.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
					var body struct {
						Status            string            `json:"status"`
						Services          map[string]string `json:"services"`
						BlocktypeRegistry string            `json:"blocktype_registry"`
						SchemaContract    string            `json:"schema_contract"`
						Version           string            `json:"version"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
						t.Fatalf("decode %s: %v", rec.Body.Bytes(), err)
					}
					if rec.Code != p.code || body.Status != p.status || !reflect.DeepEqual(body.Services, p.services) ||
						body.BlocktypeRegistry != "ok" || body.SchemaContract != contractValue || body.Version != provenance.BuildVersion() {
						t.Fatalf("/health answered %d %s, want %d status=%s services=%v blocktype_registry=ok schema_contract=%s version=%s",
							rec.Code, rec.Body.Bytes(), p.code, p.status, p.services, contractValue, provenance.BuildVersion())
					}
				}
				perMinute = append(perMinute, healthOperatorLines(logs))
			}
			t.Logf("%s: /health lines at INFO and above per minute (2 requests each): %v", p.name, perMinute)
			for i, got := range perMinute {
				want := 0
				if i < len(p.want) {
					want = p.want[i]
				}
				if got != want {
					t.Errorf("%s: minute %d logged %d lines, want %d (all minutes: %v)", p.name, i+1, got, want, perMinute)
				}
			}
		})
	}
}

// TestHealthLogsTheDatabaseOncePerChange — the database ping: one ERROR when
// it starts failing, DEBUG while it keeps failing, one INFO when it answers
// again; a second outage is an ERROR again.
func TestHealthLogsTheDatabaseOncePerChange(t *testing.T) {
	sh := newSeedHarness(t, seedTestKeyHex, false)
	broken := brokenPool(t)
	const n = 5
	const failed, again = "health check: database ping failed", "health check: database answers again"
	calls := func(pool *pgxpool.Pool, k int) *rejectLogCapture {
		logs := installRejectLogCapture(t)
		for range k {
			HealthStatus(context.Background(), pool, nil, false, "ok")
		}
		return logs
	}
	count := func(logs *rejectLogCapture, msg string, level slog.Level) int {
		k := 0
		for _, r := range logs.withMsg(msg) {
			if r.level == level {
				k++
			}
		}
		return k
	}
	calls(sh.pool, 1) // a known state: the database answers
	for _, step := range []struct {
		name                     string
		broken                   bool
		errors, debugs, recovers int
	}{
		{"the database stops answering", true, 1, n - 1, 0},
		{"it answers again", false, 0, 0, 1},
		{"second outage", true, 1, n - 1, 0},
	} {
		t.Run(step.name, func(t *testing.T) {
			pool := sh.pool
			if step.broken {
				pool = broken
			}
			logs := calls(pool, n)
			if e, d, r := count(logs, failed, slog.LevelError), count(logs, failed, slog.LevelDebug), count(logs, again, slog.LevelInfo); e != step.errors || d != step.debugs || r != step.recovers {
				t.Errorf("%s: %d ERROR, %d DEBUG, %d INFO, want %d, %d, %d (all: %+v)", step.name, e, d, r,
					step.errors, step.debugs, step.recovers, logs.all())
			}
		})
	}
}

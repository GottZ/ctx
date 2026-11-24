//go:build integration

package handler

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestChatStageRefusesNUL (issue #519): the web chat stages the writes the
// model asks for; a NUL the model wrote into an argument reached the stage's
// statements (the pending-write insert, the id lookup) and came back as a
// server error, or as the database's text in the model-visible answer. It is
// refused before any statement, model-visible, naming the field — no staged
// row, no line.
func TestChatStageRefusesNUL(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	keyRow, keyPlain, err := store.CreateApiKey(ctx, pool, "n519-chat", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	var actx context.Context
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+keyPlain)
	Auth(pool)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { actx = r.Context() })).ServeHTTP(httptest.NewRecorder(), req)
	if actx == nil || AuthResultFromContext(actx) == nil {
		t.Fatalf("auth probe resolved no identity")
	}
	runner := &chatStageRunner{pool: pool}
	rec := &nulStageLines{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s := func(v string) *string { return &v }
	nul := "a\x00b"
	cases := []struct {
		name  string
		stage func() (string, error)
		field string
	}{
		{"store content", func() (string, error) {
			_, rej, err := runner.StageWrite(actx, "learnings", "t", nul, nil, nil)
			return rej, err
		}, "content"},
		{"store tag", func() (string, error) {
			_, rej, err := runner.StageWrite(actx, "learnings", "t", "c", []string{nul}, nil)
			return rej, err
		}, "tags"},
		{"store metadata", func() (string, error) {
			_, rej, err := runner.StageWrite(actx, "learnings", "t", "c", nil, map[string]any{"source": nul})
			return rej, err
		}, "metadata"},
		{"update id", func() (string, error) {
			_, rej, err := runner.StageUpdate(actx, "01a20000"+nul, nil, s("t"), nil, nil, nil)
			return rej, err
		}, "id"},
		{"update title", func() (string, error) {
			_, rej, err := runner.StageUpdate(actx, "01a20000", nil, s(nul), nil, nil, nil)
			return rej, err
		}, "title"},
	}
	for _, tc := range cases {
		rej, err := tc.stage()
		if err != nil || rej != tc.field+" "+nulFieldMessage {
			t.Errorf("%s: want the refusal %q, got reject %q err %v", tc.name, tc.field+" "+nulFieldMessage, rej, err)
		}
	}
	var staged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1`, keyRow.ID).Scan(&staged); err != nil {
		t.Fatalf("count staged: %v", err)
	}
	if staged != 0 {
		t.Errorf("%d staged rows, want 0", staged)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, l := range rec.lines {
		t.Errorf("a line above INFO: %s", l)
	}
}

// TestChatStageUpdateBooksIntent (issue #519): the web chat's staged update
// pays its write intent at stage time, in the stage's transaction, as every
// staged write does; at the budget it gets no card.
func TestChatStageUpdateBooksIntent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv("CTX_RATE_LIMIT_WRITE", "2")
	envCfg, issues := config.FromEnv()
	if config.HasErrors(append(issues, config.Validate(envCfg)...)) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	keyRow, keyPlain, err := store.CreateApiKey(ctx, pool, "n519-chat-budget", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	var actx context.Context
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+keyPlain)
	Auth(pool)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { actx = r.Context() })).ServeHTTP(httptest.NewRecorder(), req)
	if actx == nil || AuthResultFromContext(actx) == nil {
		t.Fatalf("auth probe resolved no identity")
	}
	b, err := store.UpsertBlock(ctx, pool, "learnings", "n519 chat budget", "body", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	runner := &chatStageRunner{pool: pool, cfg: config.NewStore(envCfg)}
	counts := func() (bookings, cards int) {
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_access_log WHERE api_key_id = $1 AND action = 'write'),
			(SELECT count(*) FROM context_pending_writes WHERE api_key_id = $1)`, keyRow.ID).Scan(&bookings, &cards); err != nil {
			t.Fatalf("counts: %v", err)
		}
		return bookings, cards
	}
	title := func(v string) *string { return &v }
	for i := range 2 {
		staged, rej, err := runner.StageUpdate(actx, b.ID, nil, title(fmt.Sprintf("n519 chat budget %d", i)), nil, nil, nil)
		if err != nil || rej != "" || staged == nil {
			t.Fatalf("stage %d: rej %q err %v", i, rej, err)
		}
		if bk, cards := counts(); bk != i+1 || cards != i+1 {
			t.Errorf("after stage %d: %d bookings, %d cards; want %d and %d", i, bk, cards, i+1, i+1)
		}
	}
	_, rej, err := runner.StageUpdate(actx, b.ID, nil, title("n519 chat budget over"), nil, nil, nil)
	if err != nil || rej != "Rate limit exceeded: max 2 writes per 60 seconds" {
		t.Errorf("over the budget: rej %q err %v", rej, err)
	}
	if bk, cards := counts(); bk != 2 || cards != 2 {
		t.Errorf("over the budget: %d bookings, %d cards; want 2 and 2", bk, cards)
	}
}

// nulStageLines records every record above INFO.
type nulStageLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *nulStageLines) Enabled(context.Context, slog.Level) bool { return true }
func (l *nulStageLines) Handle(_ context.Context, r slog.Record) error {
	if r.Level > slog.LevelInfo {
		l.mu.Lock()
		l.lines = append(l.lines, r.Level.String()+" "+r.Message)
		l.mu.Unlock()
	}
	return nil
}
func (l *nulStageLines) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *nulStageLines) WithGroup(string) slog.Handler      { return l }

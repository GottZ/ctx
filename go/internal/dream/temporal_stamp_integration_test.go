//go:build integration

// Issue #427: the dream's temporal review (RunDreamCycle step 1b) marks a
// block reviewed only for the version it reviewed — the title and content it
// sent to the LLM. The review waits on its LLM for seconds (up to the temporal
// timeout); a write that lands meanwhile is forced into that window here by
// the stubbed review, which blocks until the test releases it. The writes are
// the production ones (UpsertBlock, UpdateBlock, the temporal afterwork
// RefreshTemporal, StoreEmbedding of the embed arm).
//
// Run with:
//
//	go test -tags=integration ./internal/dream/ -run TestTemporalReviewStamp -count=1 -v
package dream_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	i427Scope  = "private"
	i427Model  = "m-i427"
	i427Origin = "http://gpu-i427-it:8089"
	// i427ReviewMarker is a phrase of the temporal review's system prompt
	// (temporalValidationPrompt): the stub answers that call and no other.
	i427ReviewMarker = "temporal reference extractor"
	// i427Wait bounds every wait of these tests on the cycle or the review.
	i427Wait = 30 * time.Second
)

// errI427Stop ends the cycle right after step 1b: RunDreamCycle calls the
// throttle first thing after the temporal review and its stamp, and returns
// the throttle's error. Keywords, search and eval are not this issue's part.
var errI427Stop = errors.New("i427: cycle stopped after the temporal review")

func i427StopAfterTemporal(context.Context) error { return errI427Stop }

// i427Review is the stubbed temporal review: answer decides the reply to
// each call, and calls counts them.
type i427Review struct {
	mu     sync.Mutex
	answer func(ctx context.Context, userPrompt string) (string, error)
	calls  atomic.Int32
}

func (rv *i427Review) set(fn func(ctx context.Context, userPrompt string) (string, error)) {
	rv.mu.Lock()
	defer rv.mu.Unlock()
	rv.answer = fn
}

func (rv *i427Review) reply(ctx context.Context, userPrompt string) (string, error) {
	rv.mu.Lock()
	fn := rv.answer
	rv.mu.Unlock()
	rv.calls.Add(1)
	return fn(ctx, userPrompt)
}

// hold makes the next review block: it sends its prompt on the returned
// channel when it begins and answers answer once release is closed.
func (rv *i427Review) hold(answer func(userPrompt string) string) (entered <-chan string, release chan struct{}) {
	in := make(chan string, 1)
	release = make(chan struct{})
	rv.set(func(ctx context.Context, userPrompt string) (string, error) {
		in <- userPrompt
		select {
		case <-release:
			return answer(userPrompt), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	return in, release
}

// record makes every following review answer at once, recording its prompt.
func (rv *i427Review) record(answer func(userPrompt string) string) *[]string {
	var mu sync.Mutex
	prompts := &[]string{}
	rv.set(func(_ context.Context, userPrompt string) (string, error) {
		mu.Lock()
		*prompts = append(*prompts, userPrompt)
		mu.Unlock()
		return answer(userPrompt), nil
	})
	return prompts
}

// i427Rig is one test database with a dream router whose LLM is the stub.
type i427Rig struct {
	pool   *pgxpool.Pool
	dsn    string
	router *dream.Router
	review *i427Review
}

func newI427Rig(t *testing.T) *i427Rig {
	t.Helper()
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()

	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "i427-it-gpu", Name: "i427-it-gpu", Host: i427Origin,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: i427Model}},
		Priority: 100, Enabled: true,
	}})
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	rig := &i427Rig{
		pool: pool, dsn: dsn,
		router: &dream.Router{
			Pool:       p,
			Blocktypes: reg,
			Admit:      llm.Admission{Admitter: d, Class: dispatch.ClassBackground},
		},
		review: &i427Review{},
	}
	rig.review.set(func(context.Context, string) (string, error) {
		return "", errors.New("i427: review not scripted")
	})
	swapChatJSON(t, func(ctx context.Context, _, _, _ string, _ *bool, systemPrompt, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if !strings.Contains(systemPrompt, i427ReviewMarker) {
			return nil, fmt.Errorf("i427: unscripted LLM call")
		}
		content, err := rig.review.reply(ctx, userPrompt)
		if err != nil {
			return nil, err
		}
		return &llm.ChatResponse{Message: llm.Message{Role: "assistant", Content: content}, EvalCount: 20, PromptTokens: 200}, nil
	})
	return rig
}

func i427Vec() pgvec.Vector {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = 0.03
	}
	return pgvec.NewVector(v)
}

// block stores a block the way a client write does: the write, its temporal
// afterwork, and the embed arm's vector (the pick takes embedded blocks only).
func (rig *i427Rig) block(t *testing.T, title, content string) *store.Block {
	t.Helper()
	ctx := context.Background()
	b, err := store.UpsertBlock(ctx, rig.pool, "decisions", title, content, nil, nil, i427Scope, true,
		store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, "")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if applied, err := store.RefreshTemporal(ctx, rig.pool, b.ID, b.Content, b.CreatedAt); err != nil || !applied {
		t.Fatalf("store afterwork: applied=%v err=%v", applied, err)
	}
	rig.embed(t, b.ID)
	return b
}

func (rig *i427Rig) embed(t *testing.T, id string) {
	t.Helper()
	v := i427Vec()
	if err := store.StoreEmbedding(context.Background(), rig.pool, id, i427Model, v.Slice()); err != nil {
		t.Fatalf("embed: %v", err)
	}
}

// update is a client update and, for a content write, its temporal afterwork.
func (rig *i427Rig) update(t *testing.T, pool *pgxpool.Pool, id string, data store.UpdateBlockData) *store.Block {
	t.Helper()
	ctx := context.Background()
	b, _, err := store.UpdateBlock(ctx, pool, id, data, []string{i427Scope})
	if err != nil || b == nil {
		t.Fatalf("update: %v", err)
	}
	if data.Content != nil {
		if _, err := store.RefreshTemporal(ctx, rig.pool, b.ID, b.Content, b.CreatedAt); err != nil {
			t.Fatalf("update afterwork: %v", err)
		}
	}
	return b
}

// cycle runs one dream cycle in the background; it ends after step 1b.
func (rig *i427Rig) cycle() <-chan error { return rig.cycleOn(rig.pool) }

// cycleOn is cycle with the dream's statements on pool.
func (rig *i427Rig) cycleOn(pool *pgxpool.Pool) <-chan error {
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), i427Wait)
		defer cancel()
		_, err := dream.RunDreamCycle(ctx, pool, rig.router, dream.DreamOptions(),
			dream.BackoffConfig{Mode: "exp", Factor: 1.6, MinHours: 12, CapHours: 1080, InertOffset: 7},
			[]string{i427Scope}, i427StopAfterTemporal)
		switch {
		case errors.Is(err, errI427Stop):
			done <- nil
		case err == nil:
			done <- errors.New("the cycle picked no block")
		default:
			done <- err
		}
	}()
	return done
}

func i427Await(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(i427Wait):
		t.Fatalf("%s did not finish within %s", what, i427Wait)
	}
}

func i427Entered(t *testing.T, entered <-chan string, done <-chan error) string {
	t.Helper()
	select {
	case p := <-entered:
		return p
	case err := <-done:
		t.Fatalf("the cycle ended before its temporal review began: %v", err)
	case <-time.After(i427Wait):
		t.Fatalf("the temporal review did not begin within %s", i427Wait)
	}
	return ""
}

// endClaim ends the pick's claim on the block (dream_cooldown_until, which
// otherwise runs out after PickClaimTTL), so the next cycle picks it again.
func (rig *i427Rig) endClaim(t *testing.T, id string) {
	t.Helper()
	if _, err := rig.pool.Exec(context.Background(),
		`UPDATE context_blocks SET dream_cooldown_until = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("end claim: %v", err)
	}
}

// due answers what the next pick decides for step 1b (BlockInfo.TemporalDue
// in RunDreamCycle): the block is due for a review when no review of the
// title and content it holds was recorded (store.TemporalReviewDueCol, issue
// #452).
func (rig *i427Rig) due(t *testing.T, id string) bool {
	t.Helper()
	var due bool
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT `+store.TemporalReviewDueCol+` FROM context_blocks WHERE id = $1`, id).Scan(&due); err != nil {
		t.Fatalf("read due: %v", err)
	}
	return due
}

// rowsAt counts the block's temporal rows for the day day.
func (rig *i427Rig) rowsAt(t *testing.T, id, day string) int {
	t.Helper()
	at, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatalf("parse %s: %v", day, err)
	}
	var n int
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_temporal WHERE block_id = $1 AND source_time = $2`, id, at).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// i427Dates answers a review with these dates.
func i427Dates(days ...string) func(string) string {
	return func(string) string {
		parts := make([]string, len(days))
		for i, d := range days {
			parts[i] = fmt.Sprintf(`{"date":%q,"source":"explicit"}`, d)
		}
		return `{"dates":[` + strings.Join(parts, ",") + `]}`
	}
}

// i427Logs records the messages logged for one block.
type i427Logs struct {
	block string
	mu    sync.Mutex
	msgs  []string
}

func (l *i427Logs) Enabled(context.Context, slog.Level) bool { return true }
func (l *i427Logs) WithAttrs([]slog.Attr) slog.Handler      { return l }
func (l *i427Logs) WithGroup(string) slog.Handler           { return l }
func (l *i427Logs) Handle(_ context.Context, r slog.Record) error {
	mine := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "block_id" && a.Value.String() == l.block {
			mine = true
			return false
		}
		return true
	})
	if mine {
		l.mu.Lock()
		l.msgs = append(l.msgs, r.Message)
		l.mu.Unlock()
	}
	return nil
}

func (l *i427Logs) has(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.msgs {
		if strings.Contains(m, part) {
			return true
		}
	}
	return false
}

func i427CaptureLogs(t *testing.T, block string) *i427Logs {
	t.Helper()
	l := &i427Logs{block: block}
	prev := slog.Default()
	slog.SetDefault(slog.New(l))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return l
}

// TestTemporalReviewStampSkipsContentReplacedDuringReview is the issue's
// case: the content is replaced while the review waits on its LLM. The block
// must stay due, so the next pick reviews the content it holds. Before, the
// cycle stamped now() after the review, later than the new content's
// updated_at, and the new content was never reviewed. With a finding, the
// review's merge is dropped (the content guard of #414) and ValidateTemporal
// reports it; without one, nothing is written and the stamp's own comparison
// alone keeps the block due.
func TestTemporalReviewStampSkipsContentReplacedDuringReview(t *testing.T) {
	for _, tc := range []struct {
		name    string
		finding []string
	}{
		{"with a finding", []string{"2026-03-22"}},
		{"without a finding", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newI427Rig(t)
			picked := "Kickoff on 2026-03-15, the retro a week later."
			b := rig.block(t, "Kickoff plan", picked)
			logs := i427CaptureLogs(t, b.ID)

			entered, release := rig.review.hold(i427Dates(tc.finding...))
			done := rig.cycle()
			if p := i427Entered(t, entered, done); !strings.Contains(p, picked) {
				t.Fatalf("the review was sent %q, want the picked content", p)
			}
			replaced := "Kickoff moved to 2026-07-20."
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Content: &replaced})
			close(release)
			i427Await(t, done, "first cycle")

			if !rig.due(t, b.ID) {
				t.Errorf("the block is marked reviewed although its content was replaced during the review")
			}
			if n := rig.rowsAt(t, b.ID, "2026-03-22"); n != 0 {
				t.Errorf("the review's finding for the replaced content landed: %d rows", n)
			}
			if logs.has("LLM found additional dates") {
				t.Errorf("the dropped merge was logged as findings of the block: %q", logs.msgs)
			}

			// The next pick reviews the content the block holds.
			rig.embed(t, b.ID)
			rig.endClaim(t, b.ID)
			prompts := rig.review.record(i427Dates())
			i427Await(t, rig.cycle(), "second cycle")
			if len(*prompts) != 1 || !strings.Contains((*prompts)[0], replaced) {
				t.Errorf("the next cycle sent %d review(s) %q, want one of the replaced content", len(*prompts), *prompts)
			}
			if rig.due(t, b.ID) {
				t.Errorf("the block is still due after the review of its current content")
			}
		})
	}
}

// TestTemporalReviewStampKeepsTagsOnlyUpdate pins the decision for a write
// that changes neither title nor content: the review read nothing it changed,
// so the stamp lands and the next pick does not review again.
func TestTemporalReviewStampKeepsTagsOnlyUpdate(t *testing.T) {
	rig := newI427Rig(t)
	b := rig.block(t, "Release plan", "Release on 2026-04-02.")

	entered, release := rig.review.hold(i427Dates("2026-04-09"))
	done := rig.cycle()
	i427Entered(t, entered, done)
	rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Tags: []string{"release"}})
	close(release)
	i427Await(t, done, "first cycle")

	if rig.due(t, b.ID) {
		t.Errorf("a tags-only update during the review left the block due")
	}
	if n := rig.rowsAt(t, b.ID, "2026-04-09"); n != 8 {
		t.Errorf("the review's finding has %d rows, want 8 (a tags-only update keeps it)", n)
	}

	rig.endClaim(t, b.ID)
	prompts := rig.review.record(i427Dates())
	i427Await(t, rig.cycle(), "second cycle")
	if len(*prompts) != 0 {
		t.Errorf("the next cycle reviewed the block again: %d review(s)", len(*prompts))
	}
}

// TestTemporalReviewStampNormalCase: no write during the review — the finding
// lands, the block is marked reviewed and the next pick does not review again.
func TestTemporalReviewStampNormalCase(t *testing.T) {
	rig := newI427Rig(t)
	b := rig.block(t, "Budget plan", "Budget review on 2026-05-04.")
	logs := i427CaptureLogs(t, b.ID)

	prompts := rig.review.record(i427Dates("2026-05-11"))
	i427Await(t, rig.cycle(), "first cycle")
	if len(*prompts) != 1 {
		t.Fatalf("the cycle sent %d review(s), want 1", len(*prompts))
	}
	if rig.due(t, b.ID) {
		t.Errorf("the reviewed block is still due")
	}
	if n := rig.rowsAt(t, b.ID, "2026-05-11"); n != 8 {
		t.Errorf("the review's finding has %d rows, want 8", n)
	}
	if !logs.has("LLM found additional dates") {
		t.Errorf("the merged finding was not logged: %q", logs.msgs)
	}

	rig.endClaim(t, b.ID)
	i427Await(t, rig.cycle(), "second cycle")
	if len(*prompts) != 1 {
		t.Errorf("the next cycle reviewed the unchanged block again: %d review(s) in all", len(*prompts))
	}
}

// TestTemporalReviewStampWithAContentWriteInFlight: a content write that
// holds the block row when the review ends (here held at its COMMIT). Since
// issue #452 the review's write does not wait for it (the write holds the row
// FOR NO KEY UPDATE, the review's rows take FOR KEY SHARE): it records the
// version it read, which the block holds no longer once the write commits —
// the block must then be due. Before issue #427 the stamp was written
// regardless; under #427 it waited for the write and judged what it left.
func TestTemporalReviewStampWithAContentWriteInFlight(t *testing.T) {
	rig := newI427Rig(t)
	b := rig.block(t, "Offsite plan", "Offsite on 2026-06-02.")

	holdPool, hold := testdb.HoldCommitPool(t, rig.dsn, func(sql string) bool {
		return strings.HasPrefix(strings.TrimSpace(sql), "UPDATE context_blocks SET") && strings.Contains(sql, "content = $")
	})
	entered, release := rig.review.hold(i427Dates())
	done := rig.cycle()
	i427Entered(t, entered, done)

	replaced := "Offsite moved to 2026-09-08."
	updated := make(chan struct{})
	go func() {
		defer close(updated)
		if _, _, err := store.UpdateBlock(context.Background(), holdPool, b.ID, store.UpdateBlockData{Content: &replaced}, []string{i427Scope}); err != nil {
			t.Errorf("update: %v", err)
		}
	}()
	updPID := hold.Wait(t)

	// The review ends while the write holds the row; its record does not
	// wait for the write.
	close(release)
	i427Await(t, done, "cycle while the write holds the row")
	t.Logf("the cycle finished while the content write (pid %d) held the row", updPID)
	if rig.due(t, b.ID) {
		t.Fatalf("the review of the version it read was not recorded while the write held the row")
	}
	hold.Release()
	<-updated

	if !rig.due(t, b.ID) {
		t.Errorf("the stamp landed over the content the write in flight stored")
	}
}

// TestValidateTemporalReportsADroppedMerge pins what ValidateTemporal tells
// its caller: dropped=true when the merge of its findings was not applied
// because the content was replaced during the review, dropped=false when the
// findings landed.
func TestValidateTemporalReportsADroppedMerge(t *testing.T) {
	rig := newI427Rig(t)
	ctx := context.Background()
	b := rig.block(t, "Launch plan", "Launch on 2026-08-03.")
	pick := func() *dream.BlockInfo {
		t.Helper()
		info, err := dream.PickBlock(ctx, rig.pool, rig.router.TypeSet(ctx).DreamLinkableTypes(), []string{i427Scope}, dream.PickClaimTTL(rig.router))
		if err != nil || info == nil || info.ID != b.ID {
			t.Fatalf("pick: info=%v err=%v", info, err)
		}
		return info
	}

	info := pick()
	entered, release := rig.review.hold(i427Dates("2026-08-10"))
	type result struct {
		dropped bool
		err     error
	}
	res := make(chan result, 1)
	go func() {
		dropped, err := dream.ValidateTemporal(ctx, rig.pool, rig.router, dream.DreamOptions(), info)
		res <- result{dropped, err}
	}()
	select {
	case <-entered:
	case <-time.After(i427Wait):
		t.Fatalf("the review did not begin within %s", i427Wait)
	}
	replaced := "Launch moved to 2026-08-24."
	rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Content: &replaced})
	close(release)
	if r := <-res; r.err != nil || !r.dropped {
		t.Errorf("replaced during the review: dropped=%v err=%v, want dropped", r.dropped, r.err)
	}

	rig.endClaim(t, b.ID)
	info = pick()
	rig.review.record(i427Dates("2026-08-31"))
	if dropped, err := dream.ValidateTemporal(ctx, rig.pool, rig.router, dream.DreamOptions(), info); err != nil || dropped {
		t.Errorf("unchanged during the review: dropped=%v err=%v, want not dropped", dropped, err)
	}
	if n := rig.rowsAt(t, b.ID, "2026-08-31"); n != 8 {
		t.Errorf("the finding has %d rows, want 8", n)
	}
}

// TestTemporalReviewStampSkipsADroppedMergeWhenTheContentReturns: the
// review's merge is dropped because the content was replaced (X to Y), and the
// content is set back to X before the stamp. The block then holds what the
// review read, but not its findings: it must stay due. The write back is
// queued on the block row before the merge runs (a transaction holds the row
// FOR NO KEY UPDATE, which the merge's foreign key share lock does not wait
// for), so it commits between the merge and any stamp.
func TestTemporalReviewStampSkipsADroppedMergeWhenTheContentReturns(t *testing.T) {
	rig := newI427Rig(t)
	ctx := context.Background()
	picked := "Audit on 2026-10-05."
	b := rig.block(t, "Audit plan", picked)

	entered, release := rig.review.hold(i427Dates("2026-10-12"))
	done := rig.cycle()
	i427Entered(t, entered, done)
	other := "Audit cancelled."
	rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Content: &other})

	tx, err := rig.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin hold: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var holdPID uint32
	if err := tx.QueryRow(ctx,
		`SELECT pg_backend_pid() FROM context_blocks WHERE id = $1 FOR NO KEY UPDATE`, b.ID).Scan(&holdPID); err != nil {
		t.Fatalf("hold row: %v", err)
	}
	back := make(chan struct{})
	go func() {
		defer close(back)
		if _, _, err := store.UpdateBlock(ctx, rig.pool, b.ID, store.UpdateBlockData{Content: &picked}, []string{i427Scope}); err != nil {
			t.Errorf("write back: %v", err)
		}
	}()
	testdb.WaitBlockedBy(t, rig.pool, holdPID, back, "the write back", "transactionid")

	// The review ends; its merge finds Y and is dropped. A stamp, if any,
	// queues behind the write back.
	close(release)
	var cycleErr error
	cycled := make(chan struct{})
	go func() {
		defer close(cycled)
		cycleErr = <-done
	}()
	i427UntilDoneOrSecondWaiter(t, rig.pool, cycled)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("release hold: %v", err)
	}
	<-back
	select {
	case <-cycled:
	case <-time.After(i427Wait):
		t.Fatalf("the cycle did not finish within %s", i427Wait)
	}
	if cycleErr != nil {
		t.Fatalf("cycle: %v", cycleErr)
	}

	if n := rig.rowsAt(t, b.ID, "2026-10-12"); n != 0 {
		t.Fatalf("the dropped merge landed: %d rows", n)
	}
	if !rig.due(t, b.ID) {
		t.Errorf("the block is marked reviewed although the review's findings were dropped")
	}
}

// i427UntilDoneOrSecondWaiter waits until the cycle is done or a second
// backend of the test database waits on a lock: a stamp queued behind the
// write back. It queues on the tuple lock the write back holds while that
// waits for the hold, so pg_blocking_pids names the write back, not the
// hold; the test database is the test's own, so the count is these two.
func i427UntilDoneOrSecondWaiter(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(i427Wait / 2)
	for {
		select {
		case <-done:
			return
		default:
		}
		var waiters int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiters); err != nil {
			t.Fatalf("poll waiters: %v", err)
		}
		if waiters >= 2 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("neither the cycle finished nor a stamp queued within %s", i427Wait/2)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

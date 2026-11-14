//go:build integration

// Second review of #214, W4-NERF's thread "a client cancel puts the oldest
// block, of any tenant, into a growing hold". The inline backfill runs each
// wire call under what is left of the sync budget, and that deadline hangs off
// the request context: a client that gave up mid-embed ended the call the way
// an overrun budget does, and the block was memoized caller_timeout with one
// attempt more. With the background arm off that memo is a hold of
// backoff_base * 2^(attempts-1) at the old end of the pick, so every cancel
// doubled the hold of a block that may be another tenant's, and its owner's
// own queries waited it out too (60 s, 120 s, 240 s ... 86 400 s from the
// twelfth cancel on).
//
// A request that ends mid-embed is not an attempt: its memo is caller_timeout
// with next_attempt_at = now(), and attempts stays what it was. The hold that
// memo starts is what keeps the impatient client from meeting the same block
// on its next request; without a memo for the cancel it is answered 500 on
// every request. A budget that runs out while the request lives still counts.
//
// RED before the fix: cancels_leave_the_backoff_where_it_was and both
// subtests of cancel_on_a_wire_memo_keeps_its_level. The other two pass
// before and after: impatient_client_is_answered_after_one_500 pins that the
// cancel still writes a memo that holds the block (the variant without one),
// budget_overrun_still_counts_an_attempt that the cancel is told from the
// budget by the request's own context.
//
// Every probe drives POST /api/store and POST /api/query with two keys of two
// tenants (bfsFixture) and has a database of its own. Time is advanced by
// moving the memo's next_attempt_at back, never waited for.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryBackfillClientCancel -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

const (
	// bfcSlowTitle names the one block whose embed is slow; the backend
	// finds it in the embed text.
	bfcSlowTitle = "bfc block whose embed is slow"
	bfcSlowDelay = 5 * time.Second
	bfcArmOn     = 15 * time.Second
)

// bfcBackend is the backend of the cancel probes. The embed of the block
// titled bfcSlowTitle takes slow, or until the client goes away; every other
// embed, the question's included, is answered at once. /api/chat answers as
// bw2Backend does.
type bfcBackend struct {
	srv      *httptest.Server
	slowHits atomic.Int32
	slow     atomic.Int64
	// onSlow, when set, runs as the slow embed arrives: the probe's client
	// giving up mid-embed.
	onSlow atomic.Pointer[context.CancelFunc]
}

func bfcNewBackend(t *testing.T) *bfcBackend {
	t.Helper()
	b := &bfcBackend{}
	b.slow.Store(int64(bfcSlowDelay))
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The embed text is in the body, and net/http reports a client that
		// goes away only once the body has been read (see slowEmbedServer).
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/embed":
			if strings.Contains(string(raw), bfcSlowTitle) {
				b.slowHits.Add(1)
				if cancel := b.onSlow.Load(); cancel != nil {
					(*cancel)()
				}
				select {
				case <-time.After(time.Duration(b.slow.Load())):
				case <-r.Context().Done():
					return
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
		case "/api/chat":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message":    map[string]any{"role": "assistant", "content": "database status"},
				"eval_count": 1, "prompt_eval_count": 1,
			})
		default:
			t.Errorf("unexpected backend path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// bfcSetup is bfsSetup in front of a bfcBackend.
func bfcSetup(t *testing.T) (*bfsFixture, *bfcBackend) {
	t.Helper()
	f := bfsSetup(t)
	b := bfcNewBackend(t)
	f.backend = &bw2Backend{srv: b.srv}
	return f, b
}

// bfcConfig is the request-path config of the probes: sync_cap 4, the given
// sync_budget and arm interval (0 = arm off), backoff_base 60 s and
// backoff_cap 24 h (bw2Config).
func bfcConfig(budget, interval time.Duration) *config.Config {
	cfg := bw2Config()
	cfg.EmbedBackfill.SyncCap = 4
	cfg.EmbedBackfill.SyncBudget = budget
	cfg.EmbedBackfill.Interval = interval
	return cfg
}

// memo reads the regular failure memo of the block with this title: its
// class, its attempts, and whether it is due at once (next_attempt_at <=
// now(), what the background arm's predicate asks).
func (f *bfsFixture) memo(t *testing.T, title string) (class string, attempts int, due bool) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT m.last_class, m.attempts, m.next_attempt_at <= now()
		 FROM context_embed_failures m JOIN context_blocks cb ON cb.id = m.block_id
		 WHERE cb.title = $1 AND m.migration_id IS NULL`, title).Scan(&class, &attempts, &due); err != nil {
		t.Fatalf("read failure memo of %q: %v", title, err)
	}
	return class, attempts, due
}

// ageMemo moves the next_attempt_at of the block's memo, for a caller_timeout
// memo the time of the cut, to ago before now (an interval as Postgres reads
// it). It is the probes' clock.
func (f *bfsFixture) ageMemo(t *testing.T, title, ago string) {
	t.Helper()
	tag, err := f.pool.Exec(context.Background(),
		`UPDATE context_embed_failures m SET next_attempt_at = now() - $2::interval
		 FROM context_blocks cb
		 WHERE cb.id = m.block_id AND cb.title = $1 AND m.migration_id IS NULL`, title, ago)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("age the memo of %q: %v (%d rows)", title, err, tag.RowsAffected())
	}
}

// wireMemo gives the block a wire memo of n attempts through the production
// writer and puts its backoff a day back: a block whose backend failed n
// times and whose retry is due.
func (f *bfsFixture) wireMemo(t *testing.T, title string, n int) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
		t.Fatalf("read block id of %q: %v", title, err)
	}
	for range n {
		if err := store.RecordEmbedFailure(ctx, f.pool, id, store.EmbedFailureWire,
			store.NormalizeEmbedError(store.EmbedFailureWire, "embed backend answered 503"),
			time.Minute, 24*time.Hour); err != nil {
			t.Fatalf("record wire memo: %v", err)
		}
	}
	f.ageMemo(t, title, "1 day")
}

// cancelMidEmbed runs one query under keyCtx's key and cancels it once the
// backend has the slow block's embed in hand, the way a client that gives up
// goes away. The probe fails unless that request reached the embed.
func (f *bfsFixture) cancelMidEmbed(t *testing.T, b *bfcBackend, keyCtx context.Context, cfg *config.Config) {
	t.Helper()
	reqCtx, cancel := context.WithCancel(keyCtx)
	defer cancel()
	b.onSlow.Store(&cancel)
	defer b.onSlow.Store(nil)
	before := b.slowHits.Load()
	f.queryAs(t, reqCtx, cfg)
	if got := b.slowHits.Load() - before; got != 1 || reqCtx.Err() == nil {
		t.Fatalf("premise: the request made %d embed calls for the slow block and its context is %v; want one call and a cancelled request",
			got, reqCtx.Err())
	}
}

// patientQuery is one query under keyCtx's key that waits as long as it
// takes, and must answer 200.
func (f *bfsFixture) patientQuery(t *testing.T, keyCtx context.Context, cfg *config.Config) {
	t.Helper()
	if code, body := f.queryAs(t, keyCtx, cfg); code != http.StatusOK {
		t.Fatalf("query: status %d, body %s", code, body)
	}
}

func TestQueryBackfillClientCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	callerTimeout := string(store.EmbedFailureCallerTimeout)

	// Arm off, a budget the embed fits: three clients of the caller give up
	// while the inline backfill embeds the oldest pending block, which is the
	// other tenant's. Each cancel leaves the memo at one attempt, due for the
	// arm at once, and starts the hold of one cut, so the caller's next query
	// does not meet the block again. After that hold (60 s, passed at 90 s)
	// the owner's own query embeds the block; at three attempts the hold
	// would be 240 s.
	t.Run("cancels_leave_the_backoff_where_it_was", func(t *testing.T) {
		f, b := bfcSetup(t)
		f.write(t, f.foreignCtx, bfcSlowTitle)
		f.write(t, f.callerCtx, bfsCallerTitle)

		for cancel := 1; cancel <= 3; cancel++ {
			if cancel > 1 {
				// An hour later, past the hold any number of these cuts could
				// have started, so the next request takes the block again.
				f.ageMemo(t, bfcSlowTitle, "1 hour")
			}
			f.cancelMidEmbed(t, b, f.callerCtx, bfcConfig(10*time.Second, 0))
			if class, attempts, due := f.memo(t, bfcSlowTitle); class != callerTimeout || attempts != 1 || !due {
				t.Fatalf("after cancel %d: memo class %q, attempts %d, due at once %v; want %q, 1, true (a client that gives up is no attempt)",
					cancel, class, attempts, due, callerTimeout)
			}
			hits := b.slowHits.Load()
			f.patientQuery(t, f.callerCtx, bfcConfig(10*time.Second, 0))
			if b.slowHits.Load() != hits {
				t.Fatalf("after cancel %d: the caller's next query embedded the block again; the cancel must start a hold", cancel)
			}
		}

		b.slow.Store(0)
		f.ageMemo(t, bfcSlowTitle, "90 seconds")
		f.patientQuery(t, f.foreignCtx, bfcConfig(10*time.Second, 0))
		if !f.embedded(t, bfcSlowTitle) {
			_, attempts, _ := f.memo(t, bfcSlowTitle)
			t.Errorf("arm off, three cancels of another tenant's client, 90 s later: the owner's query left its own block pending (memo attempts %d); the hold of one cut is 60 s", attempts)
		}
	})

	// A client with a deadline of 1 s in front of a block whose embed takes
	// 5 s. Its first request ends inside that embed and is answered 500; the
	// next ones find the block inside its hold and are answered 200. With
	// the arm off the block is the other tenant's oldest one, with the arm on
	// the caller's own (the arm-on pick is bound to the caller's scopes).
	t.Run("impatient_client_is_answered_after_one_500", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			interval time.Duration
		}{
			{"arm_off", 0},
			{"arm_on", bfcArmOn},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f, b := bfcSetup(t)
				owner := f.foreignCtx
				if tc.interval > 0 {
					owner = f.callerCtx
				}
				f.write(t, owner, bfcSlowTitle)

				var got []int
				for range 4 {
					reqCtx, cancel := context.WithTimeout(f.callerCtx, time.Second)
					code, _ := f.queryAs(t, reqCtx, bfcConfig(30*time.Second, tc.interval))
					cancel()
					got = append(got, code)
				}
				want := []int{http.StatusInternalServerError, http.StatusOK, http.StatusOK, http.StatusOK}
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("%s: four requests with a deadline of 1 s answered %v, want %v (one 500, then the block is inside its hold)", tc.name, got, want)
					}
				}
				if n := b.slowHits.Load(); n != 1 {
					t.Errorf("%s: %d embed calls for the slow block, want 1", tc.name, n)
				}
			})
		}
	})

	// Control: a budget that runs out while the request lives is an attempt,
	// as before the fix. Two cuts an hour apart leave attempts 2, and each
	// query is answered 200, which is what the budget is for.
	t.Run("budget_overrun_still_counts_an_attempt", func(t *testing.T) {
		f, b := bfcSetup(t)
		f.write(t, f.foreignCtx, bfcSlowTitle)

		for cut := 1; cut <= 2; cut++ {
			if cut > 1 {
				f.ageMemo(t, bfcSlowTitle, "1 hour")
			}
			f.patientQuery(t, f.callerCtx, bfcConfig(time.Second, 0))
			if class, attempts, _ := f.memo(t, bfcSlowTitle); class != callerTimeout || attempts != cut {
				t.Fatalf("after budget cut %d: memo class %q, attempts %d, want %q and %d (a cut by the budget is an attempt)",
					cut, class, attempts, callerTimeout, cut)
			}
		}
		if n := b.slowHits.Load(); n != 2 {
			t.Errorf("%d embed calls for the slow block, want 2", n)
		}
	})

	// A block whose backend failed three times carries a wire memo whose
	// retry is due, and a client gives up while the inline backfill makes
	// that retry. The cancel neither adds a fourth attempt nor forgets the
	// three: the memo becomes caller_timeout at attempts 3, due at once. With
	// the arm on that hands the block to the arm, and the query path leaves
	// it alone. With the arm off the hold it starts is the one of three
	// attempts (240 s at backoff_base 60 s): not passed after 200 s, passed
	// after 300 s, where four attempts would hold it 480 s.
	t.Run("cancel_on_a_wire_memo_keeps_its_level", func(t *testing.T) {
		t.Run("arm_off", func(t *testing.T) {
			f, b := bfcSetup(t)
			f.write(t, f.foreignCtx, bfcSlowTitle)
			f.wireMemo(t, bfcSlowTitle, 3)

			f.cancelMidEmbed(t, b, f.callerCtx, bfcConfig(10*time.Second, 0))
			if class, attempts, due := f.memo(t, bfcSlowTitle); class != callerTimeout || attempts != 3 || !due {
				t.Fatalf("after the cancel: memo class %q, attempts %d, due at once %v; want %q, 3, true",
					class, attempts, due, callerTimeout)
			}

			b.slow.Store(0)
			f.ageMemo(t, bfcSlowTitle, "200 seconds")
			f.patientQuery(t, f.foreignCtx, bfcConfig(10*time.Second, 0))
			if f.embedded(t, bfcSlowTitle) {
				t.Fatal("200 s after the cancel the block was embedded; the hold of its three attempts is 240 s, the cancel must not reset it")
			}
			f.ageMemo(t, bfcSlowTitle, "300 seconds")
			f.patientQuery(t, f.foreignCtx, bfcConfig(10*time.Second, 0))
			if !f.embedded(t, bfcSlowTitle) {
				_, attempts, _ := f.memo(t, bfcSlowTitle)
				t.Errorf("300 s after the cancel the owner's query left its block pending (memo attempts %d); the hold of three attempts is 240 s", attempts)
			}
		})

		t.Run("arm_on", func(t *testing.T) {
			f, b := bfcSetup(t)
			f.write(t, f.callerCtx, bfcSlowTitle)
			f.wireMemo(t, bfcSlowTitle, 3)

			f.cancelMidEmbed(t, b, f.callerCtx, bfcConfig(10*time.Second, bfcArmOn))
			if class, attempts, due := f.memo(t, bfcSlowTitle); class != callerTimeout || attempts != 3 || !due {
				t.Fatalf("after the cancel: memo class %q, attempts %d, due at once %v; want %q, 3, true (due at once is the hand-off to the arm)",
					class, attempts, due, callerTimeout)
			}
			hits := b.slowHits.Load()
			f.patientQuery(t, f.callerCtx, bfcConfig(10*time.Second, bfcArmOn))
			if b.slowHits.Load() != hits {
				t.Error("arm on: the caller's next query embedded the block again; after the cancel it is the arm's")
			}
		})
	})
}

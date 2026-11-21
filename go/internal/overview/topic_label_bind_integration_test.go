//go:build integration

// Issue #452 point 5 (4a), the topic label's drift anchor: the label arm
// (topiclabel.Run) selects a stale topic with the core_hash of its node row,
// asks the model, and writes the name with label_core_hash = that hash and
// label_stale = false. A rebuild that persists a new core in between makes the
// read hash an old one; written unbound, the name of the old core lands as
// current and the topic leaves the selection until the next rebuild flips
// label_stale back. The write — and the quiesce of the credentials opt-in,
// which acknowledges the same anchor without a call — must land only while the
// node row still carries the hash the arm read, and must wait for a rebuild
// that is rewriting the node rows to see its result.
//
// The rebuild is the production persist; the label arm runs with a fake chat
// seam. The window is forced by the seam (a rebuild inside the call) and by a
// commit hold on the rebuild's pool (a rebuild in flight across the tick).
//
//	go test -tags=integration ./internal/overview/ -run TestTopicLabelAnchor -count=1 -v
package overview

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/internal/topiclabel"
)

// i452LabelDeps is a tick with a digest-capable backend and chat as the seam.
func i452LabelDeps(pool *pgxpool.Pool, chat topiclabel.ChatFunc, credentialsFallbackOnly bool) topiclabel.Deps {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{{
		ID: "1", Name: "local", Trust: backends.TrustFull, Locality: "lan",
		Roles: []string{backends.RoleDigest}, Priority: 100, Enabled: true,
	}})
	return topiclabel.Deps{
		Pool: pool, Backends: bp, Chat: chat,
		Cfg: topiclabel.Config{
			Enabled: true, Batch: 10, MinTopics: 1, PromptMaxTitles: 24,
			Interval: time.Hour, VisibleTypes: w3Types, CredentialsFallbackOnly: credentialsFallbackOnly,
		},
	}
}

// i452Anchor is the topic's label state next to its node row.
type i452Anchor struct {
	label, source    string
	stale            bool
	anchor, nodeHash string
}

func i452AnchorOf(t *testing.T, pool *pgxpool.Pool, topic string) i452Anchor {
	t.Helper()
	var a i452Anchor
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(t.label, ''), t.label_source, t.label_stale, COALESCE(t.label_core_hash, ''), n.core_hash
		  FROM graph_cluster_topic t
		  JOIN graph_cluster_node n ON n.topic_id = t.topic_id AND n.scope = t.scope
		 WHERE t.topic_id = $1::uuid`, topic).
		Scan(&a.label, &a.source, &a.stale, &a.anchor, &a.nodeHash); err != nil {
		t.Fatalf("anchor of %s: %v", topic, err)
	}
	return a
}

// wantConsistent: a topic is out of the selection only while its anchor is
// the hash of its current core.
func (a i452Anchor) wantConsistent(t *testing.T, what string) {
	t.Helper()
	if !a.stale && a.anchor != a.nodeHash {
		t.Errorf("%s: label %q (%s) is not stale, but its anchor %.12s is not the core hash %.12s of the node row",
			what, a.label, a.source, a.anchor, a.nodeHash)
	}
}

// i452Generations persists generation one of a six-block topic, and returns
// the second generation's groups: the same members (the topic carries) with
// another core.
func i452Generations(t *testing.T, pool *pgxpool.Pool, scope, sensitivity string) (topic string, second w3Group) {
	t.Helper()
	ids := w3Blocks(t, pool, scope, 4520, 6)
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET sensitivity = $2 WHERE id = ANY($1::uuid[])`, ids, sensitivity); err != nil {
		t.Fatal(err)
	}
	w3Run(t, pool, scope, 45*24*time.Hour, w3Group{members: ids})
	topic = w3TopicOf(t, pool, scope, ids[0])
	if a := i452AnchorOf(t, pool, topic); !a.stale {
		t.Fatalf("precondition: a fresh topic is not stale (%+v)", a)
	}
	return topic, w3Group{members: ids, degrees: []float64{1, 1, 1, 2, 2, 2}}
}

func TestTopicLabelAnchor(t *testing.T) {
	t.Run("a rebuild persists a new core while the model answers", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		const scope = "i452lbl1"
		topic, second := i452Generations(t, pool, scope, "internal")
		calls := 0
		chat := func(context.Context, topiclabel.Deps, backends.Sensitivity, string, string, []string) (string, string, error) {
			calls++
			if calls == 1 {
				w3Run(t, pool, scope, 45*24*time.Hour, second)
			}
			return `{"label":"Harbour logistics"}`, "fake-model", nil
		}
		ctx := context.Background()
		topiclabel.Run(ctx, i452LabelDeps(pool, chat, false), []string{scope})
		if w3TopicOf(t, pool, scope, second.members[0]) != topic {
			t.Fatalf("precondition: the second generation did not carry the topic")
		}
		a := i452AnchorOf(t, pool, topic)
		a.wantConsistent(t, "after the tick")

		// The next tick names the core the topic holds now.
		topiclabel.Run(ctx, i452LabelDeps(pool, chat, false), []string{scope})
		if a := i452AnchorOf(t, pool, topic); calls != 2 || a.stale || a.anchor != a.nodeHash || a.source != "llm" {
			t.Errorf("next tick: calls=%d state=%+v, want a second call that names the current core", calls, a)
		}
	})

	for _, tc := range []struct {
		name, sensitivity string
		fallbackOnly      bool
	}{
		{"a rebuild in flight across the label write", "internal", false},
		{"a rebuild in flight across the credentials quiesce", "credentials", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			scope := "i452lbl2"
			if tc.fallbackOnly {
				scope = "i452lbl3"
			}
			topic, second := i452Generations(t, pool, scope, tc.sensitivity)

			held, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool {
				return strings.Contains(sql, "DELETE FROM graph_cluster_node")
			})
			rebuildDone := make(chan error, 1)
			go func() {
				_, err := persist(context.Background(), held,
					clusteringOf(second),
					Options{Resolution: 1.0, VisibleTypes: w3Types, ScopeFilter: []string{scope}, TombstoneRetention: 45 * 24 * time.Hour},
					scopesOf(second, scope), tallyScopes(scopesOf(second, scope)), superLevel{})
				rebuildDone <- err
			}()
			pid := hold.Wait(t)

			var mu sync.Mutex
			calls := 0
			chat := func(context.Context, topiclabel.Deps, backends.Sensitivity, string, string, []string) (string, string, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				return `{"label":"Harbour logistics"}`, "fake-model", nil
			}
			tickDone := make(chan struct{})
			go func() {
				defer close(tickDone)
				topiclabel.Run(context.Background(), i452LabelDeps(pool, chat, tc.fallbackOnly), []string{scope})
			}()
			testdb.WaitBlockedBy(t, pool, pid, tickDone, "the label tick", "")
			hold.Release()
			select {
			case <-tickDone:
			case <-time.After(testdb.CommitHoldBound):
				t.Fatalf("the tick did not finish within %s", testdb.CommitHoldBound)
			}
			if err := <-rebuildDone; err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			mu.Lock()
			if tc.fallbackOnly && calls != 0 {
				t.Errorf("the credentials opt-in reached the model %d time(s)", calls)
			}
			mu.Unlock()
			i452AnchorOf(t, pool, topic).wantConsistent(t, "after the tick")
		})
	}
}

// clusteringOf and scopesOf are the w3Run inputs of one group in one scope.
func clusteringOf(g w3Group) clustering {
	assign, deg := map[string]string{}, map[string]float64{}
	cluster := ""
	for _, m := range g.members {
		if cluster == "" || m < cluster {
			cluster = m
		}
	}
	for i, m := range g.members {
		assign[m], deg[m] = cluster, 1
		if g.degrees != nil {
			deg[m] = g.degrees[i]
		}
	}
	return clustering{blockToCluster: assign, intraDegree: deg, clusterCount: 1}
}

func scopesOf(g w3Group, scope string) map[string]string {
	s := map[string]string{}
	for _, m := range g.members {
		s[m] = scope
	}
	return s
}

//go:build integration

// W6 DB gates — selection, the three storm layers, the two rejection classes
// and the credentials opt-in (design/01 §7 W6, Amendments A01-3 / A01-4).
//
// The fixtures write graph_cluster_topic / graph_cluster_node directly instead
// of driving a rebuild: what is under test is the LABEL pipeline, and building
// a partition through Louvain to reach it would make every gate depend on the
// identity layer it is not testing. The model is a fake with a CALL COUNTER —
// the only way a gate can assert "exactly N calls" instead of "a label
// appeared", which is the difference between the manual filter working and
// looking like it works.
package topiclabel

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
)

var labelTypes = []string{"knowledge"}

func w6ID(n int) string { return fmt.Sprintf("019e1111-0000-7000-9000-%012d", n) }

// w6Block inserts one block and returns its id.
func w6Block(t *testing.T, pool *pgxpool.Pool, scope, title, sens string, n int, tags ...string) string {
	t.Helper()
	id := w6ID(n)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO context_blocks (id, scope, category, title, content, sensitivity, tags)
		VALUES ($1::uuid, $2, 'learnings', $3, 'w6 fixture', $4, $5::text[])`,
		id, scope, title, sens, tags); err != nil {
		t.Fatalf("w6Block(%s): %v", title, err)
	}
	return id
}

// w6Topic creates one living topic with a fallback label and its node row.
func w6Topic(t *testing.T, pool *pgxpool.Pool, scope string, core []string) string {
	t.Helper()
	var topic string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO graph_cluster_topic (scope, label, label_source, label_built_at, label_stale, core_blocks)
		VALUES ($1, 'fallback name', 'fallback', now(), true, $2::uuid[])
		RETURNING topic_id::text`, scope, core).Scan(&topic); err != nil {
		t.Fatalf("w6Topic(%s): %v", scope, err)
	}
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph_cluster_node (cluster_id, scope, size, category_counts, repr_block_id,
		                                repr_title, repr_quality, topic_id, core_hash, core_blocks)
		VALUES ($1::uuid, $2, $3, '{"learnings": 1}'::jsonb, $1::uuid, 'repr', 1.0, $4::uuid, $5, $6::uuid[])`,
		core[0], scope, len(core), topic, "hash-"+topic[:8], core); err != nil {
		t.Fatalf("w6Topic node(%s): %v", scope, err)
	}
	return topic
}

// w6State reads the label state of one topic.
type w6Row struct {
	label    string
	source   string
	stale    bool
	attempts int
	model    string
	anchor   string
}

func w6State(t *testing.T, pool *pgxpool.Pool, topic string) w6Row {
	t.Helper()
	var r w6Row
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(label,''), label_source, label_stale, label_attempts,
		       COALESCE(label_model,''), COALESCE(label_core_hash,'')
		  FROM graph_cluster_topic WHERE topic_id = $1::uuid`, topic).
		Scan(&r.label, &r.source, &r.stale, &r.attempts, &r.model, &r.anchor); err != nil {
		t.Fatalf("w6State(%s): %v", topic, err)
	}
	return r
}

// fakeChat is the dispatch seam with a call counter and a prompt recorder.
type fakeChat struct {
	calls    int
	users    []string
	required []backends.Sensitivity
	// answer produces the raw model reply for call i (0-based). nil → a valid
	// generic label.
	answer func(i int) (string, error)
	// before runs at the start of every call — the hook the tombstone race gate
	// uses to retire the topic between selection and write.
	before func()
}

func (f *fakeChat) fn(_ context.Context, _ Deps, required backends.Sensitivity, _, user string, _ []string) (string, string, error) {
	if f.before != nil {
		f.before()
	}
	i := f.calls
	f.calls++
	f.users = append(f.users, user)
	f.required = append(f.required, required)
	if f.answer != nil {
		a, err := f.answer(i)
		return a, "fake-model", err
	}
	return fmt.Sprintf(`{"label":"Thema %d"}`, i), "fake-model", nil
}

// w6Deps builds a run context with a digest-capable pool and the fake seam.
func w6Deps(pool *pgxpool.Pool, chat *fakeChat, mutate func(*Deps)) Deps {
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "1", Name: "local", Trust: backends.TrustFull, Locality: "lan",
			Roles: []string{backends.RoleDigest}, Priority: 100, Enabled: true},
		{ID: "2", Name: "cloud", Trust: backends.TrustNoCredentials, Locality: "external",
			Roles: []string{backends.RoleDigest}, Priority: 50, Enabled: true},
	})
	d := Deps{
		Pool:     pool,
		Backends: bp,
		Chat:     chat.fn,
		Cfg: Config{
			Enabled: true, Batch: 100, MinTopics: 1, PromptMaxTitles: 24,
			Interval: time.Hour, VisibleTypes: labelTypes,
		},
	}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

// ─────────────────────────────────────────────────────────────────────────────

func TestW6LabelPipeline(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// ── The happy path, and the provenance that goes with it. ───────────────
	t.Run("a stale topic gets a name, a model and a fresh drift anchor", func(t *testing.T) {
		const scope = "w6ok"
		core := []string{w6Block(t, pool, scope, "RRF fusion tuning", "internal", 1, "retrieval")}
		topic := w6Topic(t, pool, scope, core)

		chat := &fakeChat{}
		st := Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if st.State != StateActive || st.Labeled != 1 || chat.calls != 1 {
			t.Fatalf("state=%s labeled=%d calls=%d, want active/1/1", st.State, st.Labeled, chat.calls)
		}
		row := w6State(t, pool, topic)
		if row.label != "Thema 0" || row.source != "llm" || row.stale || row.attempts != 0 {
			t.Fatalf("row = %+v", row)
		}
		if row.model != "fake-model" {
			t.Fatalf("label_model = %q — provenance must name the model that answered", row.model)
		}
		if row.anchor != "hash-"+topic[:8] {
			t.Fatalf("drift anchor = %q, want this run's core_hash", row.anchor)
		}
	})

	// ── G1: the drift gate. A labelled topic whose core did not move is not
	// selected — that is what makes rim churn free.
	t.Run("G1 an unstale llm topic is not selected", func(t *testing.T) {
		const scope = "w6drift"
		core := []string{w6Block(t, pool, scope, "stable core", "internal", 10)}
		topic := w6Topic(t, pool, scope, core)
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label='named', label_source='llm', label_stale=false
		                    WHERE topic_id=$1::uuid`, topic)

		chat := &fakeChat{}
		if st := Run(ctx, w6Deps(pool, chat, nil), []string{scope}); chat.calls != 0 {
			t.Fatalf("calls=%d selected=%d, want 0 — an unchanged core must cost nothing", chat.calls, st.Selected)
		}
		// Flip the flag the way the W5 pass does on a drifted core.
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label_stale=true WHERE topic_id=$1::uuid`, topic)
		if st := Run(ctx, w6Deps(pool, chat, nil), []string{scope}); chat.calls != 1 || st.Labeled != 1 {
			t.Fatalf("after drift: calls=%d labeled=%d, want 1/1", chat.calls, st.Labeled)
		}
	})

	// RED PROBE for G1: a selection whose drift condition is a constant fires
	// on every tick — the "compare the member count instead of the core hash"
	// class of defect, in its purest form.
	t.Run("G1 red probe — a selection without the drift condition re-labels forever", func(t *testing.T) {
		const scope = "w6driftred"
		core := []string{w6Block(t, pool, scope, "stable core red", "internal", 20)}
		topic := w6Topic(t, pool, scope, core)
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label='named', label_source='llm', label_stale=false
		                    WHERE topic_id=$1::uuid`, topic)

		restore := selectSQL
		selectSQL = strings.Replace(selectSQL, "AND t.label_stale", "", 1)
		defer func() { selectSQL = restore }()
		if selectSQL == restore {
			t.Fatal("red probe did not patch the selection")
		}

		chat := &fakeChat{}
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if chat.calls == 0 {
			t.Fatal("red probe stayed green — the drift condition is not what suppresses the call")
		}
	})

	// ── G2: the sensitivity fold. A credentials core folds the requirement to
	// credentials, and the trust matrix then removes the external backend from
	// the chain. Nothing here is a new mechanism — the gate proves the fold is
	// actually performed instead of a constant being passed.
	t.Run("G2 a credentials core folds to credentials and drops the external backend", func(t *testing.T) {
		const scope = "w6sens"
		core := []string{
			w6Block(t, pool, scope, "public note", "public", 30),
			w6Block(t, pool, scope, "vault dump", "credentials", 31),
		}
		w6Topic(t, pool, scope, core)

		chat := &fakeChat{}
		d := w6Deps(pool, chat, nil)
		Run(ctx, d, []string{scope})
		if len(chat.required) != 1 || chat.required[0] != backends.SensCredentials {
			t.Fatalf("required = %v, want [credentials] (MaxSensitivity over the core)", chat.required)
		}
		chain, err := d.Backends.Chain(backends.RoleDigest, chat.required[0], "")
		if err != nil {
			t.Fatalf("chain: %v", err)
		}
		for _, b := range chain {
			if b.Name == "cloud" {
				t.Fatal("the no-credentials external backend stayed in the chain")
			}
		}
		// RED DIRECTION: a hard-coded SensInternal would keep it in.
		loose, err := d.Backends.Chain(backends.RoleDigest, backends.SensInternal, "")
		if err != nil || len(loose) != 2 {
			t.Fatalf("SensInternal chain = %v (%v) — the fixture cannot show the difference", loose, err)
		}
	})

	// ── G4: scope purity of the PROMPT. The fixture forces the situation the
	// construction forbids — a core_blocks entry pointing at a foreign scope —
	// so the query's own predicate is what has to hold.
	t.Run("G4 a foreign-scope core block never reaches the prompt", func(t *testing.T) {
		const scope = "w6scopeA"
		own := w6Block(t, pool, scope, "own title A", "internal", 40)
		foreign := w6Block(t, pool, "w6scopeB", "FOREIGN SECRET TITLE", "internal", 41)
		w6Topic(t, pool, scope, []string{own, foreign})

		chat := &fakeChat{}
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if len(chat.users) != 1 {
			t.Fatalf("calls = %d, want 1", len(chat.users))
		}
		if strings.Contains(chat.users[0], "FOREIGN SECRET TITLE") {
			t.Fatal("a foreign-scope title reached the prompt")
		}
	})

	// RED PROBE for G4: strip the b.scope predicate.
	t.Run("G4 red probe — without b.scope the foreign title is in the prompt", func(t *testing.T) {
		const scope = "w6scopeAred"
		own := w6Block(t, pool, scope, "own title A red", "internal", 50)
		foreign := w6Block(t, pool, "w6scopeBred", "FOREIGN SECRET TITLE RED", "internal", 51)
		w6Topic(t, pool, scope, []string{own, foreign})

		restore := coreTitlesSQL
		// The predicate is disabled, not deleted: $2 has to stay referenced or
		// PostgreSQL cannot infer its type and the probe would fail for the
		// wrong reason.
		coreTitlesSQL = strings.Replace(coreTitlesSQL, "b.scope = $2", "(b.scope = $2 OR TRUE)", 1)
		defer func() { coreTitlesSQL = restore }()
		if coreTitlesSQL == restore {
			t.Fatal("red probe did not patch the core query")
		}

		chat := &fakeChat{}
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if len(chat.users) != 1 || !strings.Contains(chat.users[0], "FOREIGN SECRET TITLE RED") {
			t.Fatal("red probe stayed green — b.scope is not what keeps the prompt scope-pure")
		}
	})

	// ── G5: no chat-capable backend is a STATE, not an error flood. The
	// deterministic fallback keeps carrying the map.
	t.Run("G5 no digest backend ⇒ no-backend, fallback intact", func(t *testing.T) {
		const scope = "w6nobackend"
		core := []string{w6Block(t, pool, scope, "orphan", "internal", 60)}
		topic := w6Topic(t, pool, scope, core)

		chat := &fakeChat{}
		d := w6Deps(pool, chat, func(d *Deps) {
			bp := backends.NewPool(nil, nil)
			bp.SeedSnapshotForTest([]backends.Backend{
				{ID: "9", Name: "embedder", Trust: backends.TrustFull,
					Roles: []string{backends.RoleEmbed}, Priority: 1, Enabled: true},
			})
			d.Backends = bp
		})
		st := Run(ctx, d, []string{scope})
		if st.State != StateNoBackend || chat.calls != 0 {
			t.Fatalf("state=%s calls=%d, want no-backend/0", st.State, chat.calls)
		}
		if row := w6State(t, pool, topic); row.source != "fallback" || row.label != "fallback name" {
			t.Fatalf("the fallback did not survive: %+v", row)
		}
	})

	// ── G7: three strikes and the topic leaves the selection — until its core
	// drifts, which is the W5 pass's job to signal.
	t.Run("G7 the attempt cap takes a topic out and drift puts it back", func(t *testing.T) {
		const scope = "w6attempts"
		core := []string{w6Block(t, pool, scope, "unnameable", "internal", 70)}
		topic := w6Topic(t, pool, scope, core)

		chat := &fakeChat{answer: func(int) (string, error) { return "not json at all", nil }}
		d := w6Deps(pool, chat, nil)
		for i := 0; i < 4; i++ {
			Run(ctx, d, []string{scope})
		}
		if chat.calls != maxAttempts {
			t.Fatalf("calls=%d, want exactly %d — the cap must stop the fourth", chat.calls, maxAttempts)
		}
		if row := w6State(t, pool, topic); row.attempts != maxAttempts || row.source != "fallback" {
			t.Fatalf("row = %+v", row)
		}
		// The W5 pass resets the counter on a drifted core; the topic returns.
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label_attempts=0, label_stale=true
		                    WHERE topic_id=$1::uuid`, topic)
		Run(ctx, d, []string{scope})
		if chat.calls != maxAttempts+1 {
			t.Fatalf("after reset: calls=%d, want %d", chat.calls, maxAttempts+1)
		}
	})

	// ── G8: manual wins, and it wins in the SELECTION. Filtering it only on
	// write means a pinned topic burns a full model call every interval and
	// writes nothing — unbounded, because a SUCCESSFUL call does not raise the
	// attempt counter.
	t.Run("G8 a pinned topic costs zero calls", func(t *testing.T) {
		const scope = "w6manual"
		core := []string{w6Block(t, pool, scope, "pinned", "internal", 80)}
		topic := w6Topic(t, pool, scope, core)
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label='human name', label_source='manual', label_stale=true
		                    WHERE topic_id=$1::uuid`, topic)

		chat := &fakeChat{}
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if chat.calls != 0 {
			t.Fatalf("calls=%d, want 0 — a pinned topic must never be selected", chat.calls)
		}
		if row := w6State(t, pool, topic); row.label != "human name" || row.source != "manual" {
			t.Fatalf("row = %+v", row)
		}
	})

	// RED PROBE for G8: the Revision-1 shape — manual filtered on write only.
	t.Run("G8 red probe — manual only on write ⇒ a call per tick, no row", func(t *testing.T) {
		const scope = "w6manualred"
		core := []string{w6Block(t, pool, scope, "pinned red", "internal", 90)}
		topic := w6Topic(t, pool, scope, core)
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label='human name', label_source='manual', label_stale=true
		                    WHERE topic_id=$1::uuid`, topic)

		restore := selectSQL
		selectSQL = strings.Replace(selectSQL, "AND t.label_source <> 'manual'", "", 1)
		defer func() { selectSQL = restore }()
		if selectSQL == restore {
			t.Fatal("red probe did not patch the selection")
		}

		chat := &fakeChat{}
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if chat.calls != 2 {
			t.Fatalf("red probe: calls=%d, want 2 — the defect is the unbounded call, not a wrong write", chat.calls)
		}
		if row := w6State(t, pool, topic); row.source != "manual" || row.attempts != 0 {
			t.Fatalf("the write guard held (correctly) but the calls were spent: %+v", row)
		}
	})

	// ── G8b: the arm runs WITHOUT the persist advisory lock, so a rebuild can
	// retire a topic between its selection and its write.
	t.Run("G8b a name never lands on a tombstone", func(t *testing.T) {
		const scope = "w6tomb"
		core := []string{w6Block(t, pool, scope, "about to die", "internal", 100)}
		topic := w6Topic(t, pool, scope, core)

		chat := &fakeChat{before: func() {
			mustExec(t, pool, `UPDATE graph_cluster_topic SET retired_at=now() WHERE topic_id=$1::uuid`, topic)
		}}
		Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if chat.calls != 1 {
			t.Fatalf("calls=%d, want 1 (the race needs the call to happen)", chat.calls)
		}
		if row := w6State(t, pool, topic); row.source == "llm" {
			t.Fatalf("the label landed on a tombstone: %+v", row)
		}
	})

	// ── G11: the batch cap is a TICK cap across all scopes of the tenant, not
	// a per-scope cap. With S scopes a per-scope limit would be S × batch.
	t.Run("G11 the cap counts per tick, not per scope", func(t *testing.T) {
		scopes := []string{"w6capA", "w6capB", "w6capC"}
		n := 200
		for _, s := range scopes {
			for i := 0; i < 4; i++ {
				n++
				w6Topic(t, pool, s, []string{w6Block(t, pool, s, fmt.Sprintf("cap %d", n), "internal", n)})
			}
		}
		chat := &fakeChat{}
		st := Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Cfg.Batch = 4 }), scopes)
		if chat.calls != 4 || st.Selected != 4 {
			t.Fatalf("calls=%d selected=%d, want 4/4 across %d scopes", chat.calls, st.Selected, len(scopes))
		}
	})

	// RED PROBE for G11: one limit per scope branch.
	t.Run("G11 red probe — a per-scope limit ignores the cap", func(t *testing.T) {
		scopes := []string{"w6capredA", "w6capredB"}
		n := 300
		for _, s := range scopes {
			for i := 0; i < 3; i++ {
				n++
				w6Topic(t, pool, s, []string{w6Block(t, pool, s, fmt.Sprintf("capred %d", n), "internal", n)})
			}
		}
		restore := selectSQL
		selectSQL = `
SELECT x.topic_id, x.scope, x.core_hash, x.core_blocks, x.size, x.category_counts FROM (
  SELECT t.topic_id::text, t.scope, n.core_hash, n.core_blocks::text[], n.size, n.category_counts,
         row_number() OVER (PARTITION BY t.scope ORDER BY n.size DESC, t.topic_id) AS rn
    FROM graph_cluster_topic t
    JOIN graph_cluster_node  n ON n.topic_id = t.topic_id AND n.scope = t.scope
   WHERE t.retired_at IS NULL AND t.label_source <> 'manual'
     AND t.scope = ANY($1::text[]) AND t.label_attempts < $2
     AND t.label_stale
) x WHERE x.rn <= $3`
		defer func() { selectSQL = restore }()

		chat := &fakeChat{}
		Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Cfg.Batch = 3 }), scopes)
		if chat.calls != 6 {
			t.Fatalf("red probe: calls=%d, want 6 (3 per scope × 2) — the probe must reproduce the defect", chat.calls)
		}
	})

	// ── A01-4: the complexity threshold. Default-on is only safe because THIS
	// keeps a small corpus quiet — and the state has to say so, because a
	// pipeline doing nothing produces no log lines.
	t.Run("A01-4 below the threshold nothing is labelled, and the state says why", func(t *testing.T) {
		const scope = "w6thresh"
		for i := 400; i < 403; i++ {
			w6Topic(t, pool, scope, []string{w6Block(t, pool, scope, fmt.Sprintf("t %d", i), "internal", i)})
		}
		chat := &fakeChat{}
		st := Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Cfg.MinTopics = 10 }), []string{scope})
		if chat.calls != 0 {
			t.Fatalf("calls=%d, want 0 below the threshold", chat.calls)
		}
		if st.State != "below-threshold (3/10)" {
			t.Fatalf("state = %q, want %q", st.State, "below-threshold (3/10)")
		}
		// Crossing the threshold runs exactly the N topics that are there —
		// not a backlog storm, because there is no backlog.
		st = Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Cfg.MinTopics = 3 }), []string{scope})
		if chat.calls != 3 || st.State != StateActive {
			t.Fatalf("at the threshold: calls=%d state=%s, want 3/active", chat.calls, st.State)
		}
	})

	// ── A01-3 stage 1: a secret that survived into the name is discarded, and
	// the counter is visible. The map keeps its deterministic name.
	t.Run("A01-3 a label carrying a secret is discarded and counted", func(t *testing.T) {
		const scope = "w6scan"
		core := []string{w6Block(t, pool, scope, "deploy notes", "internal", 500)}
		topic := w6Topic(t, pool, scope, core)

		secret := "AKIA" + strings.Repeat("Z", 16)
		chat := &fakeChat{answer: func(int) (string, error) {
			return `{"label":"Key ` + secret + `"}`, nil
		}}
		st := Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if st.RejectedScan != 1 || st.Labeled != 0 {
			t.Fatalf("rejected_scan=%d labeled=%d, want 1/0", st.RejectedScan, st.Labeled)
		}
		row := w6State(t, pool, topic)
		if row.source != "fallback" || row.label != "fallback name" || row.attempts != 1 {
			t.Fatalf("row = %+v — the fallback must stand and the attempt must count", row)
		}
	})

	// ── A01-3 stage 2: the echo gate, on a real credentials core title.
	t.Run("A01-3 an echo of a credentials title is discarded and counted", func(t *testing.T) {
		const scope = "w6echo"
		core := []string{
			w6Block(t, pool, scope, "Rotation der Hetzner-Storagebox Zugangsdaten", "credentials", 510),
			w6Block(t, pool, scope, "Backup-Zeitplan", "internal", 511),
		}
		topic := w6Topic(t, pool, scope, core)

		chat := &fakeChat{answer: func(int) (string, error) {
			return `{"label":"Hetzner-Storagebox Zugangsdaten"}`, nil
		}}
		st := Run(ctx, w6Deps(pool, chat, nil), []string{scope})
		if st.RejectedEcho != 1 || st.Labeled != 0 {
			t.Fatalf("rejected_echo=%d labeled=%d, want 1/0", st.RejectedEcho, st.Labeled)
		}
		if row := w6State(t, pool, topic); row.source != "fallback" {
			t.Fatalf("row = %+v", row)
		}
	})

	// ── A01-3 stage 3: the opt-in. It must QUIESCE, not merely skip — a skip
	// without quiescing re-selects the topic every interval and eats batch
	// slots, which is the exact defect the manual filter was moved for.
	t.Run("A01-3 stage 3 opt-in keeps a credentials core out of the model path", func(t *testing.T) {
		const scope = "w6credsonly"
		core := []string{w6Block(t, pool, scope, "vault export", "credentials", 520)}
		topic := w6Topic(t, pool, scope, core)

		chat := &fakeChat{}
		d := w6Deps(pool, chat, func(d *Deps) { d.Cfg.CredentialsFallbackOnly = true })
		st := Run(ctx, d, []string{scope})
		if chat.calls != 0 || st.Quiesced != 1 {
			t.Fatalf("calls=%d quiesced=%d, want 0/1", chat.calls, st.Quiesced)
		}
		row := w6State(t, pool, topic)
		if row.stale || row.anchor == "" || row.source != "fallback" {
			t.Fatalf("row = %+v — the topic must leave the selection with its fallback name", row)
		}
		// Second tick: nothing left to do, no slot consumed.
		if st2 := Run(ctx, d, []string{scope}); st2.Selected != 0 {
			t.Fatalf("second tick selected %d — the quiesce did not hold", st2.Selected)
		}
		// The knob is off by default: the same core then DOES reach the model.
		mustExec(t, pool, `UPDATE graph_cluster_topic SET label_stale=true WHERE topic_id=$1::uuid`, topic)
		if st3 := Run(ctx, w6Deps(pool, chat, nil), []string{scope}); st3.Labeled != 1 {
			t.Fatalf("with the knob off: labeled=%d, want 1", st3.Labeled)
		}
	})

	// ── G9: the in-loop demand yield ends the BATCH, and it is counted. The
	// rebuild arm's pre-run check would have let all of these through.
	t.Run("G9 interactive demand ends the batch and is counted", func(t *testing.T) {
		const scope = "w6yield"
		for i := 600; i < 604; i++ {
			w6Topic(t, pool, scope, []string{w6Block(t, pool, scope, fmt.Sprintf("y %d", i), "internal", i)})
		}
		chat := &fakeChat{}
		st := Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Demand = func() int { return 1 } }), []string{scope})
		if chat.calls != 0 || st.Yielded != 1 || st.Selected != 4 {
			t.Fatalf("calls=%d yielded=%d selected=%d, want 0/1/4", chat.calls, st.Yielded, st.Selected)
		}
		// The topics stay selectable — the batch ended, the tick did not
		// consume them.
		if st2 := Run(ctx, w6Deps(pool, chat, nil), []string{scope}); st2.Labeled != 4 {
			t.Fatalf("after the yield: labeled=%d, want 4", st2.Labeled)
		}
		if st2 := Run(ctx, w6Deps(pool, chat, nil), []string{scope}); st2.LatencyP95Ms < 0 {
			t.Fatal("latency percentiles must be reported")
		}
	})

	// ── The time brake: a tick never outlasts its interval, as a property of
	// the code rather than an assumption about model latency.
	t.Run("G9b the tick ends at label_interval and is counted", func(t *testing.T) {
		const scope = "w6overrun"
		for i := 700; i < 703; i++ {
			w6Topic(t, pool, scope, []string{w6Block(t, pool, scope, fmt.Sprintf("o %d", i), "internal", i)})
		}
		chat := &fakeChat{answer: func(i int) (string, error) {
			time.Sleep(20 * time.Millisecond)
			return fmt.Sprintf(`{"label":"O %d"}`, i), nil
		}}
		st := Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Cfg.Interval = 10 * time.Millisecond }), []string{scope})
		if st.Overrun != 1 {
			t.Fatalf("overrun=%d labeled=%d, want the batch to stop at the interval", st.Overrun, st.Labeled)
		}
		if st.Labeled == st.Selected {
			t.Fatalf("the whole batch ran despite the time brake (%d/%d)", st.Labeled, st.Selected)
		}
	})

	// ── The off switch stays a hard opt-out.
	t.Run("disabled ⇒ off, zero calls", func(t *testing.T) {
		const scope = "w6off"
		w6Topic(t, pool, scope, []string{w6Block(t, pool, scope, "quiet", "internal", 800)})
		chat := &fakeChat{}
		st := Run(ctx, w6Deps(pool, chat, func(d *Deps) { d.Cfg.Enabled = false }), []string{scope})
		if st.State != StateOff || chat.calls != 0 {
			t.Fatalf("state=%s calls=%d, want off/0", st.State, chat.calls)
		}
	})
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// Package topiclabel is the LLM half of the cluster-topic naming — wave W6 of
// the Cluster-Topic-Map (design/01 §4.8, Amendments A01-3 / A01-4, decisions
// E3-01 / E4-02 / E5-01 / E7-01).
//
// WHY IT IS A PACKAGE OF ITS OWN, DRIVEN FROM THE PARENT PROCESS. The rebuild
// runs in a worker CHILD that builds nothing but a database pool — no backend
// pool, no admitter, no settings store. An LLM call from there is not
// "difficult", it is unwired. Two more reasons make that the better cut anyway:
// a model call inside the persist transaction would hold the advisory lock for
// minutes and turn a labelling timeout into an identity rollback, and the two
// cadences belong apart (a partition is rebuilt every 6 h, a name only has to
// follow a core drift).
//
// WHAT IT IS NOT ALLOWED TO BE: load-bearing. Every topic already has a
// deterministic name from W5, written in the same transaction as its identity.
// This package IMPROVES that name. Every failure path therefore ends in "the
// fallback stays", never in "the map has a gap": no backend, no eligible chain,
// an unparsable answer, a sensitivity hit, an echo — all of them leave the
// existing label untouched.
//
// THE THREE-LAYER STORM GUARD (design/01 §5.3-B8), in wirkrichtung order:
//
//  1. label_batch caps the LLM calls ONE tick can start — one cap across all
//     scopes of the tenant that tick serves, never one per scope;
//  2. the demand yield sits INSIDE the batch loop, not before it. The rebuild
//     arm's pre-run yield is right for ONE uninterruptible gonum call and
//     useless for 200 sequential model calls: with a pre-check only, the whole
//     batch runs through even when interactive load arrives at call two;
//  3. the background lease preempt is the LAST layer, not the first — it aborts
//     running attempts and therefore spends GPU time without a result.
//
// The counters that make all of that measurable rather than claimed ride out in
// Stats and into the arm log: labeled/failed/yielded/overrun/aborted, the two
// rejection counters, and the p50/p95 call latency.
package topiclabel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/visibility"
)

// maxAttempts is the confidence-free abort condition: after this many failed
// attempts a topic drops out of the selection until its core drifts, at which
// point the W5 pass resets the counter because a changed core is a new input.
//
// A CODE CONSTANT, deliberately not a config key: it is not an operating knob
// but the termination condition of a loop.
const maxAttempts = 3

// callTimeout is the DEFAULT budget of one label call, overridable through
// graph_overview.label_timeout (Config.CallTimeout). A name is two to five
// words; a chain that needs longer than this is queueing, not thinking, and the
// batch has 199 more topics to get to.
//
// The budget is TOTAL — it starts before the dispatch admission queue, not at
// the wire — which is why it needs to be configurable at all: on a saturated
// pool a label can spend all of it waiting for a slot and abort with
// `acquire_expired` without ever reaching a model (issue #37).
const callTimeout = 90 * time.Second

// callTimeoutOf resolves the effective budget: a configured
// graph_overview.label_timeout wins, anything non-positive falls back to the
// package default. Non-positive rather than zero-only because the resolver is
// the LAST line of defence — config's V17 already rejects a negative duration
// at boot and on the settings write, and an unwired caller (tests, a future
// embedder) must not end up with a deadline in the past.
func callTimeoutOf(c Config) time.Duration {
	if c.CallTimeout > 0 {
		return c.CallTimeout
	}
	return callTimeout
}

// Pipeline is the llmlog channel name of this arm.
const Pipeline = "cluster-label"

// Labelling states, as /api/status renders them. Every state is explicit —
// design/01's W16 condition is that a pipeline which is doing nothing says WHY,
// because "off" and "not complex enough yet" and "no backend" are three
// different operational situations with three different answers.
const (
	StateOff            = "off"
	StateNoBackend      = "no-backend"
	StateBelowThreshold = "below-threshold"
	StateActive         = "active"
)

// Config is the resolved policy of one run. Passed as values rather than as a
// config snapshot: internal/config is layered to cmd/handler/events/settings
// only, and a background package reading it directly would break that.
type Config struct {
	Enabled         bool
	Batch           int
	MinTopics       int
	PromptMaxTitles int
	Interval        time.Duration
	// CallTimeout is the resolved graph_overview.label_timeout — the TOTAL
	// budget of one label, admission queue wait included. 0 = the package
	// default (tests, an unwired caller).
	CallTimeout             time.Duration
	CredentialsFallbackOnly bool
	Language                string
	// VisibleTypes is the resolved retrieval allowlist. EMPTY is a wiring bug,
	// not an empty corpus: visibility.TypeVisible matches zero rows on an empty
	// array, so the run would silently label nothing.
	VisibleTypes []string
}

// Deps are the run's collaborators. Every one of them is nil-tolerant in the
// direction of "do nothing", never in the direction of "panic in a background
// goroutine".
type Deps struct {
	Pool     *pgxpool.Pool
	Backends *backends.Pool
	Adm      llm.Admission
	Cfg      Config
	// Demand reports the process-wide interactive demand. nil = never yield
	// (tests, an unwired dispatcher).
	Demand func() int
	// Floor raises a required sensitivity to a scope's floor (raise-only). nil
	// = no floor configured.
	Floor func(backends.Sensitivity, string) backends.Sensitivity
	// Chat is the dispatch seam. Production is chainCall; tests substitute a
	// fake backend with a call counter, which is the only way the gates can
	// assert "exactly N calls" instead of "a label appeared".
	Chat ChatFunc
}

// ChatFunc is one label call: system + user prompt in, the raw model answer and
// the serving model's name out.
type ChatFunc func(ctx context.Context, d Deps, required backends.Sensitivity, system, user string, blockIDs []string) (answer, model string, err error)

// Stats is one tick's ledger.
type Stats struct {
	// State is the rendered labelling state, e.g. "active" or
	// "below-threshold (3/10)".
	State string
	// LivingTopics is the largest living-topic count among the scopes of this
	// tick — the number that says how far the corpus is from the threshold.
	LivingTopics int
	MinTopics    int

	Selected int
	Labeled  int
	Failed   int
	// Quiesced counts topics the credentials opt-in took out of the LLM path.
	Quiesced      int
	RejectedScan  int
	RejectedEcho  int
	RejectedShape int

	Yielded int // batch ended early: interactive demand arrived
	Overrun int // batch ended early: the tick reached label_interval
	Aborted int // a running attempt was preempted or the context died

	LatencyP50Ms int64
	LatencyP95Ms int64
}

// candidate is one selected topic plus everything the prompt needs.
type candidate struct {
	topicID    string
	scope      string
	coreHash   string
	coreBlocks []string
	size       int
	categories map[string]int
}

// selectSQL is the ONE selection statement per tick, across all eligible scopes
// of the tenant, with ONE limit (design/01 §3.5).
//
// Fully index-addressable over idx_gct_label_pending (scope, label_stale,
// label_attempts) WHERE retired_at IS NULL AND label_source <> 'manual'. The
// drift condition is the MATERIALIZED label_stale column, never the two-table
// comparison against graph_cluster_node.core_hash — that predicate cannot use
// an index and would force a sequential scan over a table in which tombstones
// outnumber living topics, once per interval and per tenant.
//
// label_source <> 'manual' sits in the SELECTION, not only in the success
// write. Filtering it only on write means a pinned topic with a drifting core
// is selected every interval, spends a full model call, writes zero rows, and
// comes back next interval — unbounded, because label_attempts does not grow on
// a SUCCESSFUL call. At scale the pinned topics would rotate through the batch
// cap against the topics that actually need a name.
//
// ORDER BY n.size DESC is the cold-start priority: the big topics — the ones at
// the top of the map — get their name first.
//
// NAMED DEVIATION from design/01 §4.8, which writes the drift condition as
// `label_source IN ('none','fallback') OR label_stale`. The OR arm is redundant
// against the W5 materialization and actively harmful here:
//
//   - redundant, because W5 sets label_stale = (label_core_hash IS DISTINCT
//     FROM core_hash) for EVERY topic in the run, and a topic that was never
//     LLM-labelled has label_core_hash NULL against a NOT NULL core_hash — so
//     it is stale by construction. The column DEFAULT is true, which covers the
//     state before the first rebuild too;
//   - harmful, because with the OR arm no state exists in which a topic with a
//     'fallback' label is out of the selection. The credentials opt-in (A01-3
//     stage 3) needs exactly that state: it decides that THIS core generation
//     gets no model call, and without a way to record the decision the topic is
//     re-selected every interval and eats a slot of the batch cap — the very
//     defect the manual filter was moved into the selection for.
//
// Declared as a VAR so the gates can substitute the defective shapes this
// comment argues against — a missing drift condition, a per-scope limit, a
// missing manual filter — and prove each one red. Production never writes to it.
var selectSQL = `
SELECT t.topic_id::text, t.scope, n.core_hash, n.core_blocks::text[], n.size, n.category_counts
  FROM graph_cluster_topic t
  JOIN graph_cluster_node  n ON n.topic_id = t.topic_id AND n.scope = t.scope
 WHERE t.retired_at IS NULL
   AND t.label_source <> 'manual'
   AND t.scope = ANY($1::text[])
   AND t.label_attempts < $2
   AND t.label_stale
 ORDER BY n.size DESC, t.topic_id
 LIMIT $3`

// livingSQL counts the living topics per scope — the E7-01 complexity gate.
const livingSQL = `
SELECT scope, count(*)::int
  FROM graph_cluster_topic
 WHERE retired_at IS NULL AND scope = ANY($1::text[])
 GROUP BY scope`

// coreTitlesTemplate reads the core blocks of one topic in core order.
//
// b.scope = $2 is redundant to the construction — core_blocks is scope-pure by
// invariant I2 — and stands there anyway: it is the line the scope-purity
// negative probe removes to go red, on the one query in this package that turns
// corpus text into an outbound prompt.
//
// The visibility fragment is the canonical one: a core block that has since
// been archived or whose type left the retrieval cut must not describe the
// cluster to a model.
//
// ORDER BY array_position preserves the stored core order. NAMED DEVIATION from
// Amendment A01-7's "Top-N of the core": the stored array is sorted by ID, not
// by centrality, because core_hash is taken over the SORTED SET so that a pure
// re-ranking inside an unchanged core does not trigger a re-label. The rank is
// therefore not persisted and cannot be recovered here. The cap is honest about
// what it is — a deterministic, stable N OF the core, a declared resource
// bound. It only bites on hub clusters whose substance core exceeds N.
const coreTitlesTemplate = `
SELECT b.id::text, b.title, b.sensitivity, b.tags
  FROM context_blocks b
 WHERE b.id = ANY($1::uuid[]) AND b.scope = $2 AND %s
 ORDER BY array_position($1::uuid[], b.id)
 LIMIT $4`

// coreTitlesSQL is declared as a VAR so the W6 scope-purity gate can strip the
// b.scope predicate and prove the leak. Production never writes to it.
var coreTitlesSQL = fmt.Sprintf(coreTitlesTemplate, visibility.TypeVisible("b", "$3"))

// writeSQL is the success write.
//
// retired_at IS NULL is not decoration: this arm runs in the parent process
// WITHOUT the persist advisory lock, so a rebuild can retire the topic between
// the selection and the write. Without the clause the name would land on a
// tombstone — the call is lost either way, the clause only makes sure it leaves
// no state behind. label_source <> 'manual' is the second half of the E5-01
// pinning rule.
const writeSQL = `
UPDATE graph_cluster_topic
   SET label = $2, label_source = 'llm', label_built_at = now(),
       label_core_hash = $3, label_model = $4, label_attempts = 0, label_stale = false
 WHERE topic_id = $1::uuid AND label_source <> 'manual' AND retired_at IS NULL`

// failSQL records one attempt. Same two guards, same reason.
const failSQL = `
UPDATE graph_cluster_topic
   SET label_attempts = label_attempts + 1
 WHERE topic_id = $1::uuid AND label_source <> 'manual' AND retired_at IS NULL`

// quiesceSQL takes a topic out of the selection WITHOUT naming it: the drift
// anchor is acknowledged while the label and its source stay as they are.
//
// It exists for the credentials opt-in (A01-3 stage 3). Skipping such a topic
// without quiescing it would repeat the very defect the manual filter was moved
// into the selection for: the topic stays stale forever, gets re-selected every
// interval and eats a slot of the batch cap against topics that could actually
// be named. Its deterministic fallback name IS its final name for this core
// generation; when the core drifts, W5 flips label_stale back on.
//
// THE QUIESCE IS STICKY, AND THAT IS A DECISION (K3). Turning
// label_credentials_fallback_only back OFF does not re-arm the topics the knob
// already quiesced — the arm reads a fresh config snapshot every tick and has
// no memory of the previous value, so "the knob just changed" is not a fact it
// can observe. Re-arming automatically would mean selecting on
// `label_source='fallback' AND label_core_hash IS NOT NULL` whenever the knob is
// off, i.e. a second index range next to the stale one: the selection would stop
// being the narrow index scan idx_gct_label_pending was built for, permanently,
// to serve a rare and deliberate transition. Rejected.
//
// The levers that do exist: the topic re-arms by itself on the next core drift,
// or the operator re-arms the whole set in one statement —
//
//	UPDATE graph_cluster_topic SET label_stale = true
//	 WHERE label_source = 'fallback' AND label_core_hash IS NOT NULL
//	   AND retired_at IS NULL;
//
// documented in docs/operations.md next to the knob.
const quiesceSQL = `
UPDATE graph_cluster_topic
   SET label_core_hash = $2, label_stale = false
 WHERE topic_id = $1::uuid AND label_source <> 'manual' AND retired_at IS NULL`

// Run executes ONE label tick over the given scopes (the tenant's owned set).
//
// It never returns an error. A background arm that fails a tick has to log and
// come back next interval — the map is fully labelled the whole time either
// way, and an error return would only invite a caller to treat a naming problem
// as a rebuild problem.
func Run(ctx context.Context, d Deps, scopes []string) Stats {
	st := Stats{State: StateOff, MinTopics: d.Cfg.MinTopics}
	if !d.Cfg.Enabled || len(scopes) == 0 || d.Pool == nil {
		return st
	}
	// Kein-Backend-Guard (A01-4): no chat-capable digest backend means no
	// labelling — a visible state, not a per-topic error flood. The W5 fallback
	// carries the map in the meantime.
	if d.Backends == nil || !d.Backends.RoleConfigured(backends.RoleDigest) {
		st.State = StateNoBackend
		return st
	}

	eligible, largest, err := eligibleScopes(ctx, d, scopes)
	if err != nil {
		slog.Warn("topiclabel: living-topic count failed", "error", err, "scopes", scopes)
		return st
	}
	st.LivingTopics = largest
	if len(eligible) == 0 {
		st.State = fmt.Sprintf("%s (%d/%d)", StateBelowThreshold, largest, d.Cfg.MinTopics)
		return st
	}
	st.State = StateActive

	batch, err := selectCandidates(ctx, d, eligible)
	if err != nil {
		slog.Warn("topiclabel: selection failed", "error", err, "scopes", eligible)
		return st
	}
	st.Selected = len(batch)
	runBatch(ctx, d, batch, &st)
	return st
}

// eligibleScopes applies the E7-01 complexity gate PER SCOPE, and reports the
// largest living-topic count for the status line.
//
// Per scope rather than per tenant: a tenant with one large and one tiny
// partition should get names where they help and silence where they do not.
func eligibleScopes(ctx context.Context, d Deps, scopes []string) (eligible []string, largest int, err error) {
	rows, err := d.Pool.Query(ctx, livingSQL, scopes)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		var n int
		if err := rows.Scan(&scope, &n); err != nil {
			return nil, 0, err
		}
		if n > largest {
			largest = n
		}
		if n >= d.Cfg.MinTopics {
			eligible = append(eligible, scope)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	sort.Strings(eligible) // stable bind order
	return eligible, largest, nil
}

func selectCandidates(ctx context.Context, d Deps, scopes []string) ([]candidate, error) {
	limit := d.Cfg.Batch
	if limit <= 0 {
		limit = 1
	}
	rows, err := d.Pool.Query(ctx, selectSQL, scopes, maxAttempts, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		var cats []byte
		if err := rows.Scan(&c.topicID, &c.scope, &c.coreHash, &c.coreBlocks, &c.size, &cats); err != nil {
			return nil, err
		}
		if len(cats) > 0 {
			_ = json.Unmarshal(cats, &c.categories) // a broken counts blob costs the categories line, not the label
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// runBatch walks the selection with the two in-loop brakes.
//
// Both brakes end the BATCH, not the tick: the remaining topics stay selectable
// and come back next interval. The time brake is what makes "a tick never lasts
// longer than an interval" a property of the code instead of an assumption
// about model latency — the single ungemessene quantity of the whole cost
// calculation (design/01 §6.5).
func runBatch(ctx context.Context, d Deps, batch []candidate, st *Stats) {
	start := time.Now()
	lat := make([]time.Duration, 0, len(batch))
	for _, c := range batch {
		if ctx.Err() != nil {
			st.Aborted++
			break
		}
		if d.Demand != nil && d.Demand() > 0 {
			st.Yielded++
			break
		}
		if d.Cfg.Interval > 0 && time.Since(start) > d.Cfg.Interval {
			st.Overrun++
			break
		}
		took := labelOne(ctx, d, c, st)
		if took > 0 {
			lat = append(lat, took)
		}
	}
	st.LatencyP50Ms, st.LatencyP95Ms = percentiles(lat)
}

// labelOne runs the whole per-topic path and returns the wire latency of the
// model call (zero when no call happened).
func labelOne(ctx context.Context, d Deps, c candidate, st *Stats) time.Duration {
	core, sensitive, required, err := loadCore(ctx, d, c)
	if err != nil {
		slog.Warn("topiclabel: core read failed", "error", err, "topic", c.topicID, "scope", c.scope)
		st.Failed++
		return 0
	}
	if len(core.Titles) == 0 {
		// No readable core block left. MaxSensitivity of an empty list folds to
		// credentials, so the chain would already refuse — quiesce instead of
		// re-selecting this topic every interval.
		st.Quiesced++
		d.exec(ctx, quiesceSQL, c.topicID, c.coreHash)
		return 0
	}
	// A01-3 stage 3, opt-in: a core carrying credentials never reaches a model.
	if d.Cfg.CredentialsFallbackOnly && required == backends.SensCredentials {
		st.Quiesced++
		d.exec(ctx, quiesceSQL, c.topicID, c.coreHash)
		return 0
	}

	nonce := promptguard.NewNonce() // fresh PER PROMPT — not per run, not per process
	system := systemPromptFor(d.Cfg.Language, nonce)
	user := buildUser(nonce, core)

	callCtx, cancel := context.WithTimeout(ctx, callTimeoutOf(d.Cfg))
	wire := time.Now()
	answer, model, err := d.Chat(callCtx, d, required, system, user, c.coreBlocks)
	took := time.Since(wire)
	cancel()
	if err != nil {
		// K2-4: a PREEMPT is not an attempt. The background lease preempt and a
		// cancelled context both mean "the system took the slot back", not "the
		// model could not name this topic" — counting them against the
		// three-strikes budget would let three busy ticks lock a topic out of
		// the selection until its core happens to drift. The attempt counter
		// exists to stop a topic the model keeps FAILING on; it must not
		// measure how busy the GPU was.
		if llm.IsAdmissionError(err) || errors.Is(err, context.Canceled) {
			st.Aborted++
			slog.Warn("topiclabel: label call preempted", "error", err, "topic", c.topicID, "scope", c.scope)
			return took
		}
		st.Failed++
		slog.Warn("topiclabel: label call failed", "error", err, "topic", c.topicID, "scope", c.scope)
		d.exec(ctx, failSQL, c.topicID)
		return took
	}

	label, rej := parseLabel(answer)
	if rej == rejectNone {
		var detail string
		rej, detail = screenLabel(label, newEchoIndex(sensitive))
		if rej != rejectNone {
			// K2-2: NEITHER the label NOR the matched fragment is logged. The
			// scan reports its RULE NAME (a closed, code-owned vocabulary); the
			// echo gate reports a fingerprint — rune length plus a short
			// sha256 prefix — because the fragment it matched is by definition
			// the string suspected of carrying substance out of a
			// credentials-classified title, and a log file is read by a wider
			// audience than the block is.
			if rej == rejectEcho {
				detail = echoFingerprint(detail)
			}
			slog.Warn("topiclabel: label rejected", "reason", string(rej), "detail", detail,
				"topic", c.topicID, "scope", c.scope)
		}
	}
	switch rej {
	case rejectNone:
		st.Labeled++
		d.exec(ctx, writeSQL, c.topicID, label, c.coreHash, model)
	case rejectScan:
		st.RejectedScan++
		st.Failed++
		d.exec(ctx, failSQL, c.topicID)
	case rejectEcho:
		st.RejectedEcho++
		st.Failed++
		d.exec(ctx, failSQL, c.topicID)
	default:
		st.RejectedShape++
		st.Failed++
		d.exec(ctx, failSQL, c.topicID)
	}
	return took
}

// exec runs a one-row bookkeeping update. A failure here is logged and dropped:
// the arm's job is naming, and a lost attempt counter costs one extra try next
// interval — an error return would cost the rest of the batch.
func (d Deps) exec(ctx context.Context, sql string, args ...any) {
	if _, err := d.Pool.Exec(ctx, sql, args...); err != nil {
		slog.Warn("topiclabel: bookkeeping write failed", "error", err)
	}
}

// loadCore reads the core blocks of one topic and folds their sensitivity.
//
// The fold is the B5 egress rule: MaxSensitivity over the core, then the scope
// floor (raise-only). An empty parts list folds to credentials — a topic
// without readable core blocks gets the strictest requirement and therefore, in
// case of doubt, no chain at all. Fail-closed without a special case.
func loadCore(ctx context.Context, d Deps, c candidate) (core promptCore, sensitive []string, required backends.Sensitivity, err error) {
	limit := d.Cfg.PromptMaxTitles
	if limit <= 0 {
		limit = 24
	}
	rows, err := d.Pool.Query(ctx, coreTitlesSQL, c.coreBlocks, c.scope, d.Cfg.VisibleTypes, limit)
	if err != nil {
		return core, nil, backends.SensCredentials, err
	}
	defer rows.Close()

	var parts []backends.Sensitivity
	tagCount := map[string]int{}
	for rows.Next() {
		var id, title, sens string
		var tags []string
		if err := rows.Scan(&id, &title, &sens, &tags); err != nil {
			return core, nil, backends.SensCredentials, err
		}
		core.Titles = append(core.Titles, title)
		parts = append(parts, backends.Sensitivity(sens))
		// The echo gate only indexes the titles that can carry substance.
		if sens == string(backends.SensCredentials) || sens == string(backends.SensPersonal) {
			sensitive = append(sensitive, title)
		}
		for _, tg := range tags {
			if tg = strings.TrimSpace(tg); tg != "" {
				tagCount[tg]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return core, nil, backends.SensCredentials, err
	}

	core.Tags = topN(tagCount, 3)
	core.Categories = topN(c.categories, 3)
	required = backends.MaxSensitivity(parts...)
	if d.Floor != nil {
		required = d.Floor(required, c.scope)
	}
	return core, sensitive, required, nil
}

// topN returns the n most frequent keys, ties broken alphabetically so the
// prompt is byte-stable across runs.
func topN(counts map[string]int, n int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// percentiles returns p50/p95 in milliseconds over the observed wire latencies.
// Nearest-rank, no interpolation: with a batch of at most a few hundred samples
// interpolation would suggest a precision the sample size does not have.
func percentiles(lat []time.Duration) (p50, p95 int64) {
	if len(lat) == 0 {
		return 0, 0
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	at := func(q float64) int64 {
		i := int(q * float64(len(lat)))
		if i >= len(lat) {
			i = len(lat) - 1
		}
		return lat[i].Milliseconds()
	}
	return at(0.50), at(0.95)
}

// ChainCall is the production dispatch seam: the established one-shot helper
// over the digest role, which is already occupied by three backends.
//
// Role digest and not a new role: naming a cluster is the same kind of work as
// the daily report — a short abstraction over corpus text — and a new role
// would need its own backend assignment in every deployment before the feature
// could do anything at all.
func ChainCall(ctx context.Context, d Deps, required backends.Sensitivity, system, user string, blockIDs []string) (string, string, error) {
	var served string
	resp, err := llm.ChainCall{
		Pool:     d.Backends,
		Role:     backends.RoleDigest,
		Required: required,
		Pipeline: Pipeline,
		System:   system,
		User:     user,
		// Temperature near-deterministic (a name is not a creative act) and a
		// tight output budget: the whole answer is one short JSON object, and
		// an unbounded budget on a background arm is GPU time nobody asked for.
		Opts:   llm.Options{Temperature: 0.2, NumPredict: 128},
		Format: "json",
		// The SAME resolved budget as the outer deadline, not the constant:
		// leaving this on the default would re-cap the wire call at 90 s
		// through llm's own WithTimeout (the smaller of the two wins), so a
		// raised graph_overview.label_timeout would buy queue time it could
		// never spend on the model.
		DefTimeout: callTimeoutOf(d.Cfg),
		BlockIDs:   blockIDs,
		// Provenance: label_model has to name the model that ANSWERED, not the
		// one the pool would have picked.
		OnServed: func(_, model string) { served = model },
	}.Do(ctx, d.Pool, d.Adm)
	if err != nil {
		return "", "", err
	}
	if resp == nil {
		return "", "", errors.New("topiclabel: empty chain response")
	}
	if resp.ServedModel != "" {
		served = resp.ServedModel
	}
	return resp.Message.Content, served, nil
}

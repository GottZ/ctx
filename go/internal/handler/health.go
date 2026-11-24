package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/provenance"
	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthHandler provides health check endpoints. Since F3-P2 the
// service-class aggregation feeds from the backend pool (role embed has ≥1
// reachable backend? role synthesis?) instead of the three startup hosts —
// the second deliberate evolution of the health source (K9); the invariant
// stays identical both times: the response shape is host-, name- and
// gaming-free (the route is unauthenticated and public).
type HealthHandler struct {
	pool        *pgxpool.Pool
	cfg         ConfigStore
	backendPool *backends.Pool
	blocktypes  BlocktypeHealth
}

// BlocktypeHealth reports the block-type registry degradation state for the
// /health field blocktype_registry (WF T3, design 01 §4.3): "ok" or
// "builtin-fallback". Implemented by *blocktype.Registry; an interface here
// keeps handler free of the blocktype import (and lets tests fake the
// degraded state without a registry).
type BlocktypeHealth interface {
	Health() string
}

// NewHealthHandler creates a new HealthHandler. blocktypes may be nil
// (tests): the field then reports "ok" — the single-binary boot always
// wires the registry.
func NewHealthHandler(pool *pgxpool.Pool, cfg ConfigStore, backendPool *backends.Pool, blocktypes BlocktypeHealth) *HealthHandler {
	return &HealthHandler{pool: pool, cfg: cfg, backendPool: backendPool, blocktypes: blocktypes}
}

// blocktypeHealthValue maps a possibly-nil BlocktypeHealth to the wire value.
func blocktypeHealthValue(bt BlocktypeHealth) string {
	if bt == nil {
		return "ok"
	}
	return bt.Health()
}

// healthResponse is the public wire shape: role names mapped to ok|error,
// nothing else. The route is unauthenticated and proxied to the public
// internet — host strings, backend names, model names, cooldown detail or
// the gaming flag (a presence oracle) in any field would leak internal
// topology, so the shape stays name-free by invariant (pinned by
// TestHealthShapeInvariant, design §2.9). The per-backend detail lives
// exclusively behind the admin-gated backend-list.
type healthResponse struct {
	Status   string            `json:"status"`
	Services map[string]string `json:"services"`
	// BlocktypeRegistry is "ok" or "builtin-fallback" (WF T3, design 01
	// §4.3 b): the visibility-policy registry failed its DB load and runs on
	// the compiled-in builtin set — loud, because the fallback REVERTS
	// operator visibility narrowings. Not a services entry: services carry
	// ping-based role reachability, this is process-internal degradation
	// state. Name-free like everything else in this body.
	BlocktypeRegistry string `json:"blocktype_registry"`
	// SchemaContract is the schema-contract check's public projection
	// (Evokoa-Clean-Room design/03 §4.6, E-03-4/D03): "ok"|"attention"|"off",
	// name-free — never an object name, hash or drift count. D03 collapses
	// the admin-only drift/unchecked distinction into ONE public value
	// "attention" (a Timing-Orakel-Argument: "the integrity watchdog is
	// currently down" is its own information class an unauthenticated
	// poller must not get for free). The full Report — mode, mode_source,
	// per-object drifts — is admin-only behind GET /api/contract.
	SchemaContract string `json:"schema_contract"`
	// Version names the build that answers (FOLGE-X, LOOP Queue 72):
	// provenance.BuildVersion — the release stamp the image build sets
	// (build-arg CTX_VERSION), the same value the boot line "starting HTTP
	// server" logs; "dev" for an unstamped build, never empty here. Set in
	// HealthStatus, so /health and the status frame's health block keep one
	// shape (G33); omitempty only for the tenant view's deliberately zero
	// health block. Name-free like the rest: a version string, no host.
	Version string `json:"version,omitempty"`
}

// HealthStatus computes the service-class health aggregate from ONE pool
// snapshot: DB ping + per-role reachability (embed, chat=synthesis, optionally
// dream). It is shared by the public /health handler and the admin status
// collector (G33) so the two can never drift — same source, same shape. The
// result is name-free by invariant (TestHealthShapeInvariant); the caller maps
// the status string to an HTTP code.
//
// Status logic (unchanged):
//   - DB down → "unhealthy" (503 at the /health edge)
//   - Embed down → "unhealthy" (embeddings are mandatory for store+search)
//   - Chat down → "degraded" (queries broken, but store works)
//   - Dream down → "ok" (background process, not critical)
//   - Blocktype registry on builtin-fallback → "degraded" (WF T3: reads work,
//     but the visibility policy runs on compiled-in defaults)
//
// The probes run in a round of the process (healthRound, issue #519) over the
// snapshot backendPool holds when the round begins: the database ping and the
// roles concurrently (#265), so the answer takes as long as the slowest role,
// a base URL that several roles share requested once, and the whole round
// bounded by healthProbeBudget. A call that arrives while a round over the
// same inputs runs waits for it; a call whose ctx ends first returns every
// service as "error" and leaves the round to finish and record what it finds.
// backendPool may be nil: no backend, every role without one.
func HealthStatus(ctx context.Context, pool *pgxpool.Pool, backendPool *backends.Pool, dreamEnabled bool, blocktypeRegistry string) healthResponse {
	services := healthLines.observe(ctx, pool, backendPool, dreamEnabled)
	return healthResponse{
		Status:            aggregateHealthStatus(services, blocktypeRegistry),
		Services:          services,
		BlocktypeRegistry: blocktypeRegistry,
		Version:           provenance.BuildVersion(),
	}
}

// aggregateHealthStatus folds the per-service results and the blocktype
// registry state into the overall status (pure — unit-testable without a DB).
func aggregateHealthStatus(services map[string]string, blocktypeRegistry string) string {
	switch {
	case services["database"] != "ok" || services["embed"] != "ok":
		return "unhealthy"
	case services["chat"] != "ok" || blocktypeRegistry != "ok":
		return "degraded"
	default:
		return "ok"
	}
}

// schemaContractHealthValue derives the public /health schema_contract
// value from the process-wide schema-contract report (design/03 §4.6,
// E-03-4/D03). hasReport=false — no check has ever completed in this
// process yet (the boot-time RunCheckSingleFlight call in
// cmd/ctxd/schemaContractBoot always runs before the router accepts
// traffic, so this branch is a startup-window-only theoretical, never a
// steady-state case) — resolves fail-closed to "attention": design/03
// §4.4's last row ("ein Prüfer, der nicht laufen kann, darf nicht 'ok'
// implizieren") applies just as much to "never ran" as to "ran and
// failed". Mode=off takes precedence over Status (an operator's own
// decision, not a degradation, design/03 §4.6) before the ok/attention
// split; D03 collapses drift and unchecked into the single public
// "attention" value (the admin-only distinction lives in Report.Status via
// GET /api/contract).
func schemaContractHealthValue(report schemacontract.Report, hasReport bool) string {
	if !hasReport {
		return "attention"
	}
	if report.Mode == schemacontract.ModeOff {
		return "off"
	}
	if report.Status == schemacontract.StatusOK {
		return "ok"
	}
	return "attention" // drift | unchecked, D03-collapsed
}

// foldSchemaContractStatus folds the schema_contract value into an
// already-computed health aggregate (design/03 §4.6: "attention" ⇒
// mindestens degraded, exactly the blocktype_registry builtin-fallback
// precedence in aggregateHealthStatus above — never downgrades an existing
// "unhealthy"; "off"/"ok" never change the aggregate, design/03 D03:
// "off ⇒ NICHT degraded").
//
// This runs as a SEPARATE post-pass at each of HealthStatus's two call
// sites (Health below, StatusCollector.buildCheap in status.go) rather
// than as a new HealthStatus parameter: K4's status-merge-slot rule for
// this wave is additive-only, no existing signature changes (the
// armRunSource/LastArmRuns trap is the named example, but the rule is
// general — three more status waves land on this same file set after
// W03-4). blocktype_registry threading through HealthStatus's own
// parameter list was an earlier wave's (K9) deliberate choice under a
// different constraint; this wave does not repeat that shape.
func foldSchemaContractStatus(status, schemaContract string) string {
	if status == "unhealthy" {
		return status
	}
	if schemaContract == "attention" {
		return "degraded"
	}
	return status
}

// Health serves the public /health endpoint, wrapping HealthStatus with the
// HTTP status-code mapping (unhealthy → 503, else 200).
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	// The probes do not end with the request (issue #519): they run in a round
	// bounded by healthProbeBudget that the request waits for, and a client
	// that leaves first stops waiting. A round is shared by every request that
	// arrives while it runs, so an anonymous storm holds one probe per base URL
	// and one database ping, not one per request (issue #504).
	cfg := h.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: /health is public/anonymous — no tenant in reach; dream.enabled is server-global.
	resp := HealthStatus(r.Context(), h.pool, h.backendPool, cfg.Dream.Enabled, blocktypeHealthValue(h.blocktypes))

	// Evokoa-Clean-Room design/03 §4.6: fold in the schema-contract report
	// AFTER HealthStatus returns (HealthStatus's own signature stays
	// untouched, see foldSchemaContractStatus's doc).
	contractReport, hasReport := schemacontract.LatestReport()
	resp.SchemaContract = schemaContractHealthValue(contractReport, hasReport)
	resp.Status = foldSchemaContractStatus(resp.Status, resp.SchemaContract)

	statusCode := http.StatusOK
	if resp.Status == "unhealthy" {
		statusCode = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logEncodeError(w, "health check: failed to encode response", "error", err)
	}
}

// healthProbeBudget bounds a probe round (healthRound), for /health and the
// status collector alike — one budget, applied in one place: both feed the
// same healthNotice, and two budgets would judge a backend that answers
// between them reachable in one and unreachable in the other. 4 s is under
// the 5 s timeout of the compose healthcheck (docker-compose.yml), whose
// clock starts before its request: Docker reads the health document — whose
// database verdict alone fails the container — instead of a timeout while a
// backend hangs. It is also under the status collector's tick
// (events.tick_interval, 5 s by default). A variable for tests.
var healthProbeBudget = 4 * time.Second

// roleReachable reports ok when at least one enabled backend of the role
// answers its base URL. This is a REACHABILITY probe, not a model-availability
// check: any HTTP response below 500 counts as reachable — cloud API roots
// answer 404/405 on GET / and auth-gated APIs answer 401/403, all of which
// prove the host is up and serving (see pingHost). Errors log the backend name
// to slog ONLY — the response carries the aggregate — and once per change of
// state (healthNotice, issue #504): a role without an enabled backend, a role
// none of whose candidates answers, a backend that stops answering. seq is the
// round's number (healthNotice.current).
//
// Only operator rows (_global/unscoped) are pinged: /health is anonymous, and a
// tenant row's base_url is tenant-admin input — pinging it would hand every
// anonymous caller a request into wherever a tenant pointed it (GHSA
// 2026-10-31 §3). Server health is the operator's pool; a tenant's rows answer
// for themselves on its own chain calls.
//
// The candidates of one role are probed in their order and none starts once
// one answered (probeInTurn): a role never requests more than its own ordered
// walk, and the bound of a call stays the operator rows of the probed roles.
// Every probe runs on the round's whole budget (issue #519), so a first
// candidate that hangs no longer leaves a later one "probed" with a spent
// budget, and probes only removes repeats — a base URL another role of the
// same round already probed is not requested again; the answer is the one
// this role would have got itself. A probe that hangs is waited for until the
// budget ends, wherever it stands in the order.
func (n *healthNotice) roleReachable(ctx context.Context, seq uint64, snap []backends.Backend, role string, probes *hostProbes) string {
	rows := 0
	var candidates []*backends.Backend
	for i := range snap {
		b := &snap[i]
		if !b.HasRole(role) || b.EgressGuarded() {
			continue
		}
		rows++
		if b.Enabled {
			candidates = append(candidates, b)
		}
	}
	n.roleProbed(seq, role, rows, candidates)
	served, started := probeInTurn(ctx, candidates, probes, func(b *backends.Backend, err error) { n.pinged(seq, role, b, err) })
	n.walked(seq, role, candidates[:started], served)
	if served {
		return "ok"
	}
	return "error"
}

// probeInTurn probes the candidates of a role in their order (issue #519,
// the review of #530): each on the round's whole remaining budget. The next
// candidate starts when the one before it has had its share — an equal part
// of what was left when it started, among it and the candidates still to
// come — or at once when every probe in flight has failed; an earlier one
// runs on meanwhile, and its answer counts until the round ends. So a later
// candidate is tried with a share of its own (a first one that hangs does not
// spend it), and a first candidate slower than its share still serves the
// role when the later ones fail: the share they leave by failing fast goes
// back to it. No candidate starts once one answered. Each result goes to
// pinged in the order the probes end; it waits for every probe it started and
// returns whether one answered and how many it started. The caller of
// /health waits no longer than the round's budget.
func probeInTurn(ctx context.Context, candidates []*backends.Backend, probes *hostProbes, pinged func(*backends.Backend, error)) (served bool, started int) {
	type result struct {
		i   int
		err error
	}
	results := make(chan result, len(candidates))
	var shareEnd time.Time
	start := func() {
		i := started
		started++
		if deadline, ok := ctx.Deadline(); ok {
			now := time.Now()
			shareEnd = now.Add(deadline.Sub(now) / time.Duration(len(candidates)-i))
		}
		go func() { results <- result{i, probes.ping(ctx, candidates[i].Host)} }()
	}
	inFlight := 0
	if len(candidates) > 0 {
		start()
		inFlight++
	}
	for inFlight > 0 {
		var next <-chan time.Time
		var timer *time.Timer
		if !served && started < len(candidates) && !shareEnd.IsZero() {
			timer = time.NewTimer(time.Until(shareEnd))
			next = timer.C
		}
		select {
		case r := <-results:
			inFlight--
			pinged(candidates[r.i], r.err)
			served = served || r.err == nil
		case <-next:
		}
		if timer != nil {
			timer.Stop()
		}
		// The share ran out, or nothing is in flight any more: the next one.
		if !served && started < len(candidates) && (inFlight == 0 || !time.Now().Before(shareEnd)) {
			start()
			inFlight++
		}
	}
	return served, started
}

// hostProbes is the probe memo of ONE probe round (#265): the first role that
// needs a base URL requests it, every other role of the same round that needs
// it waits for that result. Every probe of a round runs on the round's whole
// budget (probeInTurn, issue #519), so a role that waits for another role's
// probe of a base URL gets the answer it would have got itself. Keyed by the
// probe target pingHost requests (the base URL without trailing slashes), so
// "http://h/" and "http://h" are one probe. It lives exactly as long as the
// round — a later round probes afresh, nothing is cached across rounds.
type hostProbes struct {
	mu sync.Mutex
	m  map[string]*hostProbe
}

// hostProbe is one in-flight or finished probe. err is written once, before
// done is closed, and read only after done is closed.
type hostProbe struct {
	done chan struct{}
	err  error
}

func newHostProbes() *hostProbes {
	return &hostProbes{m: make(map[string]*hostProbe)}
}

// ping returns pingHost's result for host: the first role that needs it
// requests it, a role that finds the probe started waits for it until ctx
// ends. A finished result stands whatever the waiting role's context says,
// so the answer does not depend on which of the two finished first.
func (p *hostProbes) ping(ctx context.Context, host string) error {
	key := strings.TrimRight(host, "/")
	p.mu.Lock()
	pr, joined := p.m[key]
	if !joined {
		pr = &hostProbe{done: make(chan struct{})}
		p.m[key] = pr
	}
	p.mu.Unlock()
	if !joined {
		pr.err = pingHost(ctx, host)
		close(pr.done)
		return pr.err
	}
	select {
	case <-pr.done:
	default:
		select {
		case <-pr.done:
		case <-ctx.Done():
			return fmt.Errorf("connecting to host: %w", ctx.Err())
		}
	}
	return pr.err
}

// pingHost issues ONE credential-free GET against the backend's base URL and
// decides reachability by status class: every HTTP response below 500 means
// "the host is up and serving", 5xx and transport failures (DNS, connection
// refused, timeout) mean unreachable.
//
// Why < 500 instead of == 200 (PR #9, cloud-API false negative): only local
// servers (llama.cpp, Ollama) answer 200 on GET /. Cloud inference APIs route
// nothing at the root and answer 404/405 (Voyage, OpenAI-compatible
// gateways), and an auth-gated API answers 401/403 to an anonymous probe. All
// of those are HTTP responses from a live host — treating them as "down"
// marked every cloud backend permanently unhealthy. /health aggregates
// reachability only; whether a MODEL is servable is the chain's business, not
// this probe's.
//
// Why credential-free: /health is unauthenticated, uncached and public, so
// every byte this function sends is attacker-triggerable egress. Sending the
// backend's Authorization/ExtraHeaders here would push real credentials to
// third-party hosts (and across their redirects) on an anonymous request,
// bypassing the chain egress gates in backends.Pool (VisibleTo/disabledBy/
// Trust) that guard genuine inference traffic. An anonymous probe answers the
// health question without ever putting a secret on the wire — a 401 is proof
// of life just like a 200. Keep it that way.
//
// The probe is an answer only once it has read the answer (issue #519): a
// bounded prefix of the body, to its end or to the cap. A backend that sends
// its headers and then holds its body is unreachable — the read runs into the
// budget, and that error is the probe's result, where it was dropped before
// and the probe counted as an answer.
//
// The returned error carries the cause class ("status 503" vs. the wrapped
// transport error) because it is the only diagnostic the /health warn log has.
func pingHost(ctx context.Context, host string) error {
	url := strings.TrimRight(host, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // G704: host is a context_backends pool row (admin-gated, locality-validated), not free user input — same false positive as the Do() below. The G34 SSE path (HandleEvents → Snapshot(r.Context()) → HealthStatus → roleReachable → pingHost) lets gosec's taint analysis reach this line too. Since the probe carries no credentials, an SSRF through a poisoned pool row cannot exfiltrate a backend secret either.
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: pool rows are admin-gated runtime data (locality-validated); the request is credential-free, so nothing sensitive travels this hop or its redirect chain.
	if err != nil {
		return fmt.Errorf("connecting to host: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			slog.Warn("failed to close response body", "error", closeErr)
		}
	}()

	// Drain a bounded prefix before Close so the transport can reuse the
	// connection; an unread body makes it drop the socket. The cap keeps a
	// chatty/hostile endpoint from streaming into the health path.
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("host unhealthy: status %d", resp.StatusCode)
	}
	if readErr != nil {
		return fmt.Errorf("reading the answer: %w", readErr)
	}

	return nil
}

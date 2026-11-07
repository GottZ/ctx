package keyquota

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The ledger reasons (design/06 §3.4, §4.1): the CHECK'd vocabulary of
// context_llm_rejections_hourly.reason (migration 167). A refusal with any
// other reason is not counted (Ledger.Note) — the flush is one statement, and
// one row outside the CHECK would fail it with every other count in it.
const (
	RejectRate        = "rate"
	RejectQuota       = "quota"
	RejectConcurrency = "concurrency"
	RejectCapacity    = "capacity"
	RejectModel       = "model"
	RejectAuth        = "auth"
	RejectFederation  = "federation"
)

// The ledger details: the refusing rule under its reason (design/06 §3.4
// "detail"). A detail outside this list is counted under its reason with an
// empty detail, so no hook can widen the table's cardinality by a typo.
const (
	DetailInvalidKey      = "invalid_key"        // auth: the model-key door's 401
	DetailLocked          = "locked"             // auth: the door's failure lock (429)
	DetailAuthOverloaded  = "auth_overloaded"    // capacity: the door's concurrency share (503)
	DetailHeaderInvalid   = "header_invalid"     // federation: a malformed hop header (400)
	DetailLoop            = "loop"               // federation: the request came back (508)
	DetailDepth           = "depth"              // federation: the chain is too deep (508)
	DetailRequestTooLarge = "request_too_large"  // capacity: the body cap (413)
	DetailInflightBudget  = "inflight_budget"    // capacity: the in-flight byte budget (503)
	DetailNotFound        = "not_found"          // model: no such alias for the consumer (404)
	DetailNotSupported    = "not_supported"      // model: the alias does not serve the op (400)
	DetailEgressDenied    = "egress_denied"      // capacity: the chain's rules left no row (503)
	DetailNotOffered      = "not_offered"        // capacity: no row offers the external class (503)
	DetailTenantCalls     = "tenant_daily_calls" // quota: the tenant's daily call budget (429)
	DetailTenantCost      = "tenant_cost"        // quota: the tenant's cost budget (429)
	DetailTenant          = "tenant"             // quota: a tenant budget of another kind (429)
	DetailRateRequests    = "rate_requests"      // rate: the RPM bucket (429)
	DetailRateTokens      = "rate_tokens"        // rate: the TPM bucket (429)
	DetailWarming         = "warming"            // capacity: the meter is not seeded yet (429)
	DetailServerBusy      = "server_busy"        // capacity: L2 refused (cap ladder, queue full)
	DetailWaitExpired     = "wait_expired"       // capacity: L2's wait budget ran out
)

// UnresolvedAlias is the alias of every refusal before the model name was
// resolved to an alias of the registry (design/05 V-06-b): the client's string
// never reaches the ledger, so 10 000 unknown model names make one row.
const UnresolvedAlias = "_unresolved"

// maxAliasBytes bounds a resolved alias name in the ledger: the registry's
// names are shorter (modelalias); a longer one is no resolved name and counts
// as UnresolvedAlias.
const maxAliasBytes = 128

// ledgerRestoreCap bounds the cells a failed flush may hand back (Restore):
// the ledger holds at most that many before it drops a returned delta, so a
// database that stays down for hours cannot grow it without bound. Telemetry,
// not enforcement — a named constant, not a setting (design/06 §4.2).
const ledgerRestoreCap = 1 << 16

var (
	reasons = map[string]bool{
		RejectRate: true, RejectQuota: true, RejectConcurrency: true, RejectCapacity: true,
		RejectModel: true, RejectAuth: true, RejectFederation: true,
	}
	details = map[string]bool{
		DetailInvalidKey: true, DetailLocked: true, DetailAuthOverloaded: true, DetailHeaderInvalid: true,
		DetailLoop: true, DetailDepth: true, DetailRequestTooLarge: true, DetailInflightBudget: true,
		DetailNotFound: true, DetailNotSupported: true, DetailEgressDenied: true, DetailNotOffered: true,
		DetailTenantCalls: true, DetailTenantCost: true, DetailTenant: true, DetailRateRequests: true,
		DetailRateTokens: true, DetailWarming: true, DetailServerBusy: true, DetailWaitExpired: true,
	}
)

// Rejection is one refused /v1 request as the ledger counts it.
type Rejection struct {
	// ConsumerID is the model consumer the door accepted ("" before it:
	// NULL, instance-wide). Anything but "" or a UUID is not counted.
	ConsumerID string
	// Op is chat, embed or rerank; "" before the route knew it.
	Op string
	// Alias is the RESOLVED alias name; "" ⇒ UnresolvedAlias.
	Alias string
	// Class and Target are the dispatch class and the target origin of an
	// L2 refusal; kept for reason capacity only (cardinality, design/06
	// §3.4).
	Class, Target string
	// Reason and Detail are the ledger vocabulary.
	Reason, Detail string
	// Wait is the vain wait of a refusal that queued in L2; Waited says
	// whether it counts toward wait_n (a refusal that never waited does not).
	Wait   time.Duration
	Waited bool
}

// RejectionDelta is one cell of a drained ledger: the counts of one hour and
// dimension since the last drain.
type RejectionDelta struct {
	Bucket                               time.Time
	ConsumerID, Op, Alias, Class, Target string
	Reason, Detail                       string
	N, WaitMsSum, WaitN                  int64
}

type ledgerKey struct {
	bucket                             int64 // unix seconds of the UTC hour
	consumer, op, alias, class, target string
	reason, detail                     string
}

type ledgerCounts struct{ n, waitMs, waitN int64 }

// Ledger is the in-memory aggregate of every /v1 refusal of one process
// (design/06 §4.2 "Ledger"): Note is O(1) under one mutex, Drain hands the
// counts since the last drain to the flusher, which writes them to
// context_llm_rejections_hourly. The ledger never writes a log row and never
// touches a database itself (I-06-2).
type Ledger struct {
	now     func() time.Time
	dropped atomic.Uint64

	mu    sync.Mutex
	cells map[ledgerKey]ledgerCounts
}

// NewLedger returns an empty ledger over the clock now (the hour bucket of
// a refusal is now's UTC hour at Note).
func NewLedger(now func() time.Time) *Ledger {
	return &Ledger{now: now, cells: map[ledgerKey]ledgerCounts{}}
}

// Note counts one refusal. A reason outside the vocabulary or a consumer
// that is no UUID is not counted (Dropped); an unknown detail counts under
// its reason with an empty detail; an empty or over-long alias counts as
// UnresolvedAlias; class and target stay only on capacity.
func (l *Ledger) Note(r Rejection) {
	k, ok := ledgerKeyOf(r, l.now())
	if !ok {
		l.dropped.Add(1)
		return
	}
	var waitMs, waitN int64
	if r.Waited {
		waitMs, waitN = max(r.Wait.Milliseconds(), 0), 1
	}
	l.mu.Lock()
	c := l.cells[k]
	c.n++
	c.waitMs = satAdd(c.waitMs, waitMs)
	c.waitN += waitN
	l.cells[k] = c
	l.mu.Unlock()
}

func ledgerKeyOf(r Rejection, now time.Time) (ledgerKey, bool) {
	if !reasons[r.Reason] || (r.ConsumerID != "" && !isUUID(r.ConsumerID)) {
		return ledgerKey{}, false
	}
	k := ledgerKey{
		// One spelling per consumer: the database compares uuid values, the
		// cell key compares strings (Fremd-Review F-4).
		bucket: now.UTC().Truncate(time.Hour).Unix(), consumer: strings.ToLower(r.ConsumerID),
		alias: r.Alias, reason: r.Reason, detail: r.Detail,
	}
	if _, ok := ParseOp(r.Op); ok {
		k.op = r.Op
	}
	if k.alias == "" || len(k.alias) > maxAliasBytes {
		k.alias = UnresolvedAlias
	}
	if !details[k.detail] {
		k.detail = ""
	}
	if r.Reason == RejectCapacity {
		k.class, k.target = bound(r.Class), bound(r.Target)
	}
	return k, true
}

// bound cuts a class or target name at maxAliasBytes (both are server-side
// names; the cut only keeps a defect from widening a row).
func bound(s string) string {
	if len(s) > maxAliasBytes {
		return s[:maxAliasBytes]
	}
	return s
}

// isUUID reports whether s is a UUID in its canonical text form (8-4-4-4-12
// hex digits): the consumer ids of the model API; anything else would fail
// the flush's uuid cast.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHex(c) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// Drain returns the counts since the last drain, one delta per cell, in a
// fixed order (bucket, then the dimensions), and empties the ledger.
func (l *Ledger) Drain() []RejectionDelta {
	l.mu.Lock()
	cells := l.cells
	l.cells = make(map[ledgerKey]ledgerCounts, len(cells))
	l.mu.Unlock()
	out := make([]RejectionDelta, 0, len(cells))
	for k, c := range cells {
		out = append(out, RejectionDelta{
			Bucket: time.Unix(k.bucket, 0).UTC(), ConsumerID: k.consumer, Op: k.op, Alias: k.alias,
			Class: k.class, Target: k.target, Reason: k.reason, Detail: k.detail,
			N: c.n, WaitMsSum: c.waitMs, WaitN: c.waitN,
		})
	}
	slices.SortFunc(out, compareDelta)
	return out
}

func compareDelta(a, b RejectionDelta) int {
	return cmp.Or(a.Bucket.Compare(b.Bucket), cmp.Compare(a.ConsumerID, b.ConsumerID), cmp.Compare(a.Op, b.Op),
		cmp.Compare(a.Alias, b.Alias), cmp.Compare(a.Class, b.Class), cmp.Compare(a.Target, b.Target),
		cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Detail, b.Detail))
}

// Restore hands the deltas of a failed flush back to the ledger, added to
// whatever was noted since, so a passing database error loses no count. It
// returns the refusals it could not take back (the ledger already holds
// ledgerRestoreCap cells) — those are lost.
func (l *Ledger) Restore(deltas []RejectionDelta) (lost int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range deltas {
		k := ledgerKey{
			bucket: d.Bucket.Unix(), consumer: d.ConsumerID, op: d.Op, alias: d.Alias,
			class: d.Class, target: d.Target, reason: d.Reason, detail: d.Detail,
		}
		c, ok := l.cells[k]
		if !ok && len(l.cells) >= ledgerRestoreCap {
			lost = satAdd(lost, d.N)
			continue
		}
		c.n = satAdd(c.n, d.N)
		c.waitMs = satAdd(c.waitMs, d.WaitMsSum)
		c.waitN = satAdd(c.waitN, d.WaitN)
		l.cells[k] = c
	}
	return lost
}

// Dropped is the number of refusals Note did not count (a reason outside
// the vocabulary, a consumer that is no UUID) since the process started — a
// hook defect, never a client's doing.
func (l *Ledger) Dropped() uint64 { return l.dropped.Load() }

// LedgerReason is the ledger's reason and detail of a refused admission
// (design/06 §4.1 steps 7–9): rate for both buckets, concurrency for L1,
// capacity/warming for an unseeded meter. An invalid subject is a wiring
// defect, no refusal of the client: "" (not counted).
func (d Decision) LedgerReason() (reason, detail string) {
	switch d.Reason {
	case ReasonRequests:
		return RejectRate, DetailRateRequests
	case ReasonTokens:
		return RejectRate, DetailRateTokens
	case ReasonConcurrency:
		return RejectConcurrency, ""
	case ReasonWarming:
		return RejectCapacity, DetailWarming
	}
	return "", ""
}

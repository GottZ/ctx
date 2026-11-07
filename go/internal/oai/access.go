package oai

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

// AccessRecord is the request holder of the model groups' access log
// (design/06 §4.6, B-06-22; wave W06-9). The access-log stage — the
// outermost stage of a model group (handler.GatewayAccessLog) — puts a
// fresh one into every request's context; the stages behind it fill what
// they know, and the stage reads it after the inner chain returned: one
// "gateway request" line for a success, one count toward the throttled
// "gateway rejections" line for a refusal.
//
// Written only on the request's own goroutine before the handler returns
// (the stage reads it after ServeHTTP, which orders the two). It carries
// identities and counters, never a label, content, a key, its hint or a
// query string.
type AccessRecord struct {
	// ConsumerID, ModelKeyID and TenantID come from the model-key door
	// once it accepted the request (stage 8); empty on a refusal before.
	ConsumerID, ModelKeyID, TenantID string
	// Reason is the refusing stage's reason, a code of the gateway
	// vocabulary (Codes); empty ⇒ the stage logs http_<status>.
	Reason string

	// Filled by the handlers (W02-07ff.): the op, the alias, streaming,
	// the token counts and their source, the lease wait, the dispatch
	// class and the federation hop.
	Op, Alias                      string
	Stream                         bool
	PromptTokens, CompletionTokens int
	Usage                          string
	QueueWaitMs                    int64
	Class                          string
	Hop                            int

	// rejection is the refusal a stage or a handler declared for the
	// refusal ledger (NoteRejection, W06-12); wait and waited are the vain
	// queue wait of an L2 refusal (NoteRejectionWait). The access-log stage
	// counts the declaration once the request ended as a refusal.
	rejection *keyquota.Rejection
	wait      time.Duration
	waited    bool
}

// NoteRejection declares the request's refusal for the refusal ledger
// (design/06 §4.1 column "Ledger reason/detail", §3.4; wave W06-12): the one
// call of every refusal hook, reason and detail of keyquota's vocabulary.
// The access-log stage counts it — in the ledger, once — only when the
// request ended as a refusal (status >= 400), with the consumer, op,
// resolved alias and class the record holds by then. The first declaration
// wins (a request is refused once); an empty reason declares nothing. A nil
// record (outside a model group) ignores it.
func (rec *AccessRecord) NoteRejection(reason, detail string) {
	rec.NoteRejectionAt(reason, detail, "")
}

// NoteRejectionAt is NoteRejection with the target origin of an L2 refusal
// (the dispatcher target the refused acquire was for; design/06 §3.4
// "target", server-admin-only). The ledger keeps it for reason capacity
// only.
func (rec *AccessRecord) NoteRejectionAt(reason, detail, target string) {
	if rec == nil || reason == "" || rec.rejection != nil {
		return
	}
	rec.rejection = &keyquota.Rejection{Reason: reason, Detail: detail, Target: target}
}

// NoteRejectionWait sets the vain queue wait of the request's refusal: an
// L2 refusal declared by the admission writer, timed by the route and
// handed over with the meter reservation's Reject (cmd/ctxd
// meterReservation). Without a declaration it counts nowhere.
func (rec *AccessRecord) NoteRejectionWait(waited time.Duration) {
	if rec == nil {
		return
	}
	rec.wait, rec.waited = waited, true
}

// Rejection is the declared refusal with the request's identity as the
// record holds it now — the consumer the door accepted ("" before), the op,
// the resolved alias ("" before the resolution) and the dispatch class the
// planner bound (Class, set once a plan exists) — its target and the wait;
// false when nothing was declared.
func (rec *AccessRecord) Rejection() (keyquota.Rejection, bool) {
	if rec == nil || rec.rejection == nil {
		return keyquota.Rejection{}, false
	}
	r := *rec.rejection
	r.ConsumerID, r.Op, r.Alias, r.Class = rec.ConsumerID, rec.Op, rec.Alias, rec.Class
	r.Wait, r.Waited = rec.wait, rec.waited
	return r, true
}

// NoteRejection declares the refusal on the request's access record (see
// AccessRecord.NoteRejection); without one it does nothing.
func NoteRejection(ctx context.Context, reason, detail string) {
	AccessRecordFrom(ctx).NoteRejection(reason, detail)
}

// NoteRejectionAt is AccessRecord.NoteRejectionAt on the request's record.
func NoteRejectionAt(ctx context.Context, reason, detail, target string) {
	AccessRecordFrom(ctx).NoteRejectionAt(reason, detail, target)
}

// planClass is the dispatch class a plan's acquirer binds ("" without one):
// the class of the request's L2 refusal before any lease names it.
func planClass(p Plan) string {
	if p.Acquirer == nil {
		return ""
	}
	return p.Acquirer.Class().String()
}

type accessRecordKey struct{}

// WithAccessRecord returns ctx carrying rec.
func WithAccessRecord(ctx context.Context, rec *AccessRecord) context.Context {
	return context.WithValue(ctx, accessRecordKey{}, rec)
}

// AccessRecordFrom returns the request's access record; nil outside a model
// group (a caller fills it only when it is there).
func AccessRecordFrom(ctx context.Context) *AccessRecord {
	rec, _ := ctx.Value(accessRecordKey{}).(*AccessRecord)
	return rec
}

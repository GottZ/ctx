package oai

import "context"

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

package events

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgxlisten"
)

type countSink struct{ notifies, backlogs atomic.Int64 }

func (s *countSink) ModelAccessNotify(string) { s.notifies.Add(1) }
func (s *countSink) ModelAccessBacklog()      { s.backlogs.Add(1) }

// TestModelAccessHandlerForwardsToTheSink (W01-8): the channel handler is
// inert without a sink (the model API's door not built yet) and forwards
// every notification and every backlog to the sink installed later —
// SetModelAccessSink runs after the scheduler (and with it the listener)
// started; the handler reads the sink per call.
func TestModelAccessHandlerForwardsToTheSink(t *testing.T) {
	s := &Scheduler{}
	h := &ModelAccessHandler{scheduler: s}
	ctx := context.Background()
	n := &pgconn.Notification{Channel: channelModelAccessWrite, Payload: `{"table":"context_model_keys","op":"UPDATE","id":"k"}`}
	if err := h.HandleNotification(ctx, n, nil); err != nil {
		t.Fatalf("without a sink: %v", err)
	}
	if err := h.HandleBacklog(ctx, channelModelAccessWrite, nil); err != nil {
		t.Fatalf("backlog without a sink: %v", err)
	}
	sink := &countSink{}
	s.SetModelAccessSink(sink)
	_ = h.HandleNotification(ctx, n, nil)
	_ = h.HandleNotification(ctx, n, nil)
	_ = h.HandleBacklog(ctx, channelModelAccessWrite, nil)
	if sink.notifies.Load() != 2 || sink.backlogs.Load() != 1 {
		t.Fatalf("sink got %d notifications, %d backlogs — want 2 and 1", sink.notifies.Load(), sink.backlogs.Load())
	}
}

// TestListenerRegistersModelAccessChannel: the production listener LISTENs
// on ctx_model_access_write (red without the registration in
// NewPgxlistenListener).
func TestListenerRegistersModelAccessChannel(t *testing.T) {
	l := NewPgxlistenListener("postgres://unused", 0, &Scheduler{}, nil, nil, nil, nil, nil)
	if !listenerHandles(l, channelModelAccessWrite) {
		t.Fatalf("NewPgxlistenListener does not handle %s", channelModelAccessWrite)
	}
}

// listenerHandles reports whether l has a handler for channel (pgxlisten
// keeps them in an unexported map; reflection only reads it).
func listenerHandles(l *pgxlisten.Listener, channel string) bool {
	m := reflect.ValueOf(l).Elem().FieldByName("handlers")
	for _, k := range m.MapKeys() {
		if k.String() == channel {
			return true
		}
	}
	return false
}

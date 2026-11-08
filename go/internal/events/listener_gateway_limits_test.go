package events

// The hot-reload path of the meter's limits rows (wave W06-15, migration
// 168; dispatch C1 "Hot-Reload … über den bestehenden Settings-/Notify-Pfad"):
// a ctx_settings_write NOTIFY with entity context_gateway_limits reaches the
// limits registry and nothing else — no settings reload (a nil config store
// would panic on that path), no tenant invalidation.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/gatewaylimits"
	"github.com/GottZ/ctx/internal/keyquota"
)

func TestSettingsNotifyReloadsGatewayLimits(t *testing.T) {
	rpm := int64(4)
	reg := gatewaylimits.NewRegistry(func(context.Context) ([]gatewaylimits.Row, error) {
		return []gatewaylimits.Row{{ID: "r1", Kind: gatewaylimits.SubjectConsumer, ConsumerID: "c1", Tier: gatewaylimits.TierServer,
			Op: keyquota.OpChat, Values: gatewaylimits.Values{RPM: &rpm}}}, nil
	})
	h := &SettingsWriteHandler{gatewayLimits: reg} // no config store: a settings reload would panic
	n := &pgconn.Notification{Channel: channelSettingsWrite, Payload: `{"entity":"context_gateway_limits","key":null,"scope":null,"op":"INSERT"}`}
	if err := h.HandleNotification(context.Background(), n, nil); err != nil {
		t.Fatalf("HandleNotification: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(reg.Snapshot().Rows()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("a context_gateway_limits NOTIFY never reached the limits registry (want the row loaded after the trigger)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := reg.Snapshot().Resolve("c1", "t1", keyquota.OpChat, gatewaylimits.Defaults{RPM: 30, TPM: 1, Concurrency: 1}).RPM; got != 4 {
		t.Errorf("after the NOTIFY: rpm %d, want the row's 4", got)
	}
}

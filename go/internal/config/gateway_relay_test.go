package config

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestGatewayStreamRelayKeys is gate (f) of A03-W10 (design/03 §4.10 no. 1,
// A-E2 no. 2): the three relay thresholds are registry keys — hot,
// global-only, strict, env CTX_GATEWAY_* — with the defaults of §4.10 (2 MiB,
// 1 KiB/s, 30 s); 0 for any of them is a V38 error; the buffer must hold one
// frame at the frame cap (V39) and joins the stream's response share of the
// budget floor, which at the defaults stays where it was.
func TestGatewayStreamRelayKeys(t *testing.T) {
	want := map[string]struct{ env, def string }{
		"gateway.stream_relay_buffer_bytes": {"CTX_GATEWAY_STREAM_RELAY_BUFFER_BYTES", "2097152"},
		"gateway.stream_min_drain_rate":     {"CTX_GATEWAY_STREAM_MIN_DRAIN_RATE", "1024"},
		"gateway.stream_min_drain_window":   {"CTX_GATEWAY_STREAM_MIN_DRAIN_WINDOW", "30"},
	}
	seen := 0
	for _, e := range registry() {
		w, ok := want[e.Key]
		if !ok {
			continue
		}
		seen++
		if e.EnvVar != w.env || e.defRaw != w.def || e.Mut != "hot" || e.Tenancy != "global-only" || !e.Strict {
			t.Errorf("%s: env %q default %q mut %q tenancy %q strict %v — want %s, %s, hot, global-only, strict",
				e.Key, e.EnvVar, e.defRaw, e.Mut, e.Tenancy, e.Strict, w.env, w.def)
		}
	}
	if seen != len(want) {
		t.Fatalf("%d of the %d relay keys in the registry", seen, len(want))
	}
	c := Defaults()
	s := c.GatewaySettings()
	if s.StreamRelayBufferBytes != 2<<20 || s.StreamMinDrainRate != 1024 || s.StreamMinDrainWindow.Seconds() != 30 {
		t.Fatalf("settings snapshot %d / %d / %v, want 2097152 / 1024 / 30s", s.StreamRelayBufferBytes, s.StreamMinDrainRate, s.StreamMinDrainWindow)
	}

	// V38: 0 refused for each of the three.
	for key, mut := range map[string]func(*Config){
		"gateway.stream_relay_buffer_bytes": func(c *Config) { c.Gateway.StreamRelayBufferBytes = 0 },
		"gateway.stream_min_drain_rate":     func(c *Config) { c.Gateway.StreamMinDrainRate = 0 },
		"gateway.stream_min_drain_window":   func(c *Config) { c.Gateway.StreamMinDrainWindow = 0 },
	} {
		c := Defaults()
		mut(c)
		if !hasErrorOn(validateGateway(c), key) {
			t.Errorf("%s = 0 is no V38 error", key)
		}
	}

	// V39: one frame at the cap fits — the buffer at frame + header is legal,
	// one byte less is an error.
	c = Defaults()
	c.Gateway.MaxStreamFrameBytes, c.Gateway.StreamRelayBufferBytes = 3<<20, 3<<20+oai.RelayRecordHeader-1
	if !hasErrorOn(validateGateway(c), "gateway.stream_relay_buffer_bytes") {
		t.Error("a relay buffer one byte short of a frame at the cap is legal")
	}
	c.Gateway.StreamRelayBufferBytes++
	if issues := validateGateway(c); len(issues) != 0 {
		t.Errorf("a relay buffer of exactly one frame at the cap + header is refused: %+v", issues)
	}

	// The chat route's response share: at the defaults the non-stream
	// share 2 × 4 MiB dominates 2 × 1 MiB + 2 MiB; past the defaults the
	// buffer counts — a stream share above the non-stream one raises the
	// chat needs 1:1 (RC-D C8: the floor is the largest route need, so the
	// chat parts carry the share, not the whole floor).
	c = Defaults()
	if share := gatewayResponseShare(&c.Gateway); share != 8<<20 {
		t.Fatalf("default chat response share %d, want %d (unchanged by the relay buffer)", share, 8<<20)
	}
	base := gatewayFloorParts(&c.Gateway, int64(c.Gateway.MaxRequestBytes), int64(c.Gateway.ScanMaxRequestBytes))
	c.Gateway.MaxStreamFrameBytes, c.Gateway.StreamRelayBufferBytes = 4<<20, 6<<20
	parts := gatewayFloorParts(&c.Gateway, int64(c.Gateway.MaxRequestBytes), int64(c.Gateway.ScanMaxRequestBytes))
	for i := range 2 {
		if want := base[i].need - 8<<20 + 8<<20 + 6<<20; parts[i].need != want {
			t.Fatalf("chat %s need with frame 4 MiB and buffer 6 MiB = %d, want %d (2 × frame + buffer replaces the 8 MiB share)", parts[i].class, parts[i].need, want)
		}
	}
	// A relay buffer large enough makes the chat route decide the floor.
	c.Gateway.StreamRelayBufferBytes = 1 << 30
	parts = gatewayFloorParts(&c.Gateway, int64(c.Gateway.MaxRequestBytes), int64(c.Gateway.ScanMaxRequestBytes))
	if floor, _ := gatewayBudgetFloor(&c.Gateway); floor != parts[1].need+oai.UpstreamFrameReserve {
		t.Fatalf("floor with a 1 GiB relay buffer = %d, want the chat need below credentials %d + the frame reserve", floor, parts[1].need)
	}
}

func hasErrorOn(issues []Issue, field string) bool {
	for _, i := range issues {
		if i.Field == field && i.Severity == SeverityError && strings.Contains(i.Msg, strings.TrimPrefix(field, "gateway.")) {
			return true
		}
	}
	return false
}

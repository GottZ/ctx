package llm

// FOLGE-S (d) (RC-D S2, cluster C8; lead decision): the relay books every
// buffer a growth discards before it makes the larger one — the doubling of
// its line buffer leaves up to one frame cap on the heap until the next
// collection, which the upfront share (2 × the frame cap) does not cover.
// A stream of 8 frames of 1 MiB − 64 books exactly the discarded doubling
// steps (4 KiB — the first piece bufio hands over — … 512 KiB); a refused
// booking ends the stream before the growth, with the booking's error.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func folgeSGrowthStream(k, size int) string {
	frame := "data: {\"id\":\"I\",\"model\":\"M\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("a", size-len("data: {\"id\":\"I\",\"model\":\"M\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":null}]}\n")) + "\"},\"finish_reason\":null}]}\n\n"
	return strings.Repeat(frame, k) + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
}

func TestRelayBooksItsGrowth(t *testing.T) {
	var booked, calls int
	rl := &sseRelay{onFrame: func([]byte) error { return nil }, data: sseFrame{limit: 1 << 20}}
	rl.grow = func(n int) error { booked += n; calls++; return nil }
	rl.data.grow = rl.grow
	if err := rl.run(strings.NewReader(folgeSGrowthStream(8, 1<<20-64)), 1<<20); err != nil {
		t.Fatal(err)
	}
	// the line buffer starts at the first piece bufio hands over (4 KiB)
	// and doubles to 1 MiB: it discards 4 KiB … 512 KiB
	want := 0
	for c := 4096; c < 1<<20; c *= 2 {
		want += c
	}
	if rl.frames != 9 {
		t.Fatalf("relayed %d frames, want 9", rl.frames)
	}
	t.Logf("FOLGE-S (d): 8 frames of 1 MiB − 64 booked %d B in %d calls (discarded doubling steps %d B)", booked, calls, want)
	if booked < want {
		t.Errorf("the relay booked %d B of its growth, want at least the %d B its doubling discarded (FOLGE-S (d))", booked, want)
	}
	if booked > want {
		t.Errorf("the relay booked %d B, want about the %d B its doubling discarded (a frame at the cap grows once)", booked, want)
	}
}

func TestRelayGrowthRefusalEndsTheStream(t *testing.T) {
	refused := errors.New("ledger full")
	var frames int
	rl := &sseRelay{onFrame: func([]byte) error { frames++; return nil }, data: sseFrame{limit: 1 << 20}}
	rl.grow = func(int) error { return refused }
	rl.data.grow = rl.grow
	err := rl.run(strings.NewReader(folgeSGrowthStream(2, 64<<10)), 1<<20)
	if !errors.Is(err, refused) {
		t.Fatalf("run = %v, want the booking's error", err)
	}
	if frames != 0 {
		t.Errorf("%d frames relayed before the refused growth, want 0 (the first growth past 512 B is refused)", frames)
	}
}

// TestGrowthBookingRidesTheContext: ChatRawStream takes the booking from its
// context (the wire signature stays as R-02-2 pins it).
func TestGrowthBookingRidesTheContext(t *testing.T) {
	book := func(int) error { return nil }
	if growthBookingFrom(context.Background()) != nil {
		t.Fatal("a plain context carries a growth booking")
	}
	if growthBookingFrom(WithGrowthBooking(context.Background(), book)) == nil {
		t.Fatal("WithGrowthBooking's context carries none")
	}
}

// TestChatRawStreamBooksEveryBuffer: through the wire site, the context's
// booking reaches the line, the spare and the frame buffer — frames of two
// data lines that grow from 8 KiB to 64 KiB each book what the same bytes
// book through a relay whose three buffers all carry the booking.
func TestChatRawStreamBooksEveryBuffer(t *testing.T) {
	line := func(n int) string { return "data: " + strings.Repeat(" ", n) }
	stream := line(8<<10) + "\n" + line(8<<10) + "{\"choices\":[]}\n\n" +
		line(64<<10) + "\n" + line(64<<10) + "{\"choices\":[]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	var direct int
	rl := &sseRelay{onFrame: func([]byte) error { return nil }, data: sseFrame{limit: 1 << 20}}
	rl.grow = func(n int) error { direct += n; return nil }
	rl.data.grow = rl.grow
	if err := rl.run(strings.NewReader(stream), 1<<20); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	t.Cleanup(srv.Close)
	var wire int
	ctx := WithGrowthBooking(context.Background(), func(n int) error { wire += n; return nil })
	if _, err := ChatRawStream(ctx, ptRow(srv.URL), []byte(`{"stream":true}`), 5*time.Second, 1<<20, func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Logf("FOLGE-S (d): two-line frames booked %d B through the wire site, %d B through a fully booked relay", wire, direct)
	if wire != direct || direct == 0 {
		t.Errorf("ChatRawStream booked %d B, a relay booking all three buffers %d B — every buffer's growth goes through the context's booking", wire, direct)
	}
}

package handler

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/recblocks"
)

// TestRecordingBlockViewsHoldNoBody (issue #575): the detail's block views
// carry their previews, cut and copied, and no byte of the body — a block's
// bytes point into the decomposition's copy of the whole body, and a view
// that kept them would hold that copy until its answer is written, past the
// content slot.
func TestRecordingBlockViewsHoldNoBody(t *testing.T) {
	long := strings.Repeat("x", 3*recordingPreviewBytes)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + long + `"},{"role":"assistant","content":[{"type":"text","text":"hello"}]}]}`)
	res := recblocks.DecomposeWindow(recblocks.Input{Op: "chat", Path: "/v1/chat/completions", State: "done", Status: 200, Request: body}, 0, recordingBlocksPage)
	if len(res.Blocks) < 3 {
		t.Fatalf("blocks: %d", len(res.Blocks))
	}
	views := recordingBlockViews(res.Blocks)
	for _, v := range views {
		if c := v.Content(); c != nil {
			t.Errorf("view %s holds %d bytes of the body", v.Address, len(c))
		}
	}
	if v := views[0]; v.Address != "request/messages/0" || v.Preview != long[:recordingPreviewBytes] || !v.PreviewTruncated || v.Bytes != len(long) {
		t.Errorf("first view: %s preview %d bytes truncated %v size %d", v.Address, len(v.Preview), v.PreviewTruncated, v.Bytes)
	}
	if v := views[1]; v.Address != "request/messages/1/content/0" || v.Preview != "hello" || v.PreviewTruncated {
		t.Errorf("second view: %s %q", v.Address, v.Preview)
	}
}

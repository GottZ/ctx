package handler

import (
	"encoding/json"
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

// TestRecordingBlockViewsCutTheirLabels: a message's labels (role, name,
// tool call id) stand in every block of the message, so a view carries each
// label cut to recordingLabelBytes and says so — a long name of a message of
// many parts multiplies into the answer once per block otherwise.
func TestRecordingBlockViewsCutTheirLabels(t *testing.T) {
	name := strings.Repeat("n", 64<<10)
	parts := strings.Repeat(`{"type":"text","text":"x"},`, 99) + `{"type":"text","text":"x"}`
	body := []byte(`{"model":"m","messages":[{"role":"user","name":"` + name + `","content":[` + parts + `]}]}`)
	res := recblocks.DecomposeWindow(recblocks.Input{Op: "chat", Path: "/v1/chat/completions", State: "done", Status: 200, Request: body}, 0, recordingBlocksPage)
	out, err := json.Marshal(recordingBlockViews(res.Blocks))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 100*1024 {
		t.Errorf("100 views of a message with a 64 KiB name: %d bytes of JSON, want the name cut in each (≤ 100 KiB in all)", len(out))
	}
	var views []map[string]any
	if err := json.Unmarshal(out, &views); err != nil {
		t.Fatal(err)
	}
	if len(views) != 101 { // the 100 parts and request/params
		t.Fatalf("views: %d", len(views))
	}
	if v := views[0]; v["name"] != name[:256] || v["labels_truncated"] != true || v["role"] != "user" {
		t.Errorf("first view: name %d bytes, labels_truncated %v, role %v", len(v["name"].(string)), v["labels_truncated"], v["role"])
	}
	short := recblocks.DecomposeWindow(recblocks.Input{Op: "chat", Path: "/v1/chat/completions", State: "done", Status: 200,
		Request: []byte(`{"messages":[{"role":"user","name":"bob","content":"hi"}]}`)}, 0, recordingBlocksPage)
	if out, _ := json.Marshal(recordingBlockViews(short.Blocks)); strings.Contains(string(out), "labels_truncated") {
		t.Errorf("a view whose labels are short names no cut: %s", out)
	}
}

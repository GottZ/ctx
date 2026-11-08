package llmlog

import (
	"encoding/json"
	"strings"
	"testing"
)

// F-W12 (design/04 §4.7 attribution, M-F4 replacement; RC-D S1): the row of
// a proved peer carries metadata.gw.peer_instance — the instance the request
// proved at the PeerIngress — byte-exact after model_key_id, a UUID or
// nothing. Red against the S1 stub (wire ignores Gateway.PeerInstance).
//
//	go test -short -count=1 -run 'TestStampGatewayPeerInstance' ./internal/llmlog/

const gwTestPeerInstance = "0195a6f0-0000-7000-8000-0000000000b2"

func gwBytes(t *testing.T, e Entry) string {
	t.Helper()
	b, err := json.Marshal(e.Metadata["gw"])
	if err != nil {
		t.Fatalf("marshal metadata.gw: %v", err)
	}
	return string(b)
}

func TestStampGatewayPeerInstance(t *testing.T) {
	e := Entry{Pipeline: PipelineGatewayChat}
	e.StampGateway(gwTestConsumer, Gateway{
		Op: GatewayOpChat, Hop: 2, ClaimedChain: []string{"0195a6f0-0000-7000-8000-0000000000a1", gwTestPeerInstance},
		PeerPipeline: "dream-eval", ModelKeyID: gwTestModelKey, PeerInstance: gwTestPeerInstance,
	})
	want := `{"v":1,"op":"chat","stream":false,"hop":2,"claimed_chain":["0195a6f0-0000-7000-8000-0000000000a1","` + gwTestPeerInstance +
		`"],"peer_pipeline":"dream-eval","model_key_id":"` + gwTestModelKey + `","peer_instance":"` + gwTestPeerInstance + `"}`
	if got := gwBytes(t, e); got != want {
		t.Errorf("metadata.gw of a proved peer:\n got %s\nwant %s", got, want)
	}

	// A malformed id is dropped, an upper-case one stored canonical.
	for in, want := range map[string]string{
		"not-a-uuid":                        "",
		strings.ToUpper(gwTestPeerInstance): gwTestPeerInstance,
		gwTestPeerInstance + "x":            "",
		"":                                  "",
	} {
		z := Entry{Pipeline: PipelineGatewayChat}
		z.StampGateway(gwTestConsumer, Gateway{Op: GatewayOpChat, PeerInstance: in})
		got := gwBytes(t, z)
		has := strings.Contains(got, `"peer_instance":`)
		if want == "" && has {
			t.Errorf("PeerInstance %q: stored %s, want no peer_instance", in, got)
		}
		if want != "" && !strings.HasSuffix(got, `,"peer_instance":"`+want+`"}`) {
			t.Errorf("PeerInstance %q: %s, want peer_instance %q last", in, got, want)
		}
	}
}

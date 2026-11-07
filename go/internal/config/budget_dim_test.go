package config

// The arithmetic half of the budget dimensioning harness (RC-D cluster C8,
// internal/oai TestBudgetDimension measures the other): V39's floor at the
// configuration's defaults, term by term, the scan's share of it, the floor
// without the scan, the largest body cap a budget holds, and the scan
// booking of a public alias as a function of a separate body cap — the
// numbers a default for gateway.inflight_budget and a scan cap are chosen
// from. One JSON line per figure to $CTX_BUDGET_DIM_OUT (appended):
//
//	CTX_BUDGET_DIM=1 CTX_BUDGET_DIM_OUT=<file> go test -run '^TestBudgetDimensionCalc$' ./internal/config/

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/oai"
)

func calcEmit(t *testing.T, point string, fields map[string]any) {
	t.Helper()
	fields["point"] = point
	fields["utc"] = time.Now().UTC().Format(time.RFC3339)
	if r := os.Getenv("CTX_BUDGET_DIM_REP"); r != "" {
		fields["rep"] = r
	}
	line, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("BUDGETDIM %s", line)
	if p := os.Getenv("CTX_BUDGET_DIM_OUT"); p != "" {
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func calcMiB(v int64) float64 { return float64(v) / (1 << 20) }

// calcLargestCap is the largest body cap whose request part plus response
// fits budget (the bisection of gatewayCapWayOut, with or without the scan's
// share of the request part).
func calcLargestCap(budget, response int64, scan bool) int64 {
	need := func(n int64) int64 {
		r := gatewayRequestNeed(n)
		if !scan {
			r -= int64(oai.ScanReserveFactor)*n + oai.ScanReserveBase + oai.ScanDepthReserve
		}
		return r + response
	}
	if need(1) > budget {
		return 0
	}
	lo, hi := int64(1), budget
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if need(mid) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func TestBudgetDimensionCalc(t *testing.T) {
	if os.Getenv("CTX_BUDGET_DIM") != "1" {
		t.Skip("CTX_BUDGET_DIM=1 not set (measurement harness)")
	}
	if testing.Short() {
		t.Skip("measurement harness: outside -short")
	}
	g := &Defaults().Gateway
	capB := int64(g.MaxRequestBytes)
	floor, ok := gatewayBudgetFloor(g)
	if !ok {
		t.Fatal("defaults: no floor")
	}
	scan := int64(oai.ScanReserveFactor)*capB + oai.ScanReserveBase + oai.ScanDepthReserve
	resp := gatewayResponseShare(g)
	calcEmit(t, "calc_v39_floor", map[string]any{
		"max_request_bytes": capB, "inflight_budget": g.InflightBudget, "max_response_bytes": g.MaxResponseBytes,
		"max_stream_frame_bytes": g.MaxStreamFrameBytes, "stream_relay_buffer_bytes": g.StreamRelayBufferBytes,
		"embedding_max_floats": g.EmbeddingMaxFloats, "httpx_max_response_bytes": httpx.MaxResponseBytes,
		"request_share_3x": int64(oai.RequestReserveFactor) * capB, "scan_share": scan, "scan_share_mib": calcMiB(scan),
		"read_term": oai.ChunkedReadReserve(capB), "json_state": oai.JSONStateReserve, "scan_depth": oai.ScanDepthReserve,
		"scan_base": oai.ScanReserveBase, "fixed": gatewayRequestFixed,
		"response_nonstream": gatewayResponseCopies * int64(g.MaxResponseBytes),
		"response_stream":    gatewayResponseCopies*int64(g.MaxStreamFrameBytes) + int64(g.StreamRelayBufferBytes),
		"response_embed":     gatewayFloatBytes*int64(g.EmbeddingMaxFloats) + gatewayEmbedEncoderReserve,
		"response_share":     resp,
		"floor":              floor, "floor_mib": calcMiB(floor), "headroom": int64(g.InflightBudget) - floor,
		"headroom_mib":       calcMiB(int64(g.InflightBudget) - floor),
		"floor_without_scan": floor - scan, "floor_without_scan_mib": calcMiB(floor - scan),
		"scan_share_of_floor":                float64(scan) / float64(floor),
		"largest_cap_at_budget":              calcLargestCap(int64(g.InflightBudget), resp, true),
		"largest_cap_at_budget_without_scan": calcLargestCap(int64(g.InflightBudget), resp, false),
	})
	// The floor at other body caps, with and without the scan's share.
	for _, c := range []int64{256 << 10, 1 << 20, 2 << 20, 4 << 20, 8 << 20, 16 << 20} {
		need := gatewayRequestNeed(c) + resp
		s := int64(oai.ScanReserveFactor)*c + oai.ScanReserveBase + oai.ScanDepthReserve
		calcEmit(t, "calc_floor_by_cap", map[string]any{
			"cap": c, "floor": need, "floor_mib": calcMiB(need), "scan_share": s, "floor_without_scan": need - s,
			"floor_without_scan_mib": calcMiB(need - s),
		})
	}
	// The budget the default caps need if the public scan has a body cap of
	// its own (a request below credentials larger than it refused before the
	// scan): the floor = max(credentials request at the full cap, public
	// request at the scan cap).
	for _, sc := range []int64{512 << 10, 1 << 20, 2 << 20, 4 << 20, 8 << 20} {
		cred := floor - scan
		pubNeed := gatewayRequestNeed(sc) + resp
		calcEmit(t, "calc_scan_cap", map[string]any{
			"scan_max_request_bytes": sc, "scan_booking": int64(oai.ScanReserveFactor)*sc + oai.ScanReserveBase + oai.ScanDepthReserve,
			"scan_booking_mib":    calcMiB(int64(oai.ScanReserveFactor)*sc + oai.ScanReserveBase + oai.ScanDepthReserve),
			"floor_cred_full_cap": cred, "floor_pub_at_scan_cap": pubNeed,
			"floor": max(cred, pubNeed), "floor_mib": calcMiB(max(cred, pubNeed)),
			"concurrent_pub_at_scan_cap_in_default_budget": int64(g.InflightBudget) / pubNeed,
		})
	}
}

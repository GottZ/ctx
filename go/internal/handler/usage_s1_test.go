package handler

// RC-D cluster C2 unit gates in the handler package:
//   - W06-17: the list query is chosen by role, without "$x IS NULL OR",
//     each tenant sub-query bounded by the 30-day floor, ?consumer_id=
//     intersected (a foreign id never reaches the statement).
//   - W06-16 / V-06-d: the top-consumers ring answers from memory and is
//     empty 61 minutes after the last request (mock clock).

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLLMLogListQueryText is the text half of W06-17's GENERIC_PLAN gate
// (the plan half is TestLLMLogConsumerGenericPlan, integration).
func TestLLMLogListQueryText(t *testing.T) {
	const (
		key     = "0195a6f0-0000-7000-8000-0000000000a1"
		own     = "0195a6f0-0000-7000-8000-0000000000c1"
		foreign = "0195a6f0-0000-7000-8000-0000000000c9"
	)
	sql, _, err := llmlogListQuery(llmlogListFilter{KeyIDs: []string{key}, ConsumerIDs: []string{own}, Limit: 50})
	if err != nil {
		t.Fatalf("W06-17 gate (role-chosen SQL): tenant list: %v", err)
	}
	if strings.Contains(sql, "IS NULL OR") {
		t.Errorf("W06-17 gate (no IS NULL OR): tenant SQL carries the OR form:\n%s", sql)
	}
	if !strings.Contains(sql, "UNION ALL") || strings.Count(sql, llmlogConsumerFloor) != 2 {
		t.Errorf("W06-17 gate (UNION ALL, 30-day floor per branch): tenant SQL has UNION ALL %v, floor %d× (want 2):\n%s",
			strings.Contains(sql, "UNION ALL"), strings.Count(sql, llmlogConsumerFloor), sql)
	}
	if !strings.Contains(sql, "consumer_id") {
		t.Errorf("W06-17 gate (consumer_id out): the list SQL never selects consumer_id:\n%s", sql)
	}
	_, args, err := llmlogListQuery(llmlogListFilter{KeyIDs: []string{key}, ConsumerIDs: []string{own}, Consumer: foreign, Limit: 50})
	if err != nil {
		t.Fatalf("W06-17 gate (intersection): %v", err)
	}
	if strings.Contains(fmt.Sprint(args...), foreign) {
		t.Errorf("W06-17 gate (intersection, never OR): a foreign consumer_id reached the statement's arguments %v", args)
	}
	sql, _, err = llmlogListQuery(llmlogListFilter{ServerAdmin: true, Consumer: own, Limit: 50})
	if err != nil || strings.Contains(sql, "IS NULL OR") || !strings.Contains(sql, "consumer_id = ") {
		t.Errorf("W06-17 gate (server-admin consumer filter): err %v, SQL:\n%s", err, sql)
	}
}

// ringClock is a settable clock for the ring.
type ringClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ringClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *ringClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestConsumerRingRotates is V-06-d: three requests of c1 and one of c2
// (other tenant) ⇒ Top lists c1 then c2; the tenant view lists only its
// own; 61 minutes later (ringBuckets + 1 one-minute buckets) the ring is
// empty — rot gegen einen Ring ohne Rotation.
func TestConsumerRingRotates(t *testing.T) {
	c := &ringClock{t: time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)}
	r := NewConsumerRing(c.now)
	for range 3 {
		r.Add("c1", "t1", 100)
	}
	r.Add("c2", "t2", 50)
	top := r.Top(10, "")
	if len(top) != 2 || top[0] != (TopConsumer{ConsumerID: "c1", Requests: 3, Tokens: 300}) || top[1].ConsumerID != "c2" {
		t.Fatalf("W06-16 gate (top_consumers_1h): Top = %+v, want c1 (3 requests, 300 tokens) then c2", top)
	}
	if own := r.Top(10, "t2"); len(own) != 1 || own[0].ConsumerID != "c2" {
		t.Errorf("W06-16 gate (tenant view): Top(t2) = %+v, want only c2", own)
	}
	c.add(time.Duration(ringBuckets+1) * time.Minute)
	if top := r.Top(10, ""); len(top) != 0 {
		t.Errorf("W06-16 gate (ring rotation, V-06-d): after 61 min Top = %+v, want empty", top)
	}
	if gatewayGenTTL != 30*time.Second || usageGenerationTTL != 30*time.Second {
		t.Errorf("V-06-d/V-06-f: generation TTLs %v / %v, want 30 s", gatewayGenTTL, usageGenerationTTL)
	}
}

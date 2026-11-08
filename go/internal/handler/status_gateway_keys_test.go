package handler

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/usageevents"
)

// snakeKey is the wire form of every key of the gateway part of
// /api/status: lower case, digits, underscores, starting with a letter.
var snakeKey = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

// jsonMarshalerType is the interface whose implementers (time.Time) carry
// their own wire form and are leaves of the key walk.
var jsonMarshalerType = reflect.TypeFor[json.Marshaler]()

// walkJSONKeyTypes checks every exported field reachable from t: it needs
// an explicit json name in snake_case (a field without one marshals under
// its Go name — the FOLGE-V defect). path names the field for the message.
func walkJSONKeyTypes(t *testing.T, ty reflect.Type, path string, seen map[reflect.Type]bool) {
	t.Helper()
	for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Array || ty.Kind() == reflect.Map {
		ty = ty.Elem()
	}
	if ty.Kind() != reflect.Struct || seen[ty] || ty.Implements(jsonMarshalerType) || reflect.PointerTo(ty).Implements(jsonMarshalerType) {
		return
	}
	seen[ty] = true
	for i := range ty.NumField() {
		f := ty.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" && f.Anonymous {
			walkJSONKeyTypes(t, f.Type, path, seen)
			continue
		}
		if name == "" {
			t.Errorf("FOLGE-V gate (key form): %s.%s (%s) has no json name — it marshals as %q, want a snake_case tag", path, f.Name, ty, f.Name)
			continue
		}
		if !snakeKey.MatchString(name) {
			t.Errorf("FOLGE-V gate (key form): %s.%s marshals as %q, want snake_case", path, f.Name, name)
		}
		walkJSONKeyTypes(t, f.Type, path+"."+name, seen)
	}
}

// walkJSONKeys checks every object key of a decoded JSON value.
func walkJSONKeys(t *testing.T, v any, path string) {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		for k, sub := range x {
			if !snakeKey.MatchString(k) {
				t.Errorf("FOLGE-V gate (wire key form): %s carries key %q, want snake_case", path, k)
			}
			walkJSONKeys(t, sub, path+"."+k)
		}
	case []any:
		for i, sub := range x {
			walkJSONKeys(t, sub, path+"["+strconv.Itoa(i)+"]")
		}
	}
}

// fillNonZero sets every settable number, string and bool reachable in v to
// a non-zero value, so a field that only marshals when set still shows up.
func fillNonZero(v reflect.Value) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				fillNonZero(v.Field(i))
			}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(7))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	}
}

// TestStatusGatewayKeysSnakeCase is the FOLGE-V pin (LOOP Queue 70): the
// whole gateway part of the server-admin /api/status — gateway with auth,
// events, top_consumers_1h, instance_envelope, and its sibling sections
// usage_split_24h and federation — speaks snake_case. The in-flight
// counters of the model-key door (W01-8) went out as Inflight,
// InflightKeys, … because ModelAuthStats carried them without json tags.
// Two halves: (a) the types — every exported field reachable from the
// section types has an explicit snake_case json name, populated or not;
// (b) the wire — one server-admin stamp with every door counter non-zero,
// walked key by key, plus the six in-flight keys by name. No compatibility
// copy: the Go field names were a defect, not a contract.
func TestStatusGatewayKeysSnakeCase(t *testing.T) {
	t.Run("types", func(t *testing.T) {
		seen := map[reflect.Type]bool{}
		walkJSONKeyTypes(t, reflect.TypeFor[gatewayStatus](), "gateway", seen)
		walkJSONKeyTypes(t, reflect.TypeFor[federationStatus](), "federation", seen)
		walkJSONKeyTypes(t, reflect.TypeFor[llmusage.SplitRow](), "usage_split_24h", seen)
		if !seen[reflect.TypeFor[ModelAuthStats]()] || !seen[reflect.TypeFor[usageevents.Stats]()] {
			t.Fatal("the type walk never reached ModelAuthStats/usageevents.Stats — the walk is broken, not the keys")
		}
	})

	t.Run("wire", func(t *testing.T) {
		var door ModelAuthStats
		fillNonZero(reflect.ValueOf(&door).Elem())
		var ev usageevents.Stats
		fillNonZero(reflect.ValueOf(&ev).Elem())
		ring := NewConsumerRing(nil)
		ring.Add("c1", "t1", 10)
		c := &StatusCollector{}
		c.gateway.gen.Store(&gatewayGeneration{
			at:       time.Now(),
			byTenant: map[string]gatewayCounts{"t1": {consumers: 1, used1h: 1, expiring: 1}},
			split:    []llmusage.SplitRow{{Target: "x", Source: "y", Class: "interactive", Calls: 1, GPUMs: 1}},
			refusals: map[[2]string]int64{{"federation", "loop"}: 1},
		})
		c.SetGatewayFeed(&GatewayFeed{
			Enabled:    func() bool { return true },
			DoorStats:  func() ModelAuthStats { return door },
			Ring:       ring,
			Events:     func() usageevents.Stats { return ev },
			FedHop:     func() fedhop.Counters { return fedhop.Counters{StampWithoutIdentity: 1, HeaderIgnored: 1} },
			Federation: func() federation.Counters { return federation.Counters{PeerWithoutPath: 1} },
		})
		var resp statusResponse
		c.stampGateway(context.Background(), &resp, true, "")
		if resp.Gateway == nil || resp.Gateway.Auth == nil || resp.Federation == nil || resp.UsageSplit24h == nil {
			t.Fatalf("the server-admin stamp lacks a section: gateway=%v auth=%v federation=%v split=%v — the fixture is broken",
				resp.Gateway != nil, resp.Gateway != nil && resp.Gateway.Auth != nil, resp.Federation != nil, resp.UsageSplit24h != nil)
		}
		b, err := json.Marshal(struct {
			Gateway       *gatewayStatus       `json:"gateway"`
			UsageSplit24h *[]llmusage.SplitRow `json:"usage_split_24h"`
			Federation    *federationStatus    `json:"federation"`
		}{resp.Gateway, resp.UsageSplit24h, resp.Federation})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		walkJSONKeys(t, doc, "status")
		gw, _ := doc["gateway"].(map[string]any)
		auth, _ := gw["auth"].(map[string]any)
		for _, k := range []string{"inflight", "inflight_keys", "inflight_timers", "inflight_cancelled", "inflight_sweeps", "inflight_sweep_errors"} {
			if v, ok := auth[k]; !ok || v != float64(7) {
				t.Errorf("FOLGE-V gate (in-flight keys): gateway.auth[%q] = %v (present %v), want 7 — auth = %s", k, v, ok, mustJSON(t, auth))
			}
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// contractRouteKeys are the routes of conformance/oai/contract.json as
// "METHOD /prefix/pattern", with the forms each declares.
func contractRouteKeys(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile("../../conformance/oai/contract.json")
	if err != nil {
		t.Fatalf("contract manifest: %v", err)
	}
	var doc struct {
		Routes []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Forms  []struct {
				Form string `json:"form"`
			} `json:"forms"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("contract manifest: %v", err)
	}
	out := map[string][]string{}
	for _, r := range doc.Routes {
		var forms []string
		for _, f := range r.Forms {
			forms = append(forms, f.Form)
		}
		out[r.Method+" "+r.Path] = forms
	}
	return out
}

// mountedModelRoutes are the routes the production mount registers (the
// lists newRouter hands mountModelGroup), as "METHOD /prefix/pattern".
func mountedModelRoutes() []string {
	var out []string
	for _, g := range []struct {
		prefix string
		routes []modelRoute
	}{{"/v1", v1Routes(oai.Deps{})}, {"/v2", v2Routes(oai.Deps{})}} {
		for _, rt := range g.routes {
			out = append(out, rt.method+" "+g.prefix+rt.pattern)
		}
	}
	return out
}

// routeContractViolations compares the mounted routes with the manifest:
// every mounted route has a contract with a success and an error form, and
// every contract names a mounted route.
func routeContractViolations(mounted []string, contract map[string][]string) []string {
	var out []string
	for _, k := range mounted {
		forms, ok := contract[k]
		if !ok {
			out = append(out, fmt.Sprintf("route %s is mounted but has no contract in conformance/oai/contract.json", k))
			continue
		}
		for _, want := range []string{"success", "error"} {
			if !slices.Contains(forms, want) {
				out = append(out, fmt.Sprintf("route %s: the contract has no %q form", k, want))
			}
		}
	}
	for k := range contract {
		if !slices.Contains(mounted, k) {
			out = append(out, fmt.Sprintf("contract for %s names no mounted route", k))
		}
	}
	slices.Sort(out)
	return out
}

// TestContractCoversEveryModelRoute is W06-19's route census (design/06
// §4.14): the required CI job "Conformance" fails when a route of /v1 or /v2
// is mounted without a contract — its answer forms would reach clients
// unvalidated — or a contract outlives its route.
func TestContractCoversEveryModelRoute(t *testing.T) {
	mounted := mountedModelRoutes()
	if len(mounted) == 0 {
		t.Fatal("no model route mounted")
	}
	contract := contractRouteKeys(t)
	t.Logf("%d mounted model routes, %d contracts", len(mounted), len(contract))
	for _, v := range routeContractViolations(mounted, contract) {
		t.Error(v)
	}
	// Counterproof: a route mounted without a contract, and a contract
	// without its route.
	extra := append(slices.Clone(mounted), "POST /v1/responses")
	stale := map[string][]string{"POST /v1/completions": {"success", "error"}}
	for k, v := range contract {
		stale[k] = v
	}
	if v := routeContractViolations(extra, contract); !slices.ContainsFunc(v, func(s string) bool { return s == "route POST /v1/responses is mounted but has no contract in conformance/oai/contract.json" }) {
		t.Errorf("counterproof: a mounted route without a contract is not reported (%q)", v)
	}
	if v := routeContractViolations(mounted, stale); !slices.Contains(v, "contract for POST /v1/completions names no mounted route") {
		t.Errorf("counterproof: a contract without its route is not reported (%q)", v)
	}
}

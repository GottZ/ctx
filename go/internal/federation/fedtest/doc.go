// Package fedtest is the in-process federation mesh of the F-W9 gates and of
// the federation waves after it (F-W10, F-W11, F-W12): ctx instances, each an
// httptest server with its own fedhop.Identity, behind the model API ingress
// of A — a fake door that puts a ConsumerAuth of the configured kind into
// the context, handler.LoopGuard, federation.PeerIngress — whose handler
// filters its chain with federation.FilterChain and walks it with
// llm.WalkChain over a pass-through dispatcher. A ctx row calls another
// instance and stamps Tier F by hand (fedhop.Stamp with TierFederation —
// Tier F on B is F-W6); every other row calls a fake leaf (an engine or a
// capture endpoint, 200 or 429, optionally slow) with the Tier L stamp of
// its HopTarget. Every incoming request of an instance or a leaf is counted
// and captured (time, target, headers), so a gate can count wire calls per
// leaf and per instance.
//
// It lives outside internal/fedhop on purpose: fedhop is a stdlib-only leaf
// (.golangci.yml fedhop-leaf), the mesh needs handler, llm and dispatch. The
// mesh is test code only (mesh_helpers_test.go): this file is the package's
// one non-test file, so nothing of it can be linked into ctxd and no
// test-only symbol enters the deadcode allowlist; the federation waves after
// F-W9 add their mesh tests to this package. Headers are set through
// http.Header.Set and fedhop.Stamp only (canonical keys: a hand-built
// "ctx-hop" key is a 400 at ParseInbound).
package fedtest

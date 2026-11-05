package oai

// Test bridges of the hardening after Fremd-Review 2 (W02-07).

// RewriteChatLane exposes rewriteChat for the lane → service_tier pin (H-6:
// the bulk lane is served only after A03-W9, so the pin sits on the pure
// function, the lane as input).
var RewriteChatLane = rewriteChat

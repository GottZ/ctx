package oai

// pinParse is the parse the FOLGE-P pins measure: both halves, without a
// reservation (the route's ledger is pinned over the route).
func pinParse(body []byte, l policyLimits) *paramError {
	req, perr := parseTopLevel(body)
	if perr != nil {
		return perr
	}
	return req.parsePolicy(l, nil)
}

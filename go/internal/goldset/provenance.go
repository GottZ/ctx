package goldset

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrNotOnPrem aborts the build when the G-Q generator would be an external
// endpoint. G-Q questions are derived from the CONTENT of private blocks — the
// one point in this axis where block content reaches a model (design 04 §4.5).
// The rule is therefore not advisory: the build stops.
var ErrNotOnPrem = errors.New("generator endpoint is not on-prem")

// Backend mirrors the context_backends row fields the generator needs.
type Backend struct {
	Name      string          `json:"name"`
	BaseURL   string          `json:"base_url"`
	Locality  string          `json:"locality"` // local | lan | external
	Trust     string          `json:"trust"`
	Roles     []string        `json:"roles"`
	Enabled   bool            `json:"enabled"`
	ModelMap  json.RawMessage `json:"model_map"`
	ExtraBody json.RawMessage `json:"extra_body"`
}

// RequireOnPrem enforces the on-prem rule on TWO independent axes: the declared
// locality column and the actual host in base_url. A single mislabelled row
// would otherwise be enough to send private block content to a public API, and
// the registry column is editable state, not a proof.
func RequireOnPrem(b Backend) error {
	switch b.Locality {
	case "local", "lan":
	default:
		return fmt.Errorf("%w: backend %q declares locality=%q", ErrNotOnPrem, b.Name, b.Locality)
	}
	host, err := endpointHost(b.BaseURL)
	if err != nil {
		return fmt.Errorf("%w: backend %q: %w", ErrNotOnPrem, b.Name, err)
	}
	if !privateHost(host) {
		return fmt.Errorf("%w: backend %q resolves to public host %q despite locality=%q",
			ErrNotOnPrem, b.Name, host, b.Locality)
	}
	return nil
}

func endpointHost(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("base_url %q unparsable: %w", raw, err)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("base_url %q has no host", raw)
	}
	return u.Hostname(), nil
}

// privateHost reports whether a host is unambiguously inside the perimeter: a
// loopback/private/link-local IP, a bare container or service name without a
// dot, or a reserved local suffix. Anything else — every public DNS name
// included — is external.
func privateHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return true
	}
	if !strings.Contains(h, ".") {
		return true // docker service name on the compose network
	}
	for _, suffix := range []string{".local", ".lan", ".internal", ".localdomain", ".home.arpa"} {
		if strings.HasSuffix(h, suffix) {
			return true
		}
	}
	return false
}

// SliceProfile is the declared construction of one slice — the part of a
// measurement that a number alone cannot carry.
//
// Every field exists because a report reader has to be able to answer one
// question without reading the generator's code: what does this slice measure,
// and what is it biased towards? A slice whose bias is not declared here is a
// slice whose result cannot be attributed.
type SliceProfile struct {
	// Construction is how the cases were built.
	Construction string `json:"construction"`
	// GoldSource names where the gold labels come from — or that there are
	// none yet and why.
	GoldSource string `json:"gold_source"`
	// DeclaredBias is the distortion the slice is KNOWN to carry. It is stated
	// up front, not discovered afterwards in a review.
	DeclaredBias string `json:"declared_bias"`
	// RolloutCriterion is false for floor checks: slices that are measured and
	// reported but must never decide whether a layer ships.
	RolloutCriterion bool `json:"rollout_criterion"`
	// Population is the ground set this slice drew from (K9), in words plus the
	// count it resolved to at draw time.
	Population string `json:"population,omitempty"`
	// Generator pins model, endpoint and prompt hash of the questions. Nil for
	// slices no model wrote.
	Generator *Generator `json:"generator,omitempty"`
	// WindowRule is the session window definition (G-SESS only): stating it in
	// the stamp is what makes the declared "window definition" bias checkable.
	WindowRule string `json:"window_rule,omitempty"`
	// ConfidenceFloor is the dream-link floor (G-MH only).
	ConfidenceFloor float64 `json:"confidence_floor,omitempty"`
}

// sliceProfiles is the static half of every slice profile: the parts that are a
// property of the CONSTRUCTION, not of a particular run. The run fills in
// population, generator and the counts.
var sliceProfiles = map[string]SliceProfile{
	SliceKI: {
		Construction:     "query = lightly paraphrased block title, gold = that very block",
		GoldSource:       "constructive (the paraphrased block)",
		DeclaredBias:     "strongly trigram- and title-FTS-friendly; floor check only",
		RolloutCriterion: false,
	},
	SliceQ: {
		Construction:     "on-prem model writes one question the block BODY answers",
		GoldSource:       "constructive (the source block)",
		DeclaredBias:     "LLM-generated by the same serving that writes derived layers — endogenous; split into DERIV/HOLD",
		RolloutCriterion: true,
	},
	SliceReal: {
		Construction:     "distinct real query texts from the access log, redaction sweep applied",
		GoldSource:       "human pooled judgements (wave B-W6)",
		DeclaredBias:     "pooling bias; the only slice with external validity",
		RolloutCriterion: true,
	},
	SliceSess: {
		Construction:     "one question per session window; the window is a calendar day (or a span of days) that carries at least one daily report",
		GoldSource:       "constructive: the daily reports of the window plus the knowledge blocks created inside it",
		DeclaredBias:     "window definition; favours recency-shaped retrieval. NOT circular against the insight layer — no gold is taken from insights",
		RolloutCriterion: true,
	},
	SliceMH: {
		Construction:     "two blocks bridged by a dream link at confidence >= 0.7; the question needs both",
		GoldSource:       "constructive: both endpoints of the link",
		DeclaredBias:     "favours graph expansion. The floor is empirical: the dream-link audit measures 56 % correctness overall but 100 % at confidence >= 0.7",
		RolloutCriterion: true,
	},
	SliceGlob: {
		Construction:     "aggregating question over a corpus tag with enough retrievable blocks",
		GoldSource:       "none yet — judged from a pool (E-9), like G-REAL",
		DeclaredBias:     "pooling bias. Drawn from TAGS, not from clusters, so the questions are not shaped by the graph layer they are meant to test",
		RolloutCriterion: true,
	},
	SliceGlobKonstr: {
		Construction:     "aggregating question over a cluster label; gold = the cluster's retrievable members",
		GoldSource:       "constructive: graph_cluster_member",
		DeclaredBias:     "CIRCULAR against the graph layer — a catalog block IS the cluster. Floor check only, never a rollout criterion",
		RolloutCriterion: false,
	},
}

// ProfileFor returns the declared construction profile of a slice.
func ProfileFor(slice string) (SliceProfile, bool) {
	p, ok := sliceProfiles[slice]
	return p, ok
}

// RequireOnPremStamp asserts the on-prem rule over everything a stamp records:
// the top-level generator and every per-slice generator. It is the write-time
// half of RequireOnPrem — the call-time check protects the call, this one
// protects the record.
func RequireOnPremStamp(s Stamp) error {
	check := func(where string, g *Generator) error {
		if g == nil {
			return nil
		}
		if err := RequireOnPrem(Backend{
			Name: g.Backend, BaseURL: g.Endpoint, Locality: g.Locality, Trust: g.Trust,
		}); err != nil {
			return fmt.Errorf("stamp %s: %w", where, err)
		}
		return nil
	}
	if err := check("generator", s.Generator); err != nil {
		return err
	}
	for _, name := range SliceNames(s) {
		st := s.Slices[name]
		if st.Profile == nil {
			continue
		}
		if err := check("slice "+name, st.Profile.Generator); err != nil {
			return err
		}
	}
	return nil
}

// DefaultModel reads model_map.default.model — the model id the generator must
// stamp. Falls back to the empty string so the caller can require an override.
func (b Backend) DefaultModel() string {
	var m map[string]struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(b.ModelMap, &m); err != nil {
		return ""
	}
	return m["default"].Model
}

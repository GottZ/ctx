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

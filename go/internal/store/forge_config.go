package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

// ForgeConfig is a project's forge config, context_projects.forge:
// {kind, owner, repo, api_base?} (docs/api.md). Every reader of the column
// reads it through ReadForgeConfig, and every API write stores the form
// ParseForgeConfig decoded (issue #474).
type ForgeConfig struct {
	Kind    string `json:"kind,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Repo    string `json:"repo,omitempty"`
	APIBase string `json:"api_base,omitempty"`
}

// forgeConfigKeys are the keys of a forge config, in the spelling the API
// takes.
var forgeConfigKeys = [...]string{"kind", "owner", "repo", "api_base"}

var (
	// ErrForgeConfigShape: the value is not a JSON object of string values.
	ErrForgeConfigShape = errors.New("forge must be a JSON object whose values are strings")
	// ErrForgeConfigKeys: a write named a key outside kind, owner, repo and
	// api_base (another spelling of one of them included), or a key twice.
	ErrForgeConfigKeys = errors.New("forge takes only the keys kind, owner, repo and api_base, each once and in lower case")
	// ErrForgeConfigSpellings: a stored value names one key in more than one
	// spelling (a row the API took before issue #474).
	ErrForgeConfigSpellings = errors.New("forge names a key in more than one spelling")
)

// absentForge reports a forge value that configures nothing: none, or JSON
// null (stored as it came, as before).
func absentForge(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

// ReadForgeConfig is the one reading of a stored forge value — the sync run
// (its repository, its API base, its throttle key) and the project API's
// answers read it this way. A key matches in any spelling, as encoding/json
// matched it before (a row with only "API_BASE" keeps its API base); a key
// the row names in two spellings (JSONB keeps "api_base" and "API_BASE"
// apart) is ErrForgeConfigSpellings, and a value that is not a string is
// ErrForgeConfigShape — the run refuses such a row, and the answers leave
// that key out. Other keys are not read. The config holds every key that
// read cleanly, also with an error.
//
// Before, the run decoded the column with encoding/json, which takes the
// last of the spellings in JSONB's key order, while GET /api/project served
// the column's text: a row with credentials in one spelling and a clean URL
// in the other synced against one and served the other (issue #474).
func ReadForgeConfig(raw json.RawMessage) (ForgeConfig, error) {
	var c ForgeConfig
	if absentForge(raw) {
		return c, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return c, ErrForgeConfigShape
	}
	var first error
	for _, name := range forgeConfigKeys {
		var val json.RawMessage
		n := 0
		for k, v := range m {
			if strings.EqualFold(k, name) {
				n++
				val = v
			}
		}
		var s *string
		switch {
		case n == 0:
			continue
		case n > 1:
			first = cmpErr(first, ErrForgeConfigSpellings)
			continue
		case json.Unmarshal(val, &s) != nil:
			first = cmpErr(first, ErrForgeConfigShape)
			continue
		case s != nil:
			c.set(name, *s)
		}
	}
	return c, first
}

func cmpErr(first, err error) error {
	if first != nil {
		return first
	}
	return err
}

func (c *ForgeConfig) set(name, v string) {
	switch name {
	case "kind":
		c.Kind = v
	case "owner":
		c.Owner = v
	case "repo":
		c.Repo = v
	case "api_base":
		c.APIBase = v
	}
}

// ParseForgeConfig decodes a forge value an API write carries — project
// create, PATCH and project-provision — and answers the form to store: the
// decoded config, serialised (raw itself when it configures nothing: none,
// or JSON null). A key outside kind, owner, repo and api_base — another
// spelling of one of them included — or a key named twice is
// ErrForgeConfigKeys, a value other than a string or null
// ErrForgeConfigShape. What is checked is what is stored: before, the API
// checked an encoding/json decode of the request and stored the request's
// text, and a second spelling of api_base carried a value the check never
// saw (issue #474).
func ParseForgeConfig(raw json.RawMessage) (ForgeConfig, json.RawMessage, error) {
	var c ForgeConfig
	if absentForge(raw) {
		return c, raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return c, nil, ErrForgeConfigShape
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return c, nil, ErrForgeConfigShape
		}
		k, _ := tok.(string)
		if seen[k] || !isForgeConfigKey(k) {
			return c, nil, ErrForgeConfigKeys
		}
		seen[k] = true
		var s *string
		if err := dec.Decode(&s); err != nil {
			return c, nil, ErrForgeConfigShape
		}
		if s != nil {
			c.set(k, *s)
		}
	}
	if _, err := dec.Token(); err != nil {
		return c, nil, ErrForgeConfigShape
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return c, nil, ErrForgeConfigShape
	}
	out, err := json.Marshal(c)
	if err != nil {
		return c, nil, err
	}
	return c, out, nil
}

func isForgeConfigKey(k string) bool {
	for _, name := range forgeConfigKeys {
		if k == name {
			return true
		}
	}
	return false
}

// ServedForgeConfig is the forge value the project API answers with for the
// stored raw (ProjectRow.MarshalJSON): raw itself when it is in the form an
// API write stores now, else the config ReadForgeConfig reads from it, with
// the userinfo of its api_base removed (an api_base that does not parse as a
// URL is left out). A row the API took before issue #474 — credentials in
// api_base, a key in two spellings, a key outside the config — so serves
// what the sync run reads, without credentials: the row's token_secret is
// never on the wire either.
func ServedForgeConfig(raw json.RawMessage) json.RawMessage {
	if absentForge(raw) {
		return raw
	}
	if c, _, err := ParseForgeConfig(raw); err == nil && !apiBaseHasUserinfo(c.APIBase) {
		return raw
	}
	c, _ := ReadForgeConfig(raw)
	if c.APIBase != "" {
		u, err := url.Parse(c.APIBase)
		if err != nil {
			c.APIBase = ""
		} else {
			u.User = nil
			c.APIBase = u.String()
		}
	}
	out, _ := json.Marshal(c)
	return out
}

// apiBaseHasUserinfo reports an api_base with user:password@ (or one that
// does not parse as a URL, so that no userinfo can be ruled out).
func apiBaseHasUserinfo(s string) bool {
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	return err != nil || u.User != nil
}

// MarshalJSON serves the row with its forge as ServedForgeConfig reads it
// (issue #474); every other column as it is.
func (p ProjectRow) MarshalJSON() ([]byte, error) {
	type wire ProjectRow
	w := wire(p)
	w.Forge = ServedForgeConfig(p.Forge)
	return json.Marshal(w)
}

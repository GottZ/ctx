package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Issue #474: one reading of context_projects.forge for every reader, and an
// API write stores the form it checked.
func TestParseForgeConfigStoresTheFormItChecked(t *testing.T) {
	for _, c := range []struct {
		name, in, stored string
		err              error
	}{
		{"the documented form", `{"kind":"github","owner":"o","repo":"r","api_base":"https://ghe.example.com/api/v3"}`, `{"kind":"github","owner":"o","repo":"r","api_base":"https://ghe.example.com/api/v3"}`, nil},
		{"key order and spaces", `{ "repo" : "r", "owner":"o" }`, `{"owner":"o","repo":"r"}`, nil},
		{"a null value", `{"owner":"o","repo":"r","api_base":null}`, `{"owner":"o","repo":"r"}`, nil},
		{"empty", `{}`, `{}`, nil},
		{"null", `null`, `null`, nil},
		{"absent", ``, ``, nil},
		{"a second spelling", `{"owner":"o","repo":"r","api_base":"https://h/x","API_BASE":"https://u:p@h/x"}`, "", ErrForgeConfigKeys},
		{"only another spelling", `{"owner":"o","repo":"r","Api_Base":"https://h/x"}`, "", ErrForgeConfigKeys},
		{"a key twice", `{"owner":"o","owner":"p","repo":"r"}`, "", ErrForgeConfigKeys},
		{"a key outside the config", `{"owner":"o","repo":"r","token":"t"}`, "", ErrForgeConfigKeys},
		{"a number", `{"owner":"o","repo":5}`, "", ErrForgeConfigShape},
		{"an object value", `{"owner":{"a":"b"},"repo":"r"}`, "", ErrForgeConfigShape},
		{"an array", `["owner"]`, "", ErrForgeConfigShape},
		{"a string", `"owner"`, "", ErrForgeConfigShape},
		{"text after the object", `{"owner":"o"} {"api_base":"https://u:p@h/x"}`, "", ErrForgeConfigShape},
	} {
		_, stored, err := ParseForgeConfig(json.RawMessage(c.in))
		if !errors.Is(err, c.err) || (c.err == nil && string(stored) != c.stored) {
			t.Errorf("%s: stored %q, err %v — want %q, %v", c.name, stored, err, c.stored, c.err)
		}
	}
}

func TestReadForgeConfigIsTheRunsReading(t *testing.T) {
	c, err := ReadForgeConfig(json.RawMessage(`{"API_BASE": "https://ghe.example.com/api/v3", "repo": "r", "owner": "o", "note": "x"}`))
	if err != nil || c.APIBase != "https://ghe.example.com/api/v3" || c.Owner != "o" || c.Repo != "r" {
		t.Errorf("a row with api_base in one other spelling: %+v %v — want it read, as encoding/json read it", c, err)
	}
	c, err = ReadForgeConfig(json.RawMessage(`{"API_BASE": "https://u:p@h/x", "api_base": "https://h/x", "repo": "r", "owner": "o"}`))
	if !errors.Is(err, ErrForgeConfigSpellings) || c.APIBase != "" || c.Owner != "o" || c.Repo != "r" {
		t.Errorf("a row with api_base in two spellings: %+v %v — want ErrForgeConfigSpellings, the other keys read", c, err)
	}
	if _, err := ReadForgeConfig(json.RawMessage(`{"owner": 5}`)); !errors.Is(err, ErrForgeConfigShape) {
		t.Errorf("a number: %v", err)
	}
	if c, err := ReadForgeConfig(json.RawMessage(`null`)); err != nil || c != (ForgeConfig{}) {
		t.Errorf("null: %+v %v", c, err)
	}
}

func TestServedForgeConfigServesWhatTheRunReads(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		// JSONB's own text of a row in the documented form: served as stored.
		{"the documented form", `{"kind": "github", "repo": "r", "owner": "o", "api_base": "https://ghe.example.com/api/v3"}`, `{"kind": "github", "repo": "r", "owner": "o", "api_base": "https://ghe.example.com/api/v3"}`},
		{"null", `null`, `null`},
		{"credentials", `{"repo": "r", "owner": "o", "api_base": "https://user:secret@ghe.example.com/api/v3"}`, `{"owner":"o","repo":"r","api_base":"https://ghe.example.com/api/v3"}`},
		{"two spellings", `{"repo": "r", "owner": "o", "API_BASE": "https://user:secret@h/x", "api_base": "https://h/x"}`, `{"owner":"o","repo":"r"}`},
		{"one other spelling", `{"repo": "r", "owner": "o", "API_BASE": "https://user:secret@h/x"}`, `{"owner":"o","repo":"r","api_base":"https://h/x"}`},
		{"a key outside the config", `{"repo": "r", "owner": "o", "token": "secret"}`, `{"owner":"o","repo":"r"}`},
		{"an api_base that does not parse", `{"repo": "r", "owner": "o", "api_base": "https://user:secret@h/%zz"}`, `{"owner":"o","repo":"r"}`},
	} {
		if got := string(ServedForgeConfig(json.RawMessage(c.in))); got != c.want {
			t.Errorf("%s: served %s, want %s", c.name, got, c.want)
		}
	}
	ts := "forge.token.x"
	b, err := json.Marshal([]ProjectRow{{ID: "p", Forge: json.RawMessage(`{"owner": "o", "api_base": "https://user:secret@h/x"}`), TokenSecret: &ts}})
	if err != nil || strings.Contains(string(b), "user:secret") || strings.Contains(string(b), "forge.token") || !strings.Contains(string(b), `"forge":{"owner":"o","api_base":"https://h/x"}`) {
		t.Errorf("a project row on the wire: %s %v", b, err)
	}
}

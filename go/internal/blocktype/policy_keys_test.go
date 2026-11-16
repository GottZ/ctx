package blocktype

import (
	"strings"
	"testing"
)

// TestDecodePolicyKeysReadAlikeInGoAndSQL pins checkConfigKeys (#291, third
// review round): a config whose keys encoding/json would read differently
// from the database's jsonb operators is refused, at every level of the
// envelope; a config both read alike still decodes.
//
// RED before the check: every "rejects" case decoded (the strict decoder
// matches a mis-spelled key to its field and merges a repeated one), and the
// first five decoded to retrieval.policy=excluded while SQL reads no policy
// from the stored jsonb.
func TestDecodePolicyKeysReadAlikeInGoAndSQL(t *testing.T) {
	rejects := []struct {
		name, cfg, want string
	}{
		{"section spelled with a capital",
			`{"v":1,"Retrieval":{"policy":"excluded"}}`, `key "Retrieval" is not spelled like its field "retrieval"`},
		{"leaf spelled with a capital",
			`{"v":1,"retrieval":{"Policy":"excluded"}}`, `key "retrieval.Policy" is not spelled like its field "policy"`},
		{"both spellings of a section",
			`{"v":1,"retrieval":{"untrusted":true},"Retrieval":{"policy":"excluded"}}`, `key "Retrieval" is not spelled`},
		{"repeated section merges in the decoder",
			`{"v":1,"retrieval":{"policy":"excluded"},"retrieval":{"untrusted":true}}`, `key "retrieval" repeats in one object`},
		{"repeated section, the copy SQL keeps names no policy",
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},"retrieval":{"untrusted":true}}`, `key "retrieval" repeats`},
		{"repeated leaf",
			`{"v":1,"retrieval":{"policy":"excluded","policy":"full-pass"}}`, `key "retrieval.policy" repeats`},
		{"version spelled with a capital",
			`{"V":1}`, `key "V" is not spelled like its field "v"`},
		{"repeated version",
			`{"v":1,"v":1}`, `key "v" repeats`},
		{"nested section of write",
			`{"v":1,"write":{"Internal_Only":true}}`, `key "write.Internal_Only" is not spelled like its field "internal_only"`},
		{"array-valued field",
			`{"v":1,"Structural_Link_Classes":["references"]}`, `key "Structural_Link_Classes" is not spelled`},
		{"key of a map repeats",
			`{"v":1,"workflow":{"states":["a","b"],"initial":"a","forge_state_map":{"open":"a","open":"b"}}}`, `key "workflow.forge_state_map.open" repeats`},
	}
	for _, c := range rejects {
		t.Run("rejects/"+c.name, func(t *testing.T) {
			_, err := DecodePolicy("kc-type", "tenant-a", false, false, []byte(c.cfg))
			if err == nil {
				t.Fatalf("config %s decoded; it reads differently in Go and in SQL", c.cfg)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to contain %q", err, c.want)
			}
		})
	}

	accepts := []struct {
		name, cfg string
		kind      string
	}{
		// A JSON escape is the same key for both readers: jsonb decodes it too.
		{"escaped spelling of a key", `{"v":1,"retrieval":{"policy":"excluded"}}`, RetrievalExcluded},
		{"workflow with its map", issueWorkflowCfg, RetrievalFullPass},
		{"the shipped checkpoint envelope",
			`{"v": 1, "dream": {"linkable": false}, "guard": {"check": false, "candidate": false}, "digest": {"include": false}, ` +
				`"parent": {"mode": "none"}, "classify": {"priority": 30, "title_patterns": ["compaction source", "compaction checkpoint"]}, ` +
				`"overview": {"include": false}, "retrieval": {"policy": "excluded", "untrusted": true}}`, RetrievalExcluded},
	}
	for _, c := range accepts {
		t.Run("accepts/"+c.name, func(t *testing.T) {
			p, err := DecodePolicy("kc-type", "tenant-a", false, false, []byte(c.cfg))
			if err != nil {
				t.Fatalf("config %s refused: %v", c.cfg, err)
			}
			if p.Retrieval.Kind != c.kind {
				t.Fatalf("retrieval.policy = %q, want %q", p.Retrieval.Kind, c.kind)
			}
		})
	}
}

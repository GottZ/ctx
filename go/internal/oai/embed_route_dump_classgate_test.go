package oai_test

// The class gate's bodies in the route dump (FOLGE-E hardening 1, part (c):
// the upstream bodies of the class gate compared between trees). Copied
// into the trees that have the class gate (embed_class_gate_test.go); the
// dump stays a comparison tool, skipped without CTX_FOLGEE_DUMP.

import (
	"fmt"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

func init() {
	rdumpExtra = func(model string) []rdumpItem {
		var out []rdumpItem
		for _, name := range oai.EmbedClassGateCombos() {
			for _, size := range oai.EmbedClassGateSizes {
				if b, ok := oai.EmbedClassGateBody(name, model, size); ok {
					out = append(out, rdumpItem{key: fmt.Sprintf("ecg:%s:%d", name, size), body: b, logs: []string{llmlog.BodyLoggingNone}})
				}
			}
		}
		return out
	}
}

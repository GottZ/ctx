package handler

// FOLGE-Z Z1: the capability probe's result per pool row on the admin
// /api/status dispatch targets — the rows behind each origin with what their
// engine reported (backends.Pool.Capabilities). Server-admin path only
// (assemble); the tenant snapshot never carries it.

import (
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// dispatchTargetCapability is one pool row behind a dispatch origin.
type dispatchTargetCapability struct {
	Backend      string                `json:"backend"`
	Capabilities backends.Capabilities `json:"capabilities"`
}

// dispatchTargetCapabilitiesOf groups the pool rows by normalized origin
// with their probe results (rows in pool order: scope, name).
func dispatchTargetCapabilitiesOf(bp *backends.Pool) map[string][]dispatchTargetCapability {
	rows := bp.Snapshot()
	out := make(map[string][]dispatchTargetCapability)
	for i := range rows {
		origin, err := dispatch.NormalizeOrigin(rows[i].Host)
		if err != nil {
			continue
		}
		out[origin] = append(out[origin], dispatchTargetCapability{Backend: rows[i].Name, Capabilities: bp.Capabilities(rows[i].ID)})
	}
	return out
}

// withTargetCapabilities attaches caps to the targets of ds by origin; a
// target without rows gets an empty list.
func withTargetCapabilities(ds *dispatchStatus, caps map[string][]dispatchTargetCapability) {
	if ds == nil {
		return
	}
	for i := range ds.Targets {
		rows := caps[ds.Targets[i].Origin]
		if rows == nil {
			rows = []dispatchTargetCapability{}
		}
		ds.Targets[i].Capabilities = rows
	}
}

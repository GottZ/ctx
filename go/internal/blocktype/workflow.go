package blocktype

import (
	"errors"
	"fmt"
)

// Workflow transition error classes. Handlers (I-D2 issue-update, later write
// surfaces W7) map ALL three to a 422 — an invalid transition is a client
// request error, not a server fault. They are distinct sentinels so the caller
// can log/branch (unknown type vs. no workflow vs. bad transition) without
// string-matching.
var (
	// ErrUnknownType — the type name is not registered in the resolved Set.
	ErrUnknownType = errors.New("blocktype: unknown type")
	// ErrNoWorkflow — the type exists but carries no workflow states (the whole
	// knowledge corpus): no status transition is meaningful.
	ErrNoWorkflow = errors.New("blocktype: type has no workflow")
	// ErrInvalidTransition — from/to are not a valid transition for the type.
	ErrInvalidTransition = errors.New("blocktype: invalid workflow transition")
)

// ValidateTransition validates a workflow status transition for typeName
// against POLICY DATA (the registry Set) — never a hardcoded status list. This
// is the mechanism (Go); the state SET, entry point and terminal flags are the
// policy (Achse-01 type config, mig-077 workflow_status is the per-block value).
//
// Rules (design/02 §4.1 form; the transition graph is complete over the
// configured states, terminal is NOT an outgoing constraint — reopen closed→open
// is real, §4.5.4):
//
//   - type unregistered            ⇒ ErrUnknownType
//   - type has no workflow states  ⇒ ErrNoWorkflow
//   - to ∉ States                  ⇒ ErrInvalidTransition
//   - from != "" AND from ∉ States ⇒ ErrInvalidTransition
//   - from == "" (entering)        ⇒ ok (any configured target status)
//   - otherwise                    ⇒ ok (including from == to; an idempotent
//     re-set is a legal no-op, the write path may short-circuit it)
//
// Because the rule keys off States, changing the registry status SET changes the
// validation outcome with NO Go rebuild (policy=data proof, I-B gate).
func (s *Set) ValidateTransition(typeName, from, to string) error {
	p, ok := s.Resolve(typeName)
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownType, typeName)
	}
	states := p.Workflow.States
	if len(states) == 0 {
		return fmt.Errorf("%w: %q", ErrNoWorkflow, typeName)
	}
	if !containsStr(states, to) {
		return fmt.Errorf("%w: %q → %q for type %q (target not a configured status)", ErrInvalidTransition, from, to, typeName)
	}
	if from != "" && !containsStr(states, from) {
		return fmt.Errorf("%w: %q → %q for type %q (source not a configured status)", ErrInvalidTransition, from, to, typeName)
	}
	return nil
}

// WorkflowStates returns the ordered board-column status set for the type
// (nil for a non-workflow/unknown type). The slice is shared with the immutable
// Set — callers must not mutate it. It is the per-status-merge input for
// store.ListWorkflowBlocks (the status-UNGEFILTERTE board list, §3.3).
func (s *Set) WorkflowStates(typeName string) []string {
	p, ok := s.Resolve(typeName)
	if !ok {
		return nil
	}
	return p.Workflow.States
}

// WorkflowInitial returns the entry status of the type ("" for a non-workflow
// type). The write path (I-D2 issue-create) stamps it on a new block.
func (s *Set) WorkflowInitial(typeName string) string {
	p, ok := s.Resolve(typeName)
	if !ok {
		return ""
	}
	return p.Workflow.Initial
}

// ForgeStatusFor maps a forge state (open/closed) to the ctx workflow status via
// the type's forge_state_map (§4.5.4). ok=false when the type has no workflow or
// the forge state is unmapped — the sync path then falls back to metadata-only
// (§4.5.4 fail-safe), never guessing a status.
func (s *Set) ForgeStatusFor(typeName, forgeState string) (string, bool) {
	p, ok := s.Resolve(typeName)
	if !ok || p.Workflow.ForgeStateMap == nil {
		return "", false
	}
	status, ok := p.Workflow.ForgeStateMap[forgeState]
	return status, ok
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

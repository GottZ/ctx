package events

// MW3 binding probes on the scheduler side of the classification table
// (design/01 §4.6 N1/N8a): every scheduler arm — dream cycle, 03:00 daily
// synthesis (both through newRouter) and the sensitivity-audit classify —
// binds background. Since MW4 (design/03 §4.1.1) the principal is not part
// of the Admission at all: it derives from the Acquire ctx, and the
// scheduler's detached contexts resolve empty — structurally background, B8.

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
)

// TestBackgroundAdmissionBinding pins the scheduler's dispatch binding:
// class background, admitter from SetDispatcher — and the typed-nil guard
// (a scheduler without SetDispatcher yields a nil Admitter interface, never
// a typed-nil pointer that would panic on Acquire).
func TestBackgroundAdmissionBinding(t *testing.T) {
	s := NewScheduler(nil, nil, nil, StartupConfig{})

	adm := s.backgroundAdmission()
	if adm.Admitter != nil {
		t.Fatalf("without SetDispatcher the admitter must be interface-nil, got %T", adm.Admitter)
	}
	if adm.Class != dispatch.ClassBackground {
		t.Fatalf("scheduler binding = class %v, want background", adm.Class)
	}

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s.SetDispatcher(d)
	adm = s.backgroundAdmission()
	if adm.Admitter == nil || adm.Class != dispatch.ClassBackground {
		t.Fatalf("with SetDispatcher: admitter=%v class=%v, want live background admission", adm.Admitter, adm.Class)
	}
}

// TestNewRouterBindsBackgroundAdmission pins the dream/daily scheduler paths
// (N1 dream router seams + N8a 03:00 path): the per-cycle router carries the
// scheduler's background admission.
func TestNewRouterBindsBackgroundAdmission(t *testing.T) {
	s := NewScheduler(nil, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	s.SetDispatcher(d)

	r := s.newRouter(config.NewStore(&config.Config{}).Snapshot(), "tenant-a")
	if r.Admit.Class != dispatch.ClassBackground {
		t.Fatalf("dream router class = %v, want background", r.Admit.Class)
	}
	if r.Admit.Admitter == nil {
		t.Fatal("dream router carries no admitter")
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/GottZ/ctx/internal/clientkey"
)

// refusalStop ends a parallel generation on the first refused redirect
// (#459). A refusal of internal/clientkey is the configuration of the
// -backend row — every call goes to the same URL and meets it alike — so
// counting it per block and going on would write a slice in which every block
// is discarded, and report a misconfigured backend as a model that answers
// nothing. ctx-goldbench and ctx-armsweep end their runs the same way.
type refusalStop struct {
	cancel context.CancelFunc
	once   sync.Once
	mu     sync.Mutex
	first  error
}

func newRefusalStop(parent context.Context) (context.Context, *refusalStop) {
	ctx, cancel := context.WithCancel(parent)
	return ctx, &refusalStop{cancel: cancel}
}

// record reports whether err is a refused redirect; the first one is kept
// and cancels the generation's context, so the calls in flight end too.
func (s *refusalStop) record(err error) bool {
	if !errors.As(err, new(*clientkey.Refusal)) {
		return false
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.first = err
		s.mu.Unlock()
		s.cancel()
	})
	return true
}

// stopped reports whether a refusal has ended the generation.
func (s *refusalStop) stopped() bool { return s.err() != nil }

// err is the refusal that ended the generation, nil if none did.
func (s *refusalStop) err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first == nil {
		return nil
	}
	return fmt.Errorf("generation stopped: %w", s.first)
}

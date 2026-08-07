package sandbox

import (
	"context"
	"sync"
)

// volumeHealthRelay closes the startup/runtime race for a volume backend.
// Before activation it cancels sandbox creation and retains the first failure.
// Activation publishes the ready sandbox while holding the relay lock, then
// exposes its handler; a concurrent report therefore cannot clean a sandbox
// before its ready state and tracking have been published.
type volumeHealthRelay struct {
	mu      sync.Mutex
	cancel  context.CancelCauseFunc
	failure error
	handler func(error)
}

func newVolumeHealthRelay(cancel context.CancelCauseFunc) *volumeHealthRelay {
	return &volumeHealthRelay{cancel: cancel}
}

func (r *volumeHealthRelay) report(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	if r.failure != nil {
		r.mu.Unlock()
		return
	}
	r.failure = err
	cancel := r.cancel
	handler := r.handler
	r.mu.Unlock()

	if cancel != nil {
		cancel(err)
	}
	if handler != nil {
		handler(err)
	}
}

func (r *volumeHealthRelay) activate(handler func(error), publish func()) error {
	if r == nil {
		if publish != nil {
			publish()
		}
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if publish != nil {
		publish()
	}
	r.handler = handler
	return nil
}

func (r *volumeHealthRelay) err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.failure
}

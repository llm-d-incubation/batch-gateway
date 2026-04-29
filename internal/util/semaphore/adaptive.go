/*
Copyright 2026 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package semaphore

import (
	"context"
	"sync"
)

// AdaptiveSemaphore implements the Semaphore interface with a dynamically
// adjustable concurrency limit. Unlike the channel-based semaphore, its
// capacity can be changed at runtime via SetLimit.
//
// Internally it uses sync.Mutex + sync.Cond instead of a buffered channel,
// because Go channels cannot be resized after creation.
type AdaptiveSemaphore struct {
	mu              sync.Mutex
	cond            *sync.Cond
	limit           int // current max concurrent acquires
	count           int // currently held tokens
	onDoubleRelease func()
	doubleOnce      sync.Once
}

// Compile-time check: AdaptiveSemaphore implements Semaphore.
var _ Semaphore = (*AdaptiveSemaphore)(nil)

// NewAdaptive creates a new AdaptiveSemaphore with the specified initial capacity.
// onDoubleRelease, if non-nil, is called at most once when Release is called
// on an already-empty semaphore (more releases than acquires).
func NewAdaptive(capacity int, onDoubleRelease func()) (*AdaptiveSemaphore, error) {
	if capacity <= 0 {
		return nil, ErrCap
	}
	s := &AdaptiveSemaphore{
		limit:           capacity,
		onDoubleRelease: onDoubleRelease,
	}
	s.cond = sync.NewCond(&s.mu)
	return s, nil
}

// Acquire blocks until a token is available or the context is cancelled.
func (s *AdaptiveSemaphore) Acquire(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Fast path: slot available, no waiting needed.
	if s.count < s.limit {
		s.count++
		return nil
	}

	// Slow path: must wait. Spawn a goroutine to broadcast on context
	// cancellation so that Wait() unblocks. The goroutine lives until either
	// ctx is cancelled or Acquire returns (closing done). With a non-cancellable
	// context (e.g. context.Background), this goroutine persists until a slot
	// becomes available — acceptable since the caller is blocked anyway.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.cond.Broadcast()
		case <-done:
		}
	}()

	for s.count >= s.limit {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.cond.Wait() // releases and reacquires s.mu
	}
	s.count++
	return nil
}

// Release returns a token to the semaphore. If called more times than
// Acquire, the onDoubleRelease callback fires at most once.
//
// The callback is invoked outside the mutex to avoid deadlock if the
// callback interacts with other locks (same pattern as AIMDController.setFn).
func (s *AdaptiveSemaphore) Release() {
	var fireCallback bool

	s.mu.Lock()
	if s.count > 0 {
		s.count--
		s.cond.Signal()
		s.mu.Unlock()
		return
	}
	if s.onDoubleRelease != nil {
		fireCallback = true
	}
	s.mu.Unlock()

	if fireCallback {
		s.doubleOnce.Do(s.onDoubleRelease)
	}
}

// TryAcquire attempts to acquire a token without blocking.
// Returns true if a token was acquired, false otherwise.
func (s *AdaptiveSemaphore) TryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.count < s.limit {
		s.count++
		return true
	}
	return false
}

// SetLimit dynamically adjusts the concurrency limit.
// The new limit is clamped to a minimum of 1 to prevent deadlock.
// If the limit increases, blocked Acquire callers are woken to compete
// for the new slots. If the limit decreases below the current count,
// no in-flight holders are interrupted; the limit takes effect as
// tokens are released.
func (s *AdaptiveSemaphore) SetLimit(newLimit int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if newLimit < 1 {
		newLimit = 1
	}
	s.limit = newLimit
	s.cond.Broadcast()
}

// Limit returns the current concurrency limit.
func (s *AdaptiveSemaphore) Limit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.limit
}

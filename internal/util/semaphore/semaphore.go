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

// Package semaphore provides a semaphore implementation for controlling
// concurrent access to a limited resource.
package semaphore

import (
	"context"
	"errors"

	"k8s.io/klog/v2"
)

// ErrCap is returned when attempting to create a semaphore with invalid capacity.
var ErrCap = errors.New("semaphore capacity must be positive")

// Semaphore is an interface for controlling concurrent access to a limited resource.
type Semaphore interface {
	// Acquire attempts to acquire a token from the semaphore.
	// It blocks until a token is available or the context is cancelled.
	// Returns an error if the context is cancelled.
	Acquire(ctx context.Context) error

	// Release releases a token back to the semaphore.
	// It should be called after Acquire when the resource is no longer needed.
	Release()

	// TryAcquire attempts to acquire a token without blocking.
	// Returns true if a token was acquired, false otherwise.
	TryAcquire() bool
}

// semaphore implements the Semaphore interface using a buffered channel.
// Compile-time check: semaphore implements Semaphore.
var _ Semaphore = (*semaphore)(nil)

type semaphore struct {
	tokens chan struct{}
}

// New creates a new semaphore with the specified capacity.
// The capacity determines the maximum number of concurrent acquisitions.
func New(capacity int) (Semaphore, error) {
	if capacity <= 0 {
		return nil, ErrCap
	}
	return &semaphore{
		tokens: make(chan struct{}, capacity),
	}, nil
}

// Acquire attempts to acquire a token from the semaphore.
func (s *semaphore) Acquire(ctx context.Context) error {
	select {
	case s.tokens <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release releases a token back to the semaphore.
func (s *semaphore) Release() {
	select {
	case <-s.tokens:
	default:
		// This should not happen in correct usage.
		klog.Background().Error(nil, "CRITICAL: token channel is full, skipping release")
	}
}

// TryAcquire attempts to acquire a token without blocking.
func (s *semaphore) TryAcquire() bool {
	select {
	case s.tokens <- struct{}{}:
		return true
	default:
		return false
	}
}

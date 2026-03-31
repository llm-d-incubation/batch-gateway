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

// Package retry provides a shared retry configuration and exponential backoff
// helper backed by github.com/cenkalti/backoff/v5.
//
// It wraps cenkalti/backoff to add an onRetry callback that runs between retry
// attempts. cenkalti v5 does not provide a between-retry hook, and callers such
// as retryclient need it for pre-retry preparation (e.g. Seek(0) to rewind a
// reader in Store, or closing the previous ReadCloser in Retrieve).
package retry

import (
	"context"
	"errors"
	"fmt"
	"time"

	cbackoff "github.com/cenkalti/backoff/v5"
)

// permanentError marks an error as non-retryable.
type permanentError struct {
	err error
}

func (e *permanentError) Error() string {
	return e.err.Error()
}

func (e *permanentError) Unwrap() error {
	return e.err
}

// Permanent wraps an error to mark it as non-retryable.
// When Do encounters a permanent error, it stops retrying immediately
// and returns the unwrapped error.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// Config holds retry parameters for exponential backoff.
type Config struct {
	MaxRetries     int           `yaml:"max_retries"`
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
}

// Validate checks that the retry configuration is consistent.
func (c *Config) Validate() error {
	if c.MaxRetries < 0 {
		return fmt.Errorf("max_retries must be >= 0")
	} else if c.MaxRetries > 0 {
		if c.InitialBackoff <= 0 {
			return fmt.Errorf("initial_backoff must be > 0 when max_retries > 0")
		}
		if c.MaxBackoff <= 0 {
			return fmt.Errorf("max_backoff must be > 0 when max_retries > 0")
		}
		if c.MaxBackoff < c.InitialBackoff {
			return fmt.Errorf("max_backoff must be >= initial_backoff")
		}
	}
	return nil
}

// Do executes fn with retries using cenkalti/backoff for exponential backoff delay calculation.
// If cfg is nil or MaxRetries is 0, fn is called exactly once with no retries.
// The fn receives the attempt number starting from 1:
//   - attempt=1: initial call
//   - attempt=2: first retry
//   - attempt=3: second retry, etc.
//
// Returns (attempts, err) where attempts is the total number of times fn was called.
// Callers can use attempt > 1 to detect retries.
func Do(ctx context.Context, cfg *Config, fn func(attempt int) error) (int, error) {
	// Initial call (attempt 1)
	err := fn(1)
	if err == nil || cfg == nil || cfg.MaxRetries == 0 {
		return 1, err
	}

	// Check before entering the retry loop — if the initial call already
	// returned a permanent error, retrying would be pointless.
	var permErr *permanentError
	if errors.As(err, &permErr) {
		return 1, permErr.err
	}

	// Retry loop (attempt 2, 3, 4, ...)
	expBackoff := &cbackoff.ExponentialBackOff{
		InitialInterval:     cfg.InitialBackoff,
		MaxInterval:         cfg.MaxBackoff,
		Multiplier:          2,
		RandomizationFactor: 0.5,
	}
	expBackoff.Reset()

	for i := range cfg.MaxRetries {
		attempt := i + 2
		select {
		case <-ctx.Done():
			return attempt - 1, ctx.Err()
		case <-time.After(expBackoff.NextBackOff()):
		}

		err = fn(attempt)
		if err == nil {
			return attempt, nil
		}

		var pe *permanentError
		if errors.As(err, &pe) {
			return attempt, pe.err
		}
	}
	return cfg.MaxRetries + 1, err
}

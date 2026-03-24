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
package retry

import (
	"context"
	"fmt"
	"time"

	cbackoff "github.com/cenkalti/backoff/v5"
)

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

// Do executes fn with retries using cenkalti/backoff exponential backoff.
// If MaxRetries is 0, fn is called exactly once with no retries.
// The onRetry callback, if non-nil, is called after fn fails and before
// the next retry attempt. The attempt parameter is the zero-based retry
// index (0 = first retry, not the initial call).
// If onRetry returns a non-nil error, retries are aborted immediately
// and that error is returned (useful for non-recoverable preparation failures
// such as a failed Seek).
func Do(ctx context.Context, cfg Config, fn func() error, onRetry func(attempt int, err error) error) error {
	if cfg.MaxRetries == 0 {
		return fn()
	}

	expBackoff := &cbackoff.ExponentialBackOff{
		InitialInterval:     cfg.InitialBackoff,
		MaxInterval:         cfg.MaxBackoff,
		Multiplier:          2,
		RandomizationFactor: 0.5,
	}

	var lastErr error // Track the last error from fn so onRetry receives it before the next attempt.
	retryCount := 0
	firstCall := true

	_, err := cbackoff.Retry(ctx, func() (struct{}, error) {
		if !firstCall && onRetry != nil {
			if abortErr := onRetry(retryCount, lastErr); abortErr != nil {
				return struct{}{}, cbackoff.Permanent(abortErr)
			}
			retryCount++
		}
		firstCall = false

		lastErr = fn()
		if lastErr != nil {
			return struct{}{}, lastErr
		}
		return struct{}{}, nil
	},
		cbackoff.WithBackOff(expBackoff),
		cbackoff.WithMaxTries(uint(cfg.MaxRetries)+1),
		cbackoff.WithMaxElapsedTime(0),
	)
	return err
}

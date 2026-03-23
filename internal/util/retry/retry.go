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

// Package retry provides a shared retry configuration and exponential backoff helper.
package retry

import (
	"context"
	"fmt"
	"time"
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
	}
	if c.MaxRetries > 0 {
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

// Do executes fn and retries on error with exponential backoff.
// If MaxRetries is 0, fn is called exactly once.
// The onRetry callback, if non-nil, is called before each retry sleep
// with the zero-based retry index and the error from the previous attempt.
// If onRetry returns a non-nil error, the retry loop is aborted immediately
// and that error is returned (useful for non-recoverable preparation failures
// such as a failed Seek).
func Do(ctx context.Context, cfg Config, fn func() error, onRetry func(attempt int, err error) error) error {
	err := fn()
	for attempt := 0; err != nil && attempt < cfg.MaxRetries; attempt++ {
		if onRetry != nil {
			if abortErr := onRetry(attempt, err); abortErr != nil {
				return abortErr
			}
		}

		backoff := cfg.InitialBackoff * (1 << attempt)
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("retry cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}

		err = fn()
	}
	return err
}

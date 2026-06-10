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
	"math"
	"sync"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
)

// AIMDConfig holds parameters for the AIMD controller.
type AIMDConfig struct {
	// MinLimit is the floor for the concurrency limit.
	MinLimit int
	// MaxLimit is the ceiling for the concurrency limit.
	MaxLimit int
	// BackoffFactor is the multiplicative decrease factor applied on 429.
	// Must be in (0, 1). Typical value: 0.5.
	BackoffFactor float64
	// AdditiveIncrease is how many slots to add per successful window.
	AdditiveIncrease int
}

// AIMDController implements Additive Increase / Multiplicative Decrease
// concurrency control. It tracks successes and rate-limit signals, adjusting
// the concurrency limit via a caller-provided callback.
//
// A "window" equals the current limit: after `limit` consecutive successes
// the limit increases by AdditiveIncrease. Any rate-limit signal resets the
// window and cuts the limit by BackoffFactor.
//
// setFn is called outside the controller mutex to avoid lock-ordering issues
// with the semaphore's own mutex. This means concurrent RecordSuccess and
// RecordRateLimit calls may interleave their setFn invocations, causing the
// semaphore's limit to temporarily diverge from c.limit. The divergence is
// corrected by the next setFn call. This eventual consistency is acceptable
// for AIMD's approximate control semantics.
type AIMDController struct {
	mu           sync.Mutex
	cfg          AIMDConfig
	limit        int
	successCount int
	setFn        func(int)
	logger       logr.Logger
}

// NewAIMDController creates a controller that calls setFn whenever the
// concurrency limit changes. initialLimit is clamped to [MinLimit, MaxLimit].
func NewAIMDController(cfg AIMDConfig, initialLimit int, setFn func(int), logger logr.Logger) *AIMDController {
	limit := clamp(initialLimit, cfg.MinLimit, cfg.MaxLimit)
	c := &AIMDController{
		cfg:    cfg,
		limit:  limit,
		setFn:  setFn,
		logger: logger,
	}
	// Ensure the semaphore matches the clamped limit from the start.
	if limit != initialLimit {
		logger.V(logging.INFO).Info("AIMD initial clamp", "requested", initialLimit, "clamped", limit)
		setFn(limit)
	}
	return c
}

// RecordSuccess records a successful request. After `limit` consecutive
// successes (one full window), the limit increases by AdditiveIncrease.
func (c *AIMDController) RecordSuccess() {
	newLimit := c.computeSuccessLimit()
	if newLimit > 0 {
		c.setFn(newLimit)
	}
}

func (c *AIMDController) computeSuccessLimit() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.successCount++
	if c.successCount >= c.limit {
		oldLimit := c.limit
		c.limit = min(c.limit+c.cfg.AdditiveIncrease, c.cfg.MaxLimit)
		c.successCount = 0
		if c.limit != oldLimit {
			c.logger.V(logging.INFO).Info("AIMD increase", "old", oldLimit, "new", c.limit)
			return c.limit
		}
	}
	return 0
}

// RecordRateLimit records a rate-limit or overload signal. The limit is cut by
// BackoffFactor and the success counter is reset. The reason parameter
// (e.g. "429", "5xx", "capacity_retry") is included in the log for diagnostics.
func (c *AIMDController) RecordRateLimit(reason string) {
	newLimit := c.computeBackoffLimit(reason)
	if newLimit > 0 {
		c.setFn(newLimit)
	}
}

func (c *AIMDController) computeBackoffLimit(reason string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldLimit := c.limit
	c.limit = max(c.cfg.MinLimit, int(math.Floor(float64(c.limit)*c.cfg.BackoffFactor)))
	c.successCount = 0
	if c.limit != oldLimit {
		c.logger.V(logging.INFO).Info("AIMD decrease", "old", oldLimit, "new", c.limit, "reason", reason)
		return c.limit
	}
	return 0
}

// Limit returns the current concurrency limit.
func (c *AIMDController) Limit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

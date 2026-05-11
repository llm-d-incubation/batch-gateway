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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
)

func defaultCfg() AIMDConfig {
	return AIMDConfig{
		MinLimit:         5,
		MaxLimit:         100,
		BackoffFactor:    0.5,
		AdditiveIncrease: 1,
	}
}

func newTestController(cfg AIMDConfig, initial int) (*AIMDController, *atomic.Int32) {
	var latest atomic.Int32
	latest.Store(int32(initial))
	c := NewAIMDController(cfg, initial, func(n int) {
		latest.Store(int32(n))
	}, logr.Discard())
	return c, &latest
}

func TestAIMDInitialClamp(t *testing.T) {
	tests := []struct {
		name      string
		initial   int
		want      int
		wantSetFn bool // true if setFn should have been called
	}{
		{name: "within range", initial: 50, want: 50, wantSetFn: false},
		{name: "below min", initial: 1, want: 5, wantSetFn: true},
		{name: "above max", initial: 200, want: 100, wantSetFn: true},
		{name: "at min", initial: 5, want: 5, wantSetFn: false},
		{name: "at max", initial: 100, want: 100, wantSetFn: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, latest := newTestController(defaultCfg(), tt.initial)
			if got := c.Limit(); got != tt.want {
				t.Fatalf("Limit() = %d, want %d", got, tt.want)
			}
			if tt.wantSetFn {
				if got := latest.Load(); got != int32(tt.want) {
					t.Fatalf("setFn called with %d, want %d", got, tt.want)
				}
			}
		})
	}
}

func TestAIMDAdditiveIncrease(t *testing.T) {
	t.Run("increases after one full window", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.MaxLimit = 20
		c, latest := newTestController(cfg, 10)

		// 10 successes = one window at limit 10
		for i := 0; i < 10; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 11 {
			t.Fatalf("after window: Limit() = %d, want 11", got)
		}
		if got := latest.Load(); got != 11 {
			t.Fatalf("setFn called with %d, want 11", got)
		}
	})

	t.Run("does not increase before full window", func(t *testing.T) {
		c, _ := newTestController(defaultCfg(), 10)
		for i := 0; i < 9; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 10 {
			t.Fatalf("before window complete: Limit() = %d, want 10", got)
		}
	})

	t.Run("clamps at MaxLimit", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.MaxLimit = 11
		c, _ := newTestController(cfg, 10)

		// window of 10 → limit becomes 11
		for i := 0; i < 10; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 11 {
			t.Fatalf("Limit() = %d, want 11", got)
		}

		// another window of 11 → should stay at 11 (MaxLimit)
		for i := 0; i < 11; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 11 {
			t.Fatalf("Limit() = %d, want 11 (clamped)", got)
		}
	})

	t.Run("additive increase > 1", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.AdditiveIncrease = 5
		cfg.MaxLimit = 100
		c, _ := newTestController(cfg, 10)

		for i := 0; i < 10; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 15 {
			t.Fatalf("Limit() = %d, want 15", got)
		}
	})
}

func TestAIMDMultiplicativeDecrease(t *testing.T) {
	t.Run("halves on rate limit", func(t *testing.T) {
		c, latest := newTestController(defaultCfg(), 100)

		c.RecordRateLimit("429")
		if got := c.Limit(); got != 50 {
			t.Fatalf("after 1 rate limit: Limit() = %d, want 50", got)
		}
		if got := latest.Load(); got != 50 {
			t.Fatalf("setFn called with %d, want 50", got)
		}
	})

	t.Run("multiple decreases", func(t *testing.T) {
		c, _ := newTestController(defaultCfg(), 100)

		c.RecordRateLimit("429") // 100 → 50
		c.RecordRateLimit("5xx") // 50 → 25
		c.RecordRateLimit("429") // 25 → 12
		if got := c.Limit(); got != 12 {
			t.Fatalf("Limit() = %d, want 12", got)
		}
	})

	t.Run("clamps at MinLimit", func(t *testing.T) {
		c, _ := newTestController(defaultCfg(), 10)

		c.RecordRateLimit("429") // 10 → 5
		c.RecordRateLimit("429") // 5 → stays 5 (MinLimit)
		if got := c.Limit(); got != 5 {
			t.Fatalf("Limit() = %d, want 5 (clamped)", got)
		}
	})

	t.Run("resets success counter", func(t *testing.T) {
		c, _ := newTestController(defaultCfg(), 10)

		// 9 successes (almost a full window)
		for i := 0; i < 9; i++ {
			c.RecordSuccess()
		}

		// rate limit resets counter
		c.RecordRateLimit("429") // 10 → 5

		// need 5 more successes for new window (not 1)
		for i := 0; i < 4; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 5 {
			t.Fatalf("Limit() = %d, want 5 (counter was reset)", got)
		}
		c.RecordSuccess() // 5th success completes window
		if got := c.Limit(); got != 6 {
			t.Fatalf("Limit() = %d, want 6", got)
		}
	})
}

func TestAIMDBackoffFactor(t *testing.T) {
	tests := []struct {
		name   string
		factor float64
		start  int
		want   int
	}{
		{name: "0.5 factor", factor: 0.5, start: 100, want: 50},
		{name: "0.7 factor", factor: 0.7, start: 100, want: 70},
		{name: "0.9 factor", factor: 0.9, start: 100, want: 90},
		{name: "0.3 factor floors", factor: 0.3, start: 10, want: 5}, // floor(3.0) = 3, but MinLimit=5
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultCfg()
			cfg.BackoffFactor = tt.factor
			c, _ := newTestController(cfg, tt.start)
			c.RecordRateLimit("429")
			if got := c.Limit(); got != tt.want {
				t.Fatalf("Limit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAIMDSawtoothPattern(t *testing.T) {
	t.Run("increase then decrease then increase", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.MinLimit = 5
		cfg.MaxLimit = 20
		c, _ := newTestController(cfg, 10)

		// Window of 10 → 11
		for i := 0; i < 10; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 11 {
			t.Fatalf("after increase: Limit() = %d, want 11", got)
		}

		// Rate limit → 5
		c.RecordRateLimit("429")
		if got := c.Limit(); got != 5 {
			t.Fatalf("after decrease: Limit() = %d, want 5", got)
		}

		// Window of 5 → 6
		for i := 0; i < 5; i++ {
			c.RecordSuccess()
		}
		if got := c.Limit(); got != 6 {
			t.Fatalf("after re-increase: Limit() = %d, want 6", got)
		}
	})
}

func TestAIMDConcurrency(t *testing.T) {
	t.Run("concurrent success and rate limit signals", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.MinLimit = 1
		cfg.MaxLimit = 1000
		c, _ := newTestController(cfg, 100)

		var wg sync.WaitGroup
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				if n%20 == 0 {
					c.RecordRateLimit("429")
				} else {
					c.RecordSuccess()
				}
			}(i)
		}
		wg.Wait()

		limit := c.Limit()
		if limit < cfg.MinLimit || limit > cfg.MaxLimit {
			t.Fatalf("Limit() = %d, out of [%d, %d]", limit, cfg.MinLimit, cfg.MaxLimit)
		}
	})
}

func TestAIMDNoCallbackWhenUnchanged(t *testing.T) {
	t.Run("no setFn call when already at MinLimit", func(t *testing.T) {
		var calls atomic.Int32
		cfg := defaultCfg()
		c := NewAIMDController(cfg, 5, func(n int) {
			calls.Add(1)
		}, logr.Discard())

		// Already at MinLimit=5, floor(5*0.5)=2 → clamped to 5 → no change
		c.RecordRateLimit("429")
		if got := calls.Load(); got != 0 {
			t.Fatalf("setFn called %d times, want 0", got)
		}
	})

	t.Run("no setFn call when already at MaxLimit", func(t *testing.T) {
		var calls atomic.Int32
		cfg := defaultCfg()
		c := NewAIMDController(cfg, 100, func(n int) {
			calls.Add(1)
		}, logr.Discard())

		// Window of 100 successes → limit stays 100 (MaxLimit)
		for i := 0; i < 100; i++ {
			c.RecordSuccess()
		}
		if got := calls.Load(); got != 0 {
			t.Fatalf("setFn called %d times, want 0", got)
		}
	})
}

func BenchmarkAIMDRecordSuccess(b *testing.B) {
	c, _ := newTestController(defaultCfg(), 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.RecordSuccess()
	}
}

func BenchmarkAIMDRecordRateLimit(b *testing.B) {
	c, _ := newTestController(defaultCfg(), 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.RecordRateLimit("429")
	}
}

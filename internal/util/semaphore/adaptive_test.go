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
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestNewAdaptive(t *testing.T) {
	tests := []struct {
		name     string
		capacity int
		wantErr  error
	}{
		{name: "valid capacity", capacity: 5, wantErr: nil},
		{name: "zero capacity", capacity: 0, wantErr: ErrCap},
		{name: "negative capacity", capacity: -1, wantErr: ErrCap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sem, err := NewAdaptive(tt.capacity, nil)
			if err != tt.wantErr {
				t.Fatalf("NewAdaptive(%d): got err=%v, want %v", tt.capacity, err, tt.wantErr)
			}
			if tt.wantErr == nil && sem == nil {
				t.Fatal("expected non-nil semaphore")
			}
			if tt.wantErr != nil && sem != nil {
				t.Fatal("expected nil semaphore on error")
			}
		})
	}
}

func TestAdaptiveAcquireRelease(t *testing.T) {
	t.Run("acquire and release single token", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		sem.Release()
	})

	t.Run("acquire blocks when exhausted", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("first Acquire: %v", err)
		}

		acquired := make(chan bool, 1)
		go func() {
			err := sem.Acquire(context.Background())
			acquired <- (err == nil)
		}()

		select {
		case <-acquired:
			t.Fatal("Acquire should have blocked")
		case <-time.After(50 * time.Millisecond):
		}

		sem.Release()

		select {
		case ok := <-acquired:
			if !ok {
				t.Fatal("Acquire should have succeeded after Release")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Acquire should have unblocked after Release")
		}
	})

	t.Run("multiple acquires and releases", func(t *testing.T) {
		sem, err := NewAdaptive(3, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		ctx := context.Background()
		for i := 0; i < 3; i++ {
			if err := sem.Acquire(ctx); err != nil {
				t.Fatalf("Acquire %d: %v", i, err)
			}
		}
		for i := 0; i < 3; i++ {
			sem.Release()
		}
		if err := sem.Acquire(ctx); err != nil {
			t.Fatalf("Acquire after release: %v", err)
		}
	})
}

func TestAdaptiveContextCancellation(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("first Acquire: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := sem.Acquire(ctx); err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("timed out context", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("first Acquire: %v", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		if err := sem.Acquire(ctx); err != context.DeadlineExceeded {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
	})
}

func TestAdaptiveTryAcquire(t *testing.T) {
	t.Run("succeeds when available", func(t *testing.T) {
		sem, err := NewAdaptive(2, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if !sem.TryAcquire() {
			t.Fatal("TryAcquire should succeed")
		}
		if !sem.TryAcquire() {
			t.Fatal("TryAcquire should succeed for second token")
		}
	})

	t.Run("fails when exhausted", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if !sem.TryAcquire() {
			t.Fatal("first TryAcquire should succeed")
		}
		if sem.TryAcquire() {
			t.Fatal("TryAcquire should fail when exhausted")
		}
	})

	t.Run("succeeds after release", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if !sem.TryAcquire() {
			t.Fatal("first TryAcquire should succeed")
		}
		sem.Release()
		if !sem.TryAcquire() {
			t.Fatal("TryAcquire should succeed after Release")
		}
	})
}

func TestAdaptiveSetLimit(t *testing.T) {
	t.Run("increase wakes blocked goroutines", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		// Fill the single slot
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		acquired := make(chan bool, 1)
		go func() {
			err := sem.Acquire(context.Background())
			acquired <- (err == nil)
		}()

		// Goroutine should be blocked
		select {
		case <-acquired:
			t.Fatal("Acquire should have blocked")
		case <-time.After(50 * time.Millisecond):
		}

		// Increase limit to 2 — blocked goroutine should wake up
		sem.SetLimit(2)

		select {
		case ok := <-acquired:
			if !ok {
				t.Fatal("Acquire should have succeeded after SetLimit increase")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Acquire should have unblocked after SetLimit increase")
		}
	})

	t.Run("decrease below count does not interrupt holders", func(t *testing.T) {
		sem, err := NewAdaptive(5, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		ctx := context.Background()

		// Acquire 3 tokens
		for i := 0; i < 3; i++ {
			if err := sem.Acquire(ctx); err != nil {
				t.Fatalf("Acquire %d: %v", i, err)
			}
		}

		// Decrease limit to 2 (below current count of 3)
		sem.SetLimit(2)

		// New acquire should block
		acquired := make(chan bool, 1)
		go func() {
			err := sem.Acquire(ctx)
			acquired <- (err == nil)
		}()

		select {
		case <-acquired:
			t.Fatal("Acquire should block when count > new limit")
		case <-time.After(50 * time.Millisecond):
		}

		// Release 2 tokens (count goes 3→2→1), now below new limit of 2
		sem.Release()
		sem.Release()

		select {
		case ok := <-acquired:
			if !ok {
				t.Fatal("Acquire should succeed after releases brought count below limit")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Acquire should have unblocked")
		}
	})

	t.Run("clamp to 1", func(t *testing.T) {
		sem, err := NewAdaptive(5, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		sem.SetLimit(0)
		if got := sem.Limit(); got != 1 {
			t.Fatalf("SetLimit(0): got limit %d, want 1", got)
		}
		sem.SetLimit(-10)
		if got := sem.Limit(); got != 1 {
			t.Fatalf("SetLimit(-10): got limit %d, want 1", got)
		}
	})

	t.Run("Limit returns current value", func(t *testing.T) {
		sem, err := NewAdaptive(10, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if got := sem.Limit(); got != 10 {
			t.Fatalf("initial Limit(): got %d, want 10", got)
		}
		sem.SetLimit(42)
		if got := sem.Limit(); got != 42 {
			t.Fatalf("Limit() after SetLimit(42): got %d, want 42", got)
		}
	})
}

func TestAdaptiveConcurrency(t *testing.T) {
	t.Run("enforces capacity limit under concurrent load", func(t *testing.T) {
		capacity := 10
		sem, err := NewAdaptive(capacity, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		ctx := context.Background()

		var current atomic.Int32
		var max atomic.Int32
		var wg sync.WaitGroup

		numGoroutines := 100
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := sem.Acquire(ctx); err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				cur := current.Add(1)
				for {
					m := max.Load()
					if cur <= m || max.CompareAndSwap(m, cur) {
						break
					}
				}
				time.Sleep(1 * time.Millisecond)
				current.Add(-1)
				sem.Release()
			}()
		}

		wg.Wait()

		if maxC := max.Load(); maxC > int32(capacity) {
			t.Fatalf("allowed %d concurrent, capacity is %d", maxC, capacity)
		}
	})
}

func TestAdaptiveDoubleRelease(t *testing.T) {
	t.Run("callback fires on double release", func(t *testing.T) {
		var called atomic.Int32
		sem, err := NewAdaptive(1, func() { called.Add(1) })
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		sem.Release()
		sem.Release() // double release

		if called.Load() != 1 {
			t.Fatalf("callback: got %d calls, want 1", called.Load())
		}
	})

	t.Run("callback fires at most once", func(t *testing.T) {
		var called atomic.Int32
		sem, err := NewAdaptive(1, func() { called.Add(1) })
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		sem.Release()
		sem.Release()

		if called.Load() != 1 {
			t.Fatalf("callback: got %d calls, want 1", called.Load())
		}
	})

	t.Run("no panic when nil callback", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		sem.Release()
	})
}

func TestAdaptiveSemaphoreWithAIMD(t *testing.T) {
	t.Run("sawtooth: increase then decrease then recover", func(t *testing.T) {
		sem, err := NewAdaptive(10, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}

		ctrl := NewAIMDController(AIMDConfig{
			MinLimit:         5,
			MaxLimit:         20,
			BackoffFactor:    0.5,
			AdditiveIncrease: 1,
		}, 10, func(n int) { sem.SetLimit(n) }, logr.Discard())

		// Window of 10 successes → limit 11
		for i := 0; i < 10; i++ {
			ctrl.RecordSuccess()
		}
		if got := sem.Limit(); got != 11 {
			t.Fatalf("after increase: sem.Limit() = %d, want 11", got)
		}

		// Rate limit → floor(11*0.5) = 5
		ctrl.RecordRateLimit("429")
		if got := sem.Limit(); got != 5 {
			t.Fatalf("after decrease: sem.Limit() = %d, want 5", got)
		}

		// Window of 5 successes → limit 6
		for i := 0; i < 5; i++ {
			ctrl.RecordSuccess()
		}
		if got := sem.Limit(); got != 6 {
			t.Fatalf("after recovery: sem.Limit() = %d, want 6", got)
		}
	})

	t.Run("limit increase unblocks waiting Acquire", func(t *testing.T) {
		sem, err := NewAdaptive(1, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}

		ctrl := NewAIMDController(AIMDConfig{
			MinLimit:         1,
			MaxLimit:         10,
			BackoffFactor:    0.5,
			AdditiveIncrease: 1,
		}, 1, func(n int) { sem.SetLimit(n) }, logr.Discard())

		// Fill the single slot
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("Acquire: %v", err)
		}

		acquired := make(chan bool, 1)
		go func() {
			err := sem.Acquire(context.Background())
			acquired <- (err == nil)
		}()

		// Should be blocked
		select {
		case <-acquired:
			t.Fatal("Acquire should have blocked")
		case <-time.After(50 * time.Millisecond):
		}

		// AIMD increase: 1 success completes the window → limit 2
		ctrl.RecordSuccess()

		select {
		case ok := <-acquired:
			if !ok {
				t.Fatal("Acquire should have succeeded after AIMD increase")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Acquire should have unblocked after AIMD increase")
		}
	})

	t.Run("limit decrease does not interrupt holders", func(t *testing.T) {
		sem, err := NewAdaptive(10, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}

		ctrl := NewAIMDController(AIMDConfig{
			MinLimit:         5,
			MaxLimit:         10,
			BackoffFactor:    0.5,
			AdditiveIncrease: 1,
		}, 10, func(n int) { sem.SetLimit(n) }, logr.Discard())

		ctx := context.Background()
		// Acquire 8 tokens
		for i := 0; i < 8; i++ {
			if err := sem.Acquire(ctx); err != nil {
				t.Fatalf("Acquire %d: %v", i, err)
			}
		}

		// Rate limit → limit drops to 5 (but 8 holders continue)
		ctrl.RecordRateLimit("429")
		if got := sem.Limit(); got != 5 {
			t.Fatalf("sem.Limit() = %d, want 5", got)
		}

		// New acquire should block
		acquired := make(chan bool, 1)
		go func() {
			err := sem.Acquire(ctx)
			acquired <- (err == nil)
		}()

		select {
		case <-acquired:
			t.Fatal("Acquire should block when count > new limit")
		case <-time.After(50 * time.Millisecond):
		}

		// Release 4 tokens (8→7→6→5→4), now count < limit
		for i := 0; i < 4; i++ {
			sem.Release()
		}

		select {
		case ok := <-acquired:
			if !ok {
				t.Fatal("Acquire should succeed after releases")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("Acquire should have unblocked")
		}
	})
}

func TestAdaptiveSetLimitConcurrentAcquire(t *testing.T) {
	t.Run("no race or deadlock under concurrent setlimit and acquire", func(t *testing.T) {
		sem, err := NewAdaptive(10, nil)
		if err != nil {
			t.Fatalf("NewAdaptive: %v", err)
		}
		ctx := context.Background()
		var wg sync.WaitGroup

		// Goroutines that acquire, hold briefly, and release
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					if err := sem.Acquire(ctx); err != nil {
						return
					}
					time.Sleep(100 * time.Microsecond)
					sem.Release()
				}
			}()
		}

		// Concurrently change limit up and down
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if j%2 == 0 {
					sem.SetLimit(3)
				} else {
					sem.SetLimit(15)
				}
				time.Sleep(50 * time.Microsecond)
			}
			sem.SetLimit(10)
		}()

		wg.Wait()
		// If we reach here without deadlock or -race failure, the test passes.
	})
}

func BenchmarkAdaptiveAcquireRelease(b *testing.B) {
	sem, err := NewAdaptive(10, nil)
	if err != nil {
		b.Fatalf("NewAdaptive: %v", err)
	}
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := sem.Acquire(ctx); err != nil {
			b.Fatal(err)
		}
		sem.Release()
	}
}

func BenchmarkAdaptiveConcurrentAcquireRelease(b *testing.B) {
	sem, err := NewAdaptive(10, nil)
	if err != nil {
		b.Fatalf("NewAdaptive: %v", err)
	}
	ctx := context.Background()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := sem.Acquire(ctx); err != nil {
				b.Fatal(err)
			}
			sem.Release()
		}
	})
}

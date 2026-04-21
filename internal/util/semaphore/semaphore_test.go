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
)

func TestNew(t *testing.T) {
	t.Run("creates semaphore with valid capacity", func(t *testing.T) {
		sem, err := New(5, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sem == nil {
			t.Fatal("expected non-nil semaphore")
		}
	})

	t.Run("returns error with zero capacity", func(t *testing.T) {
		sem, err := New(0, nil)
		if err != ErrCap {
			t.Fatalf("expected ErrCap, got %v", err)
		}
		if sem != nil {
			t.Fatal("expected nil semaphore")
		}
	})

	t.Run("returns error with negative capacity", func(t *testing.T) {
		sem, err := New(-1, nil)
		if err != ErrCap {
			t.Fatalf("expected ErrCap, got %v", err)
		}
		if sem != nil {
			t.Fatal("expected nil semaphore")
		}
	})
}

func TestAcquireRelease(t *testing.T) {
	t.Run("acquire and release single token", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}
		ctx := context.Background()

		if err := sem.Acquire(ctx); err != nil {
			t.Fatalf("unexpected error on acquire: %v", err)
		}

		sem.Release()
	})

	t.Run("acquire blocks when capacity is exhausted", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}
		ctx := context.Background()

		// Acquire the only token
		if err := sem.Acquire(ctx); err != nil {
			t.Fatalf("unexpected error on first acquire: %v", err)
		}

		// Try to acquire again - should block
		acquired := make(chan bool)
		go func() {
			err := sem.Acquire(ctx)
			acquired <- (err == nil)
		}()

		// Wait a bit to ensure the goroutine is blocked
		select {
		case <-acquired:
			t.Fatal("acquire should have blocked")
		case <-time.After(50 * time.Millisecond):
			// Expected - goroutine is blocked
		}

		// Release and verify the blocked goroutine can now acquire
		sem.Release()

		select {
		case success := <-acquired:
			if !success {
				t.Fatal("acquire should have succeeded after release")
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("acquire should have unblocked after release")
		}
	})

	t.Run("multiple acquires and releases", func(t *testing.T) {
		sem, err := New(3, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}
		ctx := context.Background()

		// Acquire all 3 tokens
		for i := 0; i < 3; i++ {
			if err := sem.Acquire(ctx); err != nil {
				t.Fatalf("unexpected error on acquire %d: %v", i, err)
			}
		}

		// Release all tokens
		for i := 0; i < 3; i++ {
			sem.Release()
		}

		// Should be able to acquire again
		if err := sem.Acquire(ctx); err != nil {
			t.Fatalf("unexpected error on acquire after release: %v", err)
		}
	})
}

func TestAcquireContextCancellation(t *testing.T) {
	t.Run("acquire returns error when context is cancelled", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		// Acquire the only token
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("unexpected error on first acquire: %v", err)
		}

		// Try to acquire with cancelled context
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err = sem.Acquire(ctx)
		if err == nil {
			t.Fatal("expected error on acquire with cancelled context")
		}
		if err != context.Canceled {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	})

	t.Run("acquire returns error when context times out", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		// Acquire the only token
		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("unexpected error on first acquire: %v", err)
		}

		// Try to acquire with timeout context
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		err = sem.Acquire(ctx)
		if err == nil {
			t.Fatal("expected error on acquire with timeout context")
		}
		if err != context.DeadlineExceeded {
			t.Fatalf("expected context.DeadlineExceeded, got %v", err)
		}
	})
}

func TestTryAcquire(t *testing.T) {
	t.Run("succeeds when tokens are available", func(t *testing.T) {
		sem, err := New(2, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		if !sem.TryAcquire() {
			t.Fatal("TryAcquire should succeed when tokens are available")
		}
		if !sem.TryAcquire() {
			t.Fatal("TryAcquire should succeed for second token")
		}
	})

	t.Run("fails when capacity is exhausted", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		if !sem.TryAcquire() {
			t.Fatal("first TryAcquire should succeed")
		}

		if sem.TryAcquire() {
			t.Fatal("TryAcquire should fail when capacity is exhausted")
		}
	})

	t.Run("succeeds after release", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		if !sem.TryAcquire() {
			t.Fatal("first TryAcquire should succeed")
		}
		if sem.TryAcquire() {
			t.Fatal("second TryAcquire should fail")
		}

		sem.Release()

		if !sem.TryAcquire() {
			t.Fatal("TryAcquire should succeed after release")
		}
	})
}

func TestConcurrency(t *testing.T) {
	t.Run("enforces capacity limit under concurrent load", func(t *testing.T) {
		capacity := 10
		sem, err := New(capacity, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}
		ctx := context.Background()

		var current atomic.Int32
		var max atomic.Int32
		var wg sync.WaitGroup

		// Launch 100 goroutines that will compete for tokens
		numGoroutines := 100
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				if err := sem.Acquire(ctx); err != nil {
					t.Errorf("unexpected error on acquire: %v", err)
					return
				}

				cur := current.Add(1)
				// Update max if current is greater
				for {
					m := max.Load()
					if cur <= m || max.CompareAndSwap(m, cur) {
						break
					}
				}

				// Simulate work
				time.Sleep(1 * time.Millisecond)

				current.Add(-1)
				sem.Release()
			}()
		}

		wg.Wait()

		maxConcurrent := max.Load()
		if maxConcurrent > int32(capacity) {
			t.Fatalf("semaphore allowed %d concurrent acquisitions, capacity is %d",
				maxConcurrent, capacity)
		}
		if maxConcurrent < 2 {
			t.Fatal("concurrent execution did not happen (max concurrent < 2)")
		}
	})

	t.Run("all goroutines complete successfully", func(t *testing.T) {
		sem, err := New(5, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}
		ctx := context.Background()

		var completed atomic.Int32
		var wg sync.WaitGroup

		numGoroutines := 50
		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()

				if err := sem.Acquire(ctx); err != nil {
					t.Errorf("unexpected error on acquire: %v", err)
					return
				}

				time.Sleep(1 * time.Millisecond)

				sem.Release()
				completed.Add(1)
			}()
		}

		wg.Wait()

		if completed.Load() != int32(numGoroutines) {
			t.Fatalf("expected %d completions, got %d", numGoroutines, completed.Load())
		}
	})
}

func TestReleaseWithoutAcquire(t *testing.T) {
	t.Run("release without acquire does not panic", func(t *testing.T) {
		sem, err := New(5, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}
		// This should not panic, though it's incorrect usage
		sem.Release()
	})
}

func TestDoubleReleaseCallback(t *testing.T) {
	t.Run("callback fires on double release", func(t *testing.T) {
		var called atomic.Int32
		sem, err := New(1, func() { called.Add(1) })
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		if err := sem.Acquire(context.Background()); err != nil {
			t.Fatalf("acquire: %v", err)
		}
		sem.Release()
		sem.Release() // double release

		if called.Load() != 1 {
			t.Fatalf("expected callback to fire once, got %d", called.Load())
		}
	})

	t.Run("callback fires at most once", func(t *testing.T) {
		var called atomic.Int32
		sem, err := New(1, func() { called.Add(1) })
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		sem.Release() // first double release
		sem.Release() // second double release

		if called.Load() != 1 {
			t.Fatalf("expected callback to fire exactly once, got %d", called.Load())
		}
	})

	t.Run("no callback when nil", func(t *testing.T) {
		sem, err := New(1, nil)
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		// No callback — should not panic on double release
		sem.Release()
	})

	t.Run("normal acquire-release does not trigger callback", func(t *testing.T) {
		var called atomic.Int32
		sem, err := New(2, func() { called.Add(1) })
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		for i := 0; i < 100; i++ {
			if err := sem.Acquire(context.Background()); err != nil {
				t.Fatalf("acquire %d: %v", i, err)
			}
			sem.Release()
		}

		if called.Load() != 0 {
			t.Fatalf("callback should not fire during normal usage, got %d", called.Load())
		}
	})

	t.Run("concurrent double releases fire callback exactly once", func(t *testing.T) {
		var called atomic.Int32
		sem, err := New(1, func() { called.Add(1) })
		if err != nil {
			t.Fatalf("failed to create semaphore: %v", err)
		}

		var wg sync.WaitGroup
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem.Release()
			}()
		}
		wg.Wait()

		if called.Load() != 1 {
			t.Fatalf("expected callback exactly once under concurrent double-release, got %d", called.Load())
		}
	})
}

func BenchmarkAcquireRelease(b *testing.B) {
	sem, err := New(10, nil)
	if err != nil {
		b.Fatalf("failed to create semaphore: %v", err)
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

func BenchmarkConcurrentAcquireRelease(b *testing.B) {
	sem, err := New(10, nil)
	if err != nil {
		b.Fatalf("failed to create semaphore: %v", err)
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

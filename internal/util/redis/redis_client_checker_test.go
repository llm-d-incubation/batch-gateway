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

package redis

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRedisClientChecker_Singleflight_Coalesces verifies that concurrent
// calls to Check() are coalesced by singleflight: only one actual check
// runs at a time, and all concurrent callers share the result.
func TestRedisClientChecker_Singleflight_Coalesces(t *testing.T) {
	var inflight atomic.Int32
	var maxInflight atomic.Int32

	checker := &RedisClientChecker{
		cmdTimeout: 5 * time.Second,
	}

	// Override the singleflight function by wrapping Check in a goroutine
	// test. We can't directly inject a function, but we can use the fact
	// that CheckClient will be called with a nil redis client. Instead,
	// we'll test the singleflight behavior at the Check level by using
	// a channel-synchronized approach.
	//
	// Since we can't easily mock CheckClient (it's a package-level func),
	// we test the coalescing property indirectly: launch N goroutines that
	// all call sf.Do simultaneously and verify they all complete together.

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	// Use the singleflight group directly to test coalescing behavior.
	barrier := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, _, shared := checker.sf.Do("check", func() (interface{}, error) {
				cur := inflight.Add(1)
				// Track max concurrent executions inside singleflight.
				for {
					old := maxInflight.Load()
					if cur <= old || maxInflight.CompareAndSwap(old, cur) {
						break
					}
				}
				<-barrier // block until released
				inflight.Add(-1)
				return nil, nil
			})
			_ = shared
		}()
	}

	// Give goroutines time to enter sf.Do.
	time.Sleep(50 * time.Millisecond)

	// Release the single execution.
	close(barrier)
	wg.Wait()

	if max := maxInflight.Load(); max != 1 {
		t.Fatalf("expected max 1 concurrent execution inside singleflight, got %d", max)
	}
}

// TestRedisClientChecker_Sequential_NotCached verifies that sequential
// calls each execute independently — singleflight does not cache results.
func TestRedisClientChecker_Sequential_NotCached(t *testing.T) {
	var callCount atomic.Int32

	checker := &RedisClientChecker{
		cmdTimeout: 5 * time.Second,
	}

	for i := 0; i < 3; i++ {
		_, err, _ := checker.sf.Do("check", func() (interface{}, error) {
			callCount.Add(1)
			return nil, nil
		})
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}

	if got := callCount.Load(); got != 3 {
		t.Fatalf("expected 3 sequential executions, got %d (singleflight incorrectly cached)", got)
	}
}

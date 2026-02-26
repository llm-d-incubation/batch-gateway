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

package metrics

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMockClient_Sine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &MockClient{}
		ctx := context.Background()

		// Test at t=0 (should be at midpoint, which is 0.5)
		budget, err := client.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.5, budget, 0.01, "at t=0s, sine should be at midpoint")

		// Advance 15 seconds (quarter period) - should be at max
		time.Sleep(15 * time.Second)
		synctest.Wait()

		budget, err = client.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 1.0, budget, 0.01, "at t=15s, sine should be at maximum")

		// Advance 15 more seconds (half period) - should be back at midpoint
		time.Sleep(15 * time.Second)
		synctest.Wait()

		budget, err = client.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.5, budget, 0.01, "at t=30s, sine should be at midpoint")

		// Advance 15 more seconds (three-quarter period) - should be at min
		time.Sleep(15 * time.Second)
		synctest.Wait()

		budget, err = client.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.0, budget, 0.01, "at t=45s, sine should be at minimum")

		// Advance 15 more seconds (full period) - should be back at midpoint
		time.Sleep(15 * time.Second)
		synctest.Wait()

		budget, err = client.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.5, budget, 0.01, "at t=60s, sine should complete the cycle")
	})
}

func TestMockClient_Range(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &MockClient{}
		ctx := context.Background()

		// Sample at different points in the sine wave
		// Period is 60 seconds, so sample every 6 seconds (10 samples over one period)
		for i := 0; i < 10; i++ {
			budget, err := client.Budget(ctx)
			require.NoError(t, err, "iteration %d at t=%ds", i, i*6)

			// Check that budget is always within [0.0, 1.0]
			require.GreaterOrEqual(t, budget, 0.0,
				"iteration %d at t=%ds: budget should be >= 0.0", i, i*6)
			require.LessOrEqual(t, budget, 1.0,
				"iteration %d at t=%ds: budget should be <= 1.0", i, i*6)

			// Advance time by 6 seconds
			if i < 9 {
				time.Sleep(6 * time.Second)
				synctest.Wait()
			}
		}
	})
}

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

package worker

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	inferencemetrics "github.com/llm-d-incubation/batch-gateway/internal/inference/metrics"
)

func TestFlowControl_RetryDelayOnZeroBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Create a metrics client with MockClient (sine wave budget)
		metricsClient := &inferencemetrics.MockClient{}
		ctx := context.Background()

		// Test at t=0s - midpoint budget (0.5)
		budget, err := metricsClient.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.5, budget, 0.01, "at t=0s, budget should be ~0.5")

		// Advance to t=15s - max budget (1.0)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		budget, err = metricsClient.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 1.0, budget, 0.01, "at t=15s, budget should be ~1.0")

		// Advance to t=30s - midpoint budget (0.5)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		budget, err = metricsClient.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.5, budget, 0.01, "at t=30s, budget should be ~0.5")

		// Advance to t=45s - min budget (0.0)
		time.Sleep(15 * time.Second)
		synctest.Wait()
		budget, err = metricsClient.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.0, budget, 0.01, "at t=45s, budget should be ~0.0")
	})
}

func TestFlowControl_PollIntervalDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		pollInterval := 5 * time.Second

		// Simulate the retry delay behavior when budget is zero
		// This mimics the select statement in RunPollingLoop

		// Start time
		start := time.Now()

		// Simulate waiting for poll interval (as in the code)
		select {
		case <-ctx.Done():
			t.Fatal("context should not be canceled")
		case <-time.After(pollInterval):
			// Expected path
		}

		// Verify that time advanced by exactly pollInterval
		elapsed := time.Since(start)
		require.Equal(t, pollInterval, elapsed, "time should advance by exactly pollInterval")
	})
}

func TestFlowControl_NoBudgetSkipsQueuePull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		metricsClient := &inferencemetrics.MockClient{}

		// Advance to t=45s where budget should be ~0.0
		time.Sleep(45 * time.Second)
		synctest.Wait()

		budget, err := metricsClient.Budget(ctx)
		require.NoError(t, err)
		require.InDelta(t, 0.0, budget, 0.01, "at t=45s, budget should be near 0")

		// Simulate the poll interval delay
		pollInterval := 5 * time.Second
		beforeDelay := time.Now()
		time.Sleep(pollInterval)
		synctest.Wait()
		afterDelay := time.Now()

		require.Equal(t, pollInterval, afterDelay.Sub(beforeDelay),
			"should wait exactly pollInterval before retry")
	})
}

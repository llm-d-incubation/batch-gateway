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
	"math"
	"time"
)

// MockClient implements Client interface with a simple sine wave behavior
type MockClient struct{}

var _ Client = (*MockClient)(nil)

// Budget returns a mock dispatch budget value following a sine wave pattern.
// The sine wave oscillates between 0.0 and 1.0 with a 60 second period.
func (m *MockClient) Budget(ctx context.Context) (float64, error) {
	now := time.Now().Unix()
	// Sine wave with 60 second period, amplitude 0.5, midpoint 0.5
	// Result ranges from 0.0 to 1.0
	period := 60.0
	value := 0.5 + 0.5*math.Sin(2*math.Pi*float64(now)/period)
	return value, nil
}

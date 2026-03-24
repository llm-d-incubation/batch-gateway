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

package retryclient

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	retriesTotal        *prometheus.CounterVec
	retryExhaustedTotal *prometheus.CounterVec
	metricsOnce         sync.Once
)

// InitMetrics registers retry-related Prometheus metrics.
// It is safe to call multiple times; registration happens only once.
func InitMetrics() {
	metricsOnce.Do(func() {
		retriesTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "file_storage_retries_total",
				Help: "Total number of file storage retry attempts",
			},
			[]string{"operation", "component"},
		)
		retryExhaustedTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "file_storage_retry_exhausted_total",
				Help: "Total number of file storage operations that failed after exhausting all retries",
			},
			[]string{"operation", "component"},
		)
		prometheus.MustRegister(retriesTotal, retryExhaustedTotal)
	})
}

// recordRetry increments the retry counter for a file storage operation.
func recordRetry(operation, component string) {
	retriesTotal.WithLabelValues(operation, component).Inc()
}

// recordRetryExhausted increments the exhaustion counter when all retries fail.
func recordRetryExhausted(operation, component string) {
	retryExhaustedTotal.WithLabelValues(operation, component).Inc()
}

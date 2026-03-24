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

import "github.com/prometheus/client_golang/prometheus"

var (
	retriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "file_storage_retries_total",
			Help: "Total number of file storage retry attempts",
		},
		[]string{"operation", "tenant_id", "component"},
	)

	retryExhaustedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "file_storage_retry_exhausted_total",
			Help: "Total number of file storage operations that failed after exhausting all retries",
		},
		[]string{"operation", "tenant_id", "component"},
	)
)

func init() {
	prometheus.MustRegister(retriesTotal, retryExhaustedTotal)
}

// RecordRetry increments the retry counter for a file storage operation.
func RecordRetry(operation, tenantID, component string) {
	retriesTotal.WithLabelValues(operation, tenantID, component).Inc()
}

// RecordRetryExhausted increments the exhaustion counter when all retries fail.
func RecordRetryExhausted(operation, tenantID, component string) {
	retryExhaustedTotal.WithLabelValues(operation, tenantID, component).Inc()
}

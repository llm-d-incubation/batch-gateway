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

const (
	statusSuccess   = "success"
	statusRetry     = "retry"
	statusExhausted = "exhausted"
)

var (
	operationsTotal *prometheus.CounterVec
	metricsOnce     sync.Once
)

// InitMetrics registers file storage operation metrics.
// It is safe to call multiple times; registration happens only once.
func InitMetrics() {
	metricsOnce.Do(func() {
		operationsTotal = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "file_storage_operations_total",
				Help: "Total number of file storage operations by outcome",
			},
			[]string{"operation", "component", "status"},
		)
		prometheus.MustRegister(operationsTotal)
	})
}

func recordSuccess(operation, component string) {
	operationsTotal.WithLabelValues(operation, component, statusSuccess).Inc()
}

func recordRetry(operation, component string) {
	operationsTotal.WithLabelValues(operation, component, statusRetry).Inc()
}

func recordExhausted(operation, component string) {
	operationsTotal.WithLabelValues(operation, component, statusExhausted).Inc()
}

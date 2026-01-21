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

import "github.com/prometheus/client_golang/prometheus"

var (
	// number of jobs processed so far
	JobsProcessed = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "jobs_processed_total",
			Help: "Total number of jobs processed",
		},
	)

	// duration of job processing
	JobProcessingDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "job_processing_duration_seconds",
			Help:    "Duration of job processing in seconds",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // from 100ms to ~51s
		},
	)

	// current number of active workers
	ActiveWorkers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "active_workers",
			Help: "Current number of active workers processing jobs",
		},
	)

	// current number of idled workers
	IdleWorkers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "idled_workers",
			Help: "Current number of idled workers waiting for jobs",
		},
	)

	// errors by model
	JobErrorsModelTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "job_errors_by_model_total",
			Help: "Total number of job processing errors by model",
		},
		[]string{"model"},
	)
)

func init() {
	prometheus.MustRegister(JobsProcessed)
	prometheus.MustRegister(JobProcessingDuration)
	prometheus.MustRegister(ActiveWorkers)
	prometheus.MustRegister(IdleWorkers)
	prometheus.MustRegister(JobErrorsModelTotal)
}

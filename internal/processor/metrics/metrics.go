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
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// number of jobs processed so far
	jobsProcessed = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "jobs_processed_total",
			Help: "Total number of jobs processed",
		},
	)

	// duration of job processing
	jobProcessingDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "job_processing_duration_seconds",
			Help:    "Duration of job processing in seconds",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10), // from 100ms to ~51s
		},
	)

	// current number of active workers
	activeWorkers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "active_workers",
			Help: "Current number of active workers processing jobs",
		},
	)

	// errors by model
	jobErrorsModelTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "job_errors_by_model_total",
			Help: "Total number of job processing errors by model",
		},
		[]string{"model"},
	)
)

func init() {
	prometheus.MustRegister(jobsProcessed)
	prometheus.MustRegister(jobProcessingDuration)
	prometheus.MustRegister(activeWorkers)
	prometheus.MustRegister(jobErrorsModelTotal)
}

// Recorder funcs

// RecordJobProcessed increments the total processed jobs count.
func RecordJobProcessed() {
	jobsProcessed.Inc()
}

// RecordJobDuration observes the time taken to process a job.
func RecordJobDuration(duration time.Duration) {
	jobProcessingDuration.Observe(duration.Seconds())
}

// IncActiveWorkers increments the gauge for active workers.
func IncActiveWorkers() {
	activeWorkers.Inc()
}

// DecActiveWorkers decrements the gauge for active workers.
func DecActiveWorkers() {
	activeWorkers.Dec()
}

// RecordJobError increments the error count for a specific model.
func RecordJobError(model string) {
	jobErrorsModelTotal.WithLabelValues(model).Inc()
}

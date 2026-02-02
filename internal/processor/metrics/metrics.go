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

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/prometheus/client_golang/prometheus"
)

// labels definition
const (
	// result labels
	ResultSuccess = "success"
	ResultFailed  = "failed"

	// reason lables
	ReasonUnknown     = "unknown"      // unknown reason
	ReasonUserError   = "user_error"   // method, request validation failed.. etc.,
	ReasonSystemError = "system_error" // SLO failed, system error.. etc.,
	ReasonNone        = "none"         // job is successfully completed

	// size bucket labels
	Bucket100   = "100"   // less than 100 lines
	Bucket1000  = "1000"  // less than 1000 lines
	Bucket10000 = "10000" // less than 10000 lines
	Bucket30000 = "30000" // less than 30000 lines
	BucketLarge = "large" // more than 30000 lines
)

func GetSizeBucket(totalLines int) string {
	switch {
	case totalLines < 100:
		return Bucket100
	case totalLines < 1000:
		return Bucket1000
	case totalLines < 10000:
		return Bucket10000
	case totalLines < 30000:
		return Bucket30000
	default:
		return BucketLarge
	}
}

var (
	jobsProcessed         *prometheus.CounterVec
	jobProcessingDuration *prometheus.HistogramVec
	jobQueueWaitDuration  *prometheus.HistogramVec
	totalWorkers          prometheus.Gauge
	activeWorkers         prometheus.Gauge
	jobErrorsModelTotal   *prometheus.CounterVec
)

func InitMetrics(cfg config.ProcessorConfig) error {
	// number of jobs processed : TODO:: add tenantID?
	jobsProcessed = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "jobs_processed_total",
			Help: "Total number of jobs processed",
		}, []string{"result", "reason"},
	)

	// total number of workers for utilization %
	// this is set once on initialization
	totalWorkers = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "total_workers",
			Help: "Total number of configured workers",
		},
	)
	totalWorkers.Set(float64(cfg.NumWorkers))

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

	// job processing duratino
	jobProcessingDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "job_processing_duration_seconds",
			Help: "Duration of job processing in seconds",
			Buckets: prometheus.ExponentialBuckets(
				cfg.ProcessTimeBucket.BucketStart,
				cfg.ProcessTimeBucket.BucketFactor,
				cfg.ProcessTimeBucket.BucketCount,
			),
		}, []string{"tenantID", "size_bucket"},
	)

	// duration of queue wait time
	jobQueueWaitDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "job_queue_wait_duration",
			Help: "Time spent in the priority queue before being picked up",
			Buckets: prometheus.ExponentialBuckets(
				cfg.QueueTimeBucket.BucketStart,
				cfg.QueueTimeBucket.BucketFactor,
				cfg.QueueTimeBucket.BucketCount,
			),
		}, []string{"tenantID"},
	)

	// metrics to register
	metricsToRegister := []prometheus.Collector{
		jobProcessingDuration,
		jobQueueWaitDuration,
		totalWorkers,
		activeWorkers,
		jobsProcessed,
		jobErrorsModelTotal,
	}

	for _, metric := range metricsToRegister {
		if err := prometheus.Register(metric); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
				continue
			}
			return err
		}
	}

	return nil
}

// Recorder funcs

// RecordQueueWait observes the queue time
func RecordQueueWaitDuration(duration time.Duration, tenantID string) {
	jobQueueWaitDuration.WithLabelValues(tenantID).Observe(duration.Seconds())
}

// RecordJobProcessed increments the total processed jobs count.
func RecordJobProcessed(result string, reason string) {
	jobsProcessed.WithLabelValues(result, reason).Inc()
}

// RecordJobProcessingDuration observes the time taken to process a job.
func RecordJobProcessingDuration(duration time.Duration, tenantID string, sizeBucket string) {
	jobProcessingDuration.WithLabelValues(tenantID, sizeBucket).Observe(duration.Seconds())
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

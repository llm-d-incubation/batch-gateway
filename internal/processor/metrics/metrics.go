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
	// -- Result --
	// ResultSuccess: Job reached a terminal state treated as success by policy (completed; cancelled is treated as success because it is user-initiated).
	// ResultFailed: Job is failed and updated to failed status in the db
	// ResultSkipped: Job was not processed by this worker (e.g. already terminal, not runnable, data inconsistency)
	// ResultReEnqueued: Job was re-enqueued for retry due to transient backend/system issues
	// ResultExpired: Job exceeded SLO deadline (either at dequeue time or mid-execution)

	// Result labels
	ResultSuccess    = "success"
	ResultFailed     = "failed"
	ResultSkipped    = "skipped"
	ResultReEnqueued = "re_enqueued"
	ResultExpired    = "expired" // job exceeded SLO deadline (dequeue-time or mid-execution)

	// -- Reason --
	// - If expired at dequeue time, use ReasonExpiredDequeue
	// - If expired mid-execution, use ReasonExpiredExecution
	// - If data inconsistency, use ReasonDBInconsistency
	// - If retryable backend error, use ReasonDBTransient
	// - If not runnable, use ReasonNotRunnableState
	// - Otherwise, fall back to ReasonSystemError

	// Reason labels
	ReasonSystemError      = "system_error"       // unexpected internal errors (panic, serialization failure, invariant violation)
	ReasonDBTransient      = "db_transient"       // temporary backend/storage error; safe to retry
	ReasonDBInconsistency  = "db_inconsistency"   // PQ item exists but DB item missing or corrupted
	ReasonNotRunnableState = "not_runnable_state" // job status is not runnable by processor policy
	ReasonExpiredDequeue   = "expired_dequeue"    // SLO already exceeded before execution started; skipped at dequeue
	ReasonExpiredExecution = "expired_execution"  // SLO deadline fired during execution; partial results preserved
	ReasonNone             = "none"               // no additional reason beyond the result (e.g. success, cancelled)

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
	jobsProcessed                 *prometheus.CounterVec
	jobProcessingDuration         *prometheus.HistogramVec
	jobQueueWaitDuration          *prometheus.HistogramVec
	totalWorkers                  prometheus.Gauge
	activeWorkers                 prometheus.Gauge
	requestErrorsModelTotal       *prometheus.CounterVec
	processorInflightRequests     prometheus.Gauge
	processorMaxInflightConc      prometheus.Gauge
	planBuildDuration             *prometheus.HistogramVec
	modelInflightRequests         *prometheus.GaugeVec
	modelRequestExecutionDuration *prometheus.HistogramVec
	fileUploadRetriesTotal        *prometheus.CounterVec
	startupRecoveryTotal          *prometheus.CounterVec
)

// FileType labels for file upload metrics.
type FileType string

const (
	FileTypeOutput FileType = "output"
	FileTypeError  FileType = "error"
)

func InitMetrics(cfg config.ProcessorConfig) error {
	// number of jobs processed
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
	requestErrorsModelTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "request_errors_by_model_total",
			Help: "Total number of request errors by model",
		},
		[]string{"model"},
	)

	// global in-flight request count during execution
	processorInflightRequests = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "processor_inflight_requests",
			Help: "Current number of in-flight inference requests for the processor",
		},
	)

	// configured GlobalConcurrency value for utilization calculation
	processorMaxInflightConc = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "processor_max_inflight_concurrency",
			Help: "Configured maximum number of concurrent in-flight inference requests (GlobalConcurrency)",
		},
	)
	processorMaxInflightConc.Set(float64(cfg.GlobalConcurrency))

	// ingestion plan build duration
	planBuildDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "plan_build_duration_seconds",
			Help: "Duration of ingestion and plan build in seconds",
			Buckets: prometheus.ExponentialBuckets(
				cfg.ProcessTimeBucket.BucketStart,
				cfg.ProcessTimeBucket.BucketFactor,
				cfg.ProcessTimeBucket.BucketCount,
			),
		}, []string{"tenantID", "size_bucket"},
	)

	// per-model in-flight requests during execution
	modelInflightRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "model_inflight_requests",
			Help: "Current number of in-flight inference requests per model",
		},
		[]string{"model"},
	)

	// per-request execution duration by model
	modelRequestExecutionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "model_request_execution_duration_seconds",
			Help: "Per-request execution duration in seconds by model",
			Buckets: prometheus.ExponentialBuckets(
				cfg.ProcessTimeBucket.BucketStart,
				cfg.ProcessTimeBucket.BucketFactor,
				cfg.ProcessTimeBucket.BucketCount,
			),
		}, []string{"model"},
	)

	// job processing duration
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
			Name: "job_queue_wait_duration_seconds",
			Help: "Time spent in the priority queue before being picked up",
			Buckets: prometheus.ExponentialBuckets(
				cfg.QueueTimeBucket.BucketStart,
				cfg.QueueTimeBucket.BucketFactor,
				cfg.QueueTimeBucket.BucketCount,
			),
		}, []string{"tenantID"},
	)

	// upload retries by file type (output / error)
	fileUploadRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "file_upload_retries_total",
			Help: "Total number of file upload retry attempts by file type",
		},
		[]string{"file_type"},
	)

	// Startup recovery: counts jobs discovered in workdir after a container restart.
	// Non-zero values indicate container-level crashes (OOM, panic) occurred.
	//
	// Operational signals:
	//   - status="in_progress" is high → frequent crashes during inference.
	//   - status="finalizing" is high → frequent crashes during upload.
	//   - action="re_enqueued" is high → most crashes happen early (low wasted inference cost).
	//   - action="failed" is high → significant inference results lost on crash.
	//   - action="finalized" → jobs successfully completed despite crash.
	//   - Sustained failed{status="in_progress"} suggests checkpoint/resume investment.
	startupRecoveryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "batch_startup_recovery_total",
			Help: "Jobs recovered during processor startup after container restart",
		},
		[]string{"status", "action"},
	)

	// metrics to register
	metricsToRegister := []prometheus.Collector{
		jobProcessingDuration,
		jobQueueWaitDuration,
		totalWorkers,
		activeWorkers,
		jobsProcessed,
		requestErrorsModelTotal,
		processorInflightRequests,
		processorMaxInflightConc,
		planBuildDuration,
		modelInflightRequests,
		modelRequestExecutionDuration,
		fileUploadRetriesTotal,
		startupRecoveryTotal,
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

// RecordRequestError increments the error count for a specific model.
func RecordRequestError(model string) {
	requestErrorsModelTotal.WithLabelValues(model).Inc()
}

// IncProcessorInflightRequests increments the processor global in-flight request gauge.
func IncProcessorInflightRequests() {
	processorInflightRequests.Inc()
}

// DecProcessorInflightRequests decrements the processor global in-flight request gauge.
func DecProcessorInflightRequests() {
	processorInflightRequests.Dec()
}

// RecordPlanBuildDuration observes ingestion plan build duration.
func RecordPlanBuildDuration(duration time.Duration, tenantID string, sizeBucket string) {
	planBuildDuration.WithLabelValues(tenantID, sizeBucket).Observe(duration.Seconds())
}

// IncModelInflightRequests increments the in-flight request gauge for a model.
func IncModelInflightRequests(model string) {
	modelInflightRequests.WithLabelValues(model).Inc()
}

// DecModelInflightRequests decrements the in-flight request gauge for a model.
func DecModelInflightRequests(model string) {
	modelInflightRequests.WithLabelValues(model).Dec()
}

// RecordModelRequestExecutionDuration observes per-request execution duration by model.
func RecordModelRequestExecutionDuration(duration time.Duration, model string) {
	modelRequestExecutionDuration.WithLabelValues(model).Observe(duration.Seconds())
}

// RecordFileUploadRetry increments the upload retry counter for a given file type.
func RecordFileUploadRetry(fileType FileType) {
	fileUploadRetriesTotal.WithLabelValues(string(fileType)).Inc()
}

// RecordStartupRecovery increments the startup recovery counter.
func RecordStartupRecovery(status, action string) {
	startupRecoveryTotal.WithLabelValues(status, action).Inc()
}

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
	"testing"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// replace with a new registry for every test
func withIsolatedPromRegistry(t *testing.T, fn func(reg *prometheus.Registry)) {
	t.Helper()

	oldReg := prometheus.DefaultRegisterer
	oldGather := prometheus.DefaultGatherer

	reg := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = reg
	prometheus.DefaultGatherer = reg

	t.Cleanup(func() {
		prometheus.DefaultRegisterer = oldReg
		prometheus.DefaultGatherer = oldGather
	})

	fn(reg)
}

func TestGetSizeBucket(t *testing.T) {
	cases := []struct {
		lines int
		want  string
	}{
		{0, Bucket100},
		{99, Bucket100},
		{100, Bucket1000},
		{999, Bucket1000},
		{1000, Bucket10000},
		{9999, Bucket10000},
		{10000, Bucket30000},
		{29999, Bucket30000},
		{30000, BucketLarge},
		{999999, BucketLarge},
	}

	for _, tc := range cases {
		got := GetSizeBucket(tc.lines)
		if got != tc.want {
			t.Fatalf("GetSizeBucket(%d)=%q, want %q", tc.lines, got, tc.want)
		}
	}
}

func TestInitMetrics_AndRecorders(t *testing.T) {
	withIsolatedPromRegistry(t, func(reg *prometheus.Registry) {
		cfg := *config.NewConfig()
		cfg.NumWorkers = 7

		if err := InitMetrics(cfg); err != nil {
			t.Fatalf("InitMetrics() error: %v", err)
		}

		// recorder calls
		RecordJobProcessed(ResultSuccess, ReasonNone)
		RecordJobProcessed(ResultFailed, ReasonSystemError)

		RecordQueueWaitDuration(250*time.Millisecond, "tenantA")
		RecordJobProcessingDuration(1500*time.Millisecond, "tenantA", Bucket1000)

		IncActiveWorkers()
		IncActiveWorkers()
		DecActiveWorkers()

		RecordRequestError("test")
		RecordRequestError("test")
		IncProcessorInflightRequests()
		IncProcessorInflightRequests()
		DecProcessorInflightRequests()
		RecordPlanBuildDuration(2*time.Second, "tenantA", Bucket1000)
		IncModelInflightRequests("modelA")
		IncModelInflightRequests("modelA")
		DecModelInflightRequests("modelA")
		RecordModelRequestExecutionDuration(300*time.Millisecond, "modelA")
		RecordFileUploadRetry(FileTypeOutput)
		RecordFileUploadRetry(FileTypeOutput)
		RecordFileUploadRetry(FileTypeError)

		// gather + value check
		mfs, err := reg.Gather()
		if err != nil {
			t.Fatalf("Gather() error: %v", err)
		}

		// helpers
		find := func(name string) *dto.MetricFamily {
			for _, mf := range mfs {
				if mf.GetName() == name {
					return mf
				}
			}
			return nil
		}

		// total_workers gauge == cfg.NumWorkers
		{
			mf := find("total_workers")
			if mf == nil || len(mf.Metric) != 1 {
				t.Fatalf("total_workers metric not found or invalid")
			}
			got := mf.Metric[0].GetGauge().GetValue()
			if got != float64(cfg.NumWorkers) {
				t.Fatalf("total_workers=%v, want %v", got, float64(cfg.NumWorkers))
			}
		}

		// active_workers gauge == 1 (2 inc, 1 dec)
		{
			mf := find("active_workers")
			if mf == nil || len(mf.Metric) != 1 {
				t.Fatalf("active_workers metric not found or invalid")
			}
			got := mf.Metric[0].GetGauge().GetValue()
			if got != 1 {
				t.Fatalf("active_workers=%v, want %v", got, 1)
			}
		}

		// jobs_processed_total counter: (success/none)=1, (failed/system_error)=1
		{
			mf := find("jobs_processed_total")
			if mf == nil {
				t.Fatalf("jobs_processed_total not found")
			}
			// label match > count check
			var successNone, failedSys float64
			for _, m := range mf.Metric {
				var result, reason string
				for _, lp := range m.Label {
					if lp.GetName() == "result" {
						result = lp.GetValue()
					}
					if lp.GetName() == "reason" {
						reason = lp.GetValue()
					}
				}
				val := m.GetCounter().GetValue()
				if result == ResultSuccess && reason == ReasonNone {
					successNone = val
				}
				if result == ResultFailed && reason == ReasonSystemError {
					failedSys = val
				}
			}
			if successNone != 1 {
				t.Fatalf("jobs_processed_total{result=%q,reason=%q}=%v, want 1", ResultSuccess, ReasonNone, successNone)
			}
			if failedSys != 1 {
				t.Fatalf("jobs_processed_total{result=%q,reason=%q}=%v, want 1", ResultFailed, ReasonSystemError, failedSys)
			}
		}

		{
			mf := find("request_errors_by_model_total")
			if mf == nil {
				t.Fatalf("request_errors_by_model_total not found")
			}
			var got float64
			for _, m := range mf.Metric {
				var model string
				for _, lp := range m.Label {
					if lp.GetName() == "model" {
						model = lp.GetValue()
					}
				}
				if model == "test" {
					got = m.GetCounter().GetValue()
				}
			}
			if got != 2 {
				t.Fatalf("request_errors_by_model_total{model=%q}=%v, want 2", "test", got)
			}
		}

		// histogram minimal observation check
		{
			mf := find("job_queue_wait_duration_seconds")
			if mf == nil || len(mf.Metric) == 0 {
				t.Fatalf("job_queue_wait_duration_seconds not found")
			}
			if mf.Metric[0].GetHistogram().GetSampleCount() < 1 {
				t.Fatalf("expected at least 1 observation")
			}
		}
		{
			mf := find("job_processing_duration_seconds")
			if mf == nil {
				t.Fatalf("job_processing_duration_seconds not found")
			}
			if len(mf.Metric) == 0 {
				t.Fatalf("job_processing_duration_seconds has no metrics")
			}
		}
		{
			mf := find("processor_inflight_requests")
			if mf == nil || len(mf.Metric) != 1 {
				t.Fatalf("processor_inflight_requests metric not found or invalid")
			}
			got := mf.Metric[0].GetGauge().GetValue()
			if got != 1 {
				t.Fatalf("processor_inflight_requests=%v, want %v", got, 1)
			}
		}
		{
			mf := find("plan_build_duration_seconds")
			if mf == nil || len(mf.Metric) == 0 {
				t.Fatalf("plan_build_duration_seconds not found")
			}
			if mf.Metric[0].GetHistogram().GetSampleCount() < 1 {
				t.Fatalf("expected at least 1 observation for plan_build_duration_seconds")
			}
		}
		{
			mf := find("model_inflight_requests")
			if mf == nil || len(mf.Metric) == 0 {
				t.Fatalf("model_inflight_requests not found")
			}
			var got float64
			for _, m := range mf.Metric {
				var model string
				for _, lp := range m.Label {
					if lp.GetName() == "model" {
						model = lp.GetValue()
					}
				}
				if model == "modelA" {
					got = m.GetGauge().GetValue()
				}
			}
			if got != 1 {
				t.Fatalf("model_inflight_requests{model=%q}=%v, want 1", "modelA", got)
			}
		}
		{
			mf := find("model_request_execution_duration_seconds")
			if mf == nil || len(mf.Metric) == 0 {
				t.Fatalf("model_request_execution_duration_seconds not found")
			}
			if mf.Metric[0].GetHistogram().GetSampleCount() < 1 {
				t.Fatalf("expected at least 1 observation for model_request_execution_duration_seconds")
			}
		}
		{
			mf := find("file_upload_retries_total")
			if mf == nil {
				t.Fatalf("file_upload_retries_total not found")
			}
			var outputRetries, errorRetries float64
			for _, m := range mf.Metric {
				for _, lp := range m.Label {
					if lp.GetName() == "file_type" {
						switch lp.GetValue() {
						case string(FileTypeOutput):
							outputRetries = m.GetCounter().GetValue()
						case string(FileTypeError):
							errorRetries = m.GetCounter().GetValue()
						}
					}
				}
			}
			if outputRetries != 2 {
				t.Fatalf("file_upload_retries_total{file_type=%q}=%v, want 2", FileTypeOutput, outputRetries)
			}
			if errorRetries != 1 {
				t.Fatalf("file_upload_retries_total{file_type=%q}=%v, want 1", FileTypeError, errorRetries)
			}
		}
		{
			mf := find("processor_max_inflight_concurrency")
			if mf == nil || len(mf.Metric) != 1 {
				t.Fatalf("processor_max_inflight_concurrency metric not found or invalid")
			}
			got := mf.Metric[0].GetGauge().GetValue()
			if got != float64(cfg.GlobalConcurrency) {
				t.Fatalf("processor_max_inflight_concurrency=%v, want %v", got, float64(cfg.GlobalConcurrency))
			}
		}
	})
}

func TestInitMetrics_Twice_DoesNotError(t *testing.T) {
	withIsolatedPromRegistry(t, func(reg *prometheus.Registry) {
		cfg := *config.NewConfig()

		if err := InitMetrics(cfg); err != nil {
			t.Fatalf("InitMetrics first error: %v", err)
		}
		// AlreadyRegisteredError is passed with continue. expect no error.
		if err := InitMetrics(cfg); err != nil {
			t.Fatalf("InitMetrics second error: %v", err)
		}
	})
}

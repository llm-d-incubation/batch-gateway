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

// this file contains the worker logic for processing batch requests.
package worker

import (
	"context"
	"time"

	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
)

type Processor struct {
	cfg          *config.ProcessorConfig
	workerPool   *WorkerPool
	llmClient    *batch_shared.LLMClient
	dbConnection *batch_shared.DBConnection
}

func NewWorkerPool(maxWorkers int) *WorkerPool {
	return &WorkerPool{
		sem: make(chan struct{}, maxWorkers),
	}
}

func NewProcessor(cfg *config.ProcessorConfig) *Processor {
	return &Processor{
		cfg:        cfg,
		workerPool: NewWorkerPool(cfg.MaxWorkers),
	}
}

// InitResources initializes any resources needed by the processor.
func (p *Processor) InitResources(ctx context.Context) error {
	// Initialize database connections, caches, error check etc. here.
	// Initially all workers are idle.
	metrics.IdleWorkers.Set(float64(p.cfg.MaxWorkers))
	metrics.ActiveWorkers.Set(0)

	// Log initialization
	// DB connection initialization

	klog.InfoS("Processor resources initialized", "max_workers", p.cfg.MaxWorkers)
	return nil
}

// RunPollingLoop starts the main polling loop for the processor.
func (p *Processor) RunPollingLoop(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.InfoS("Shutting down polling loop")
			return nil
		case <-ticker.C:
			// Poll for new jobs from the database
			for {
				job, err := p.tryAssignJobs(ctx)
				if err != nil {
					klog.ErrorS(err, "Error while trying to assign jobs")
					break
				}
				if job == nil {
					break // no more jobs to assign
				}

				go p.startWorker(ctx, job)
			}
		}
	}
}

// Stop gracefully stops the processor, waiting for all workers to finish.
func (p *Processor) Stop() {
	p.workerPool.WaitAll()
	klog.InfoS("All workers have finished")
}

// tryAssignJobs tries to assign jobs if there are available worker slots.
func (p *Processor) tryAssignJobs(ctx context.Context) (*Job, error) {
	// Check for available worker slots
	if !p.workerPool.TryAcquire() {
		// No available slots
		return nil, nil
	}

	// Poll the database for new jobs
	job, err := pollForJobFromDatabase(ctx)
	if err != nil {
		p.workerPool.Release() // release the slot on error
		return nil, err
	}
	if job == nil {
		// No new jobs available
		p.workerPool.Release() // release the slot if no job found
		return nil, nil
	}

	// Job assigned successfully
	metrics.IdleWorkers.Dec()
	metrics.ActiveWorkers.Inc()
	return job, nil
}

// startWorker starts a new worker to process the given job.
func (p *Processor) startWorker(ctx context.Context, job *Job) {
	defer func() {
		p.workerPool.Release()
		metrics.ActiveWorkers.Dec()
		metrics.IdleWorkers.Inc()
	}()

	startTime := time.Now()
	err := processJob(ctx, job)
	duration := time.Since(startTime).Seconds()
	metrics.JobProcessingDuration.Observe(duration)

	if err != nil {
		klog.ErrorS(err, "Error processing job", "job_id", job.ID)
		metrics.JobErrorsModelTotal.WithLabelValues(job.Model).Inc()
		return
	}
	metrics.JobsProcessed.Inc()
	klog.InfoS("Job processed successfully", "job_id", job.ID, "duration_seconds", duration)
}

// pollForJobFromDatabase is currently a mock. it simulates polling the database for a new job.
func pollForJobFromDatabase(ctx context.Context) (*Job, error) {
	// Simulate database polling logic here.
	// Return nil if no job is available.
	return &Job{ID: "job123", Model: "gpt-4"}, nil
}

// processJob is currently a mock func. it simulates processing a job.
func processJob(ctx context.Context, job *Job) error {
	// Simulate job processing logic here.
	time.Sleep(2 * time.Second) // simulate processing time
	return nil
}

// Job represents a processing job.
type Job struct {
	RequetId    string
	Model       string
	ID          string
	SLO         string
	EndPoint    string
	RequestBody []byte
	//... other job fields
}

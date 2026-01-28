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
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

type ProcessorClients struct {
	database      db.BatchDBClient
	priorityQueue db.BatchPriorityQueueClient
	status        db.BatchStatusClient
	event         db.BatchEventChannelClient
	inference     inference.Client
}

func NewProcessorClients(
	db db.BatchDBClient,
	pq db.BatchPriorityQueueClient,
	status db.BatchStatusClient,
	event db.BatchEventChannelClient,
	inference inference.Client,
) ProcessorClients {
	return ProcessorClients{
		database:      db,
		priorityQueue: pq,
		status:        status,
		event:         event,
		inference:     inference,
	}
}

type Processor struct {
	cfg        *config.ProcessorConfig
	workerPool *WorkerPool

	clients *ProcessorClients
}

func NewProcessor(
	cfg *config.ProcessorConfig,
	clients *ProcessorClients,
) *Processor {
	return &Processor{
		cfg:        cfg,
		workerPool: NewWorkerPool(cfg.NumWorkers),
		clients:    clients,
	}
}

func (pc *ProcessorClients) Validate() error {
	if pc.database == nil {
		return fmt.Errorf("database client is missing")
	}
	if pc.priorityQueue == nil {
		return fmt.Errorf("priority queue client is missing")
	}
	if pc.status == nil {
		return fmt.Errorf("status client is missing")
	}
	if pc.event == nil {
		return fmt.Errorf("event channel client is missing")
	}
	if pc.inference == nil {
		return fmt.Errorf("inference client is missing")
	}
	return nil
}

// pre-flight check - need to add more checks here
func (p *Processor) prepare(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	if err := p.clients.Validate(); err != nil {
		return fmt.Errorf("critical clients are missing in processor: %w", err)
	}

	logger.V(logging.DEBUG).Info("Processor pre-flight check done", "max_workers", p.cfg.NumWorkers)
	return nil
}

// TODO: events implementation (cancel, pause, resume)
// RunPollingLoop runs the main job polling loop for the processor, try assign the job to the worker,
func (p *Processor) RunPollingLoop(ctx context.Context) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info(
		"Polling loop started",
		"loopInterval", p.cfg.PollInterval,
		"maxWorkers", p.cfg.NumWorkers,
	)

	// worker driven non-busy wait
	for {
		var workerId int
		select {
		case <-ctx.Done():
			return nil
		case id, ok := <-p.workerPool.workerIds: // wait until at least one worker is available
			if !ok {
				return nil
			}
			workerId = id
		}

		// check queue for available tasks
		task := p.getTaskFromQueue(ctx)

		// when there's no waiting tasks in the queue
		if task == nil {
			p.workerPool.Release(workerId)
			// wait for poll interval to protect db from frequent queueing
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(p.cfg.PollInterval):
				continue
			}
		}

		// get detailed job info for processor
		jobDbData, err := p.getJobData(ctx, task)
		if err != nil {
			p.workerPool.Release(workerId)
			continue
		}

		// TODO:: get tenant id from job.Spec
		// tenantID := "unknown"
		// TODO:: job queue object should have enqueued at field (maybe updated at too)
		// TODO:: metrics.RecordQueueWait(time.Since(task.EnqueuedAt), tenantID)

		// process job
		go func(wid int, j *db.BatchJob) {
			defer func() {
				if r := recover(); r != nil {
					recoverErr := fmt.Errorf("%v", r)
					logger.V(logging.ERROR).Error(recoverErr, "Panic recovered", "workerID", wid)
				}
				p.workerPool.Release(wid)
				metrics.DecActiveWorkers()
			}()

			metrics.IncActiveWorkers()
			p.processJob(ctx, wid, j)
		}(workerId, jobDbData)
	}
}

// getTask is executed when at least one worker is available
func (p *Processor) getTaskFromQueue(ctx context.Context) *db.BatchJobPriority {
	logger := klog.FromContext(ctx)

	tasks, err := p.clients.priorityQueue.Dequeue(ctx, 0, 1) // get only one job without blocking the queue
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to dequeue a batch job")
		return nil
	}

	// there's no backlog
	if len(tasks) == 0 {
		logger.V(logging.TRACE).Info("No jobs to fetch")
		return nil
	}

	logger.V(logging.DEBUG).Info("Successfully fetched a job", "jobID", tasks[0].ID)
	return tasks[0]
}

// getJobData gets job's db data
func (p *Processor) getJobData(ctx context.Context, task *db.BatchJobPriority) (*db.BatchJob, error) {
	logger := klog.FromContext(ctx)

	// get only one job data
	ids := []string{task.ID}
	jobs, _, err := p.clients.database.Get(ctx, ids, nil, db.TagsLogicalCondNa, true, 0, 1)

	// job db data does not exist or failed to fetch the data
	if err != nil || len(jobs) == 0 {
		jobDataErr := err
		if len(jobs) == 0 {
			jobDataErr = fmt.Errorf("Job data for %s does not exist", task.ID)
		}
		logger.V(logging.ERROR).Error(jobDataErr, "Failed to fetch detailed job info. re-queueing ID", "jobID", task.ID)

		// can't process the job. put the task back to the queue.
		if enqueueErr := p.clients.priorityQueue.Enqueue(ctx, task); enqueueErr != nil {
			logger.V(logging.ERROR).Error(enqueueErr, "CRITICAL: Failed to re-enqueue job", "jobID", task.ID)
		}
		return nil, jobDataErr
	}

	logger.V(logging.TRACE).Info("Job DB Data retrieved", "jobID", task.ID)
	return jobs[0], nil
}

// TODO:: complete job processing logic
// read input file, streaming, line processing, result writing, etc.
// TODO:: add event handling (cancel, pause, resume)
// TODO:: add metrics (job duration, job processed, job result, job failure reason)
// TODO:: add logging (job started, job finished, job failed)
// TODO:: add error handling (error handling. inference request failed)
// TODO:: add response handling (response handling + writing line to the output file ...)
// TODO:: add output file writing (output file writing)
// TODO:: add output file reading (output file reading)
// TODO:: add output file closing (output file closing)
func (p *Processor) processJob(ctx context.Context, workerId int, job *db.BatchJob) {
	// logger and ctx
	logger := klog.FromContext(ctx).WithValues("jobID", job.ID, "workerID", workerId)
	jobctx := klog.NewContext(ctx, logger)

	// metrics
	startTime := time.Now()
	metadata := batch.JobResultMetadata{}
	defer func() {
		// TODO:: get tenant id from job.Spec (should be included in the job object)
		// job result / failure reason for metric
		// TODO:: how to check if the failure is on user or system
		tenantID := "unknown"
		jobFailureReason := metrics.ReasonUnknown
		jobResult := metrics.ResultSuccess

		metrics.RecordJobProcessingDuration(time.Since(startTime), tenantID, metrics.GetSizeBucket(metadata.Total))
		metrics.RecordJobProcessed(jobResult, jobFailureReason)
	}()

	// status update - inprogress (TTL 24h)
	p.clients.status.Set(jobctx, job.ID, 24*60*60, []byte(batch.StatusInProgress))
	logger.V(logging.DEBUG).Info("Worker started job", "workerID", workerId, "jobID", job.ID)

	// TODO:: file validating
	p.clients.status.Set(jobctx, job.ID, 24*60*60, []byte(batch.StatusValidating))

	// TODO:: download file, streaming
	// check if the method in the request is allowed
	// check if the model in the request is allowed (optional)
	// set total request num in result obj + init other fields
	// goroutine per one line reading
	// limit goroutines using config's max job concurrency
	sem := make(chan struct{}, p.cfg.MaxJobConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex // for metadata update

	// TODO:: mock file lines
	lines := []string{"req1", "req2", "req3"}

	// result metadata init
	metadata = batch.JobResultMetadata{
		Total:     len(lines),
		Succeeded: 0,
		Failed:    0,
	}

	// TODO:: read lines + process (mockup)
	lineChan := make(chan string)
	go func() {
		for _, l := range lines {
			lineChan <- l
		}
		close(lineChan)
	}()

	for line := range lineChan {
		// check context termination
		select {
		case <-jobctx.Done():
			logger.V(logging.INFO).Info("Stopping line processing due to shutdown")
			return
		case sem <- struct{}{}: // wait here if max concurrency is reached
		}
		wg.Add(1)
		go func(l string) {
			defer func() {
				<-sem
				wg.Done()
			}()

			// check again for signal in the goroutine
			select {
			case <-jobctx.Done():
				return
			default:
			}
			// TODO:: line parsing
			// TODO:: check allowed methods
			// TODO:: request validation

			// mock request
			mockRequest := &inference.GenerateRequest{}
			result, err := p.clients.inference.Generate(jobctx, mockRequest)

			// shared resources (metadata / totaljoblines) lock
			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				p.handleError(jobctx, err)
				metadata.Failed++
				return
			}

			if err := p.handleResponse(jobctx, result); err != nil {
				metadata.Failed++
			} else {
				metadata.Succeeded++
			}
		}(line)

	}
	wg.Wait()

	// final status decision
	// TODO:: final status decision (should be included in the job object)
	// openai batch set the job as completed even there are some failures - should we do the same?
	// failed status is used when the file is not valid or the batch request is not started properly
	finalStatus := batch.StatusCompleted
	if !metadata.Validate() {
		logger.V(logging.WARNING).Info("Job finished with partial failures", "jobID", job.ID, "metadata", metadata)
		// TODO:: finalStatus = batch.Failed
	}

	// status update
	p.clients.status.Set(jobctx, job.ID, 24*60*60, []byte(batch.StatusFinalizing))

	// db update (job.Status should be updated before this line)
	if err := p.clients.database.Update(jobctx, job); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update final job status in DB", "jobID", job.ID)
	}
	p.clients.status.Set(jobctx, job.ID, 24*60*60, []byte(finalStatus))
	logger.V(logging.INFO).Info("Job Processed", "jobID", job.ID, "status", finalStatus)
}

func (p *Processor) handleError(ctx context.Context, err error) {
	// TODO:: error handling.
	logger := klog.FromContext(ctx)
	logger.V(logging.ERROR).Error(err, "Inference request failed")
}

func (p *Processor) handleResponse(ctx context.Context, inferenceResponse *inference.GenerateResponse) error {
	// TODO:: response handling + writing line to the output file ...
	logger := klog.FromContext(ctx)
	logger.V(logging.DEBUG).Info("Handling response")
	return nil
}

// Stop gracefully stops the processor, waiting for all workers to finish.
func (p *Processor) Stop(ctx context.Context) {
	logger := klog.FromContext(ctx)
	p.workerPool.WaitAll()
	logger.V(logging.INFO).Info("All workers have finished")
}

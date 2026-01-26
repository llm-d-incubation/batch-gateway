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
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch"
)

type ProcessorClients struct {
	Database      db.BatchDBClient
	PriorityQueue db.BatchPriorityQueueClient
	Status        db.BatchStatusClient
	Event         db.BatchEventChannelClient
	Inference     batch.InferenceClient
}
type Processor struct {
	cfg        *config.ProcessorConfig
	workerPool *WorkerPool

	clients ProcessorClients
}

func NewProcessor(
	cfg *config.ProcessorConfig,
	clients ProcessorClients,
) *Processor {
	return &Processor{
		cfg:        cfg,
		workerPool: NewWorkerPool(cfg.MaxWorkers),
		clients:    clients,
	}
}

// pre-flight check
func (p *Processor) prepare(ctx context.Context) error {
	// check injections
	if p.clients.Database == nil || p.clients.Inference == nil || p.clients.PriorityQueue == nil || p.clients.Event == nil || p.clients.Status == nil {
		return fmt.Errorf("critical clients are missing in Processor")
	}

	// init metrics - all workers are initially idle.
	metrics.IdleWorkers.Set(float64(p.cfg.MaxWorkers))
	metrics.ActiveWorkers.Set(0)

	klog.InfoS("Processor pre-flight check done", "max_workers", p.cfg.MaxWorkers)
	return nil
}

// RunPollingLoop runs the main job polling loop for the processor, try assign the job to the worker,
func (p *Processor) RunPollingLoop(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.InfoS("Shutting down polling loop")
			return nil
		case <-ticker.C:
			// dispatch new jobs from the database
			klog.InfoS("Job polling loop ticked")
			tasks := p.dispatchTasks(ctx)
			if len(tasks) == 0 {
				continue
			}
			// process tasks
			p.processJobs(ctx, tasks)
		}
	}
}

// dispatchTasks is responsible to dispatch max number of tasks considering the number of current idle workers
func (p *Processor) dispatchTasks(ctx context.Context) []*db.BatchJobPriority {
	// check cancel signal
	select {
	case <-ctx.Done():
		return nil
	default:
	}

	// check how many workers are available
	availableWorkers := len(p.workerPool.workerIds)
	klog.InfoS("Dispathcing jobs", "available_workers", availableWorkers)

	if availableWorkers <= 0 {
		return nil // skip this turn as there's no available workers
	}

	// dequeue max number of tasks
	tasks, err := p.clients.PriorityQueue.Dequeue(ctx, p.cfg.TaskWaitTime, availableWorkers)
	if err != nil {
		klog.ErrorS(err, "Failed to dequeue batch jobs")
		return nil
	}
	klog.InfoS("Dispatched jobs", "jobs", len(tasks))

	return tasks
}

func (p *Processor) processJobs(ctx context.Context, tasks []*db.BatchJobPriority) {
	// get all job ids from pq & create a task/id map for fast search/db fetch
	klog.InfoS("Fetching detailed job data from DB")
	taskMap := make(map[string]*db.BatchJobPriority)
	ids := make([]string, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
		taskMap[task.ID] = task
	}

	// get all job db data
	jobs, _, err := p.clients.Database.Get(ctx, ids, nil, db.TagsLogicalCondNa, true, 0, len(ids))
	if err != nil {
		klog.ErrorS(err, "Failed to fetch detailed job info, re-queueing all IDs")
		for _, task := range tasks {
			p.clients.PriorityQueue.Enqueue(ctx, task)
		}
		klog.InfoS("Successfully re-queued IDs")
		return
	}

	klog.InfoS("Fetched job data from DB")

	// worker assigning loop
	for _, job := range jobs {
		// get assigned worker id
		workerId, ok := p.workerPool.TryAcquire()
		if !ok {
			// edge case: no worker available - this can happen if dispatching took too long?
			// find the original task BatchJobPriority then enqueue
			if originalTask, exists := taskMap[job.ID]; exists {
				klog.Warning("message", "No Worker available, re-queueing jobs", "jobID", job.ID)
				err := p.clients.PriorityQueue.Enqueue(ctx, originalTask)
				if err != nil {
					klog.ErrorS(err, "CRITICAL: Failed to re-enqueue job", "jobID", originalTask.ID, "SLO", originalTask.SLO)
				}
			}
			continue
		}

		// run goroutine for each job
		go func(id int, j *db.BatchJob) {
			// release resources
			defer func() {
				if r := recover(); r != nil {
					klog.ErrorS(fmt.Errorf("%v", r), "Panic recovered in worker", "workerID", id, "jobID", j.ID)
				}
				p.workerPool.Release(id)
				defer metrics.ActiveWorkers.Dec()
				defer metrics.IdleWorkers.Inc()
			}()

			// metric & status update
			metrics.ActiveWorkers.Inc()
			metrics.IdleWorkers.Dec()
			p.processJob(ctx, id, j)
		}(workerId, job)
	}
}

func (p *Processor) processJob(ctx context.Context, workerId int, job *db.BatchJob) {
	// status update - inprogress (TTL 24h)
	p.clients.Status.Set(ctx, job.ID, 24*60*60, []byte(batch.InProgress.String()))
	klog.InfoS("Worker started job", "workerID", workerId, "jobID", job.ID)

	// TODO:: file validating
	p.clients.Status.Set(ctx, job.ID, 24*60*60, []byte(batch.Validating.String()))

	// TODO:: download file, streaming
	// check if the method in the request is allowed
	// check if the model in the request is allowed (optional)
	// set total request num in result obj + init other fields
	// goroutine per one line reading
	var wg sync.WaitGroup
	var mu sync.Mutex // for metadata update

	// mock file lines
	lines := []string{"req1", "req2", "req3"}

	// result metadata init
	metadata := batch.JobResultMetadata{
		Total:     len(lines),
		Succeeded: 0,
		Failed:    0,
	}

	// read lines + process (mockup)
	lineChan := make(chan string)
	go func() {
		for _, l := range lines {
			lineChan <- l
		}
		close(lineChan)
	}()

	for line := range lineChan {
		wg.Add(1)
		go func(l string) {
			defer wg.Done()
			// TODO:: line parsing
			// TODO:: check allowed methods
			// TODO:: request validation

			// mock request
			mockRequest := &batch.InferenceRequest{}
			result, err := p.clients.Inference.Generate(ctx, mockRequest)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				p.handleError(err)
				metadata.Failed++
				return
			}

			if err := p.handleResponse(result); err != nil {
				metadata.Failed++
			}
			metadata.Succeeded++
		}(line)

	}
	wg.Wait()

	// final status decision
	finalStatus := batch.Completed
	if !metadata.Validate() {
		klog.Warning("Job finished with partial failures", "jobID", job.ID, "metadata", metadata)
		finalStatus = batch.Failed
	}

	// status update
	p.clients.Status.Set(ctx, job.ID, 24*60*60, []byte(batch.Finalizing.String()))

	// db update (job.Status should be updated before this line)
	if err := p.clients.Database.Update(ctx, job); err != nil {
		klog.ErrorS(err, "Failed to update final job status in DB", "jobID", job.ID)
	}
	p.clients.Status.Set(ctx, job.ID, 24*60*60, []byte(finalStatus.String()))
	klog.InfoS("Job Processed", "jobID", job.ID, "status", finalStatus.String())
}

func (p *Processor) handleError(err error) {
	// TODO:: error handling.
	klog.ErrorS(err, "Inference request failed")
}

func (p *Processor) handleResponse(inferenceResponse *batch.InferenceResponse) error {
	// TODO:: response handling + writing line to the output file ...
	klog.InfoS("Handling response")
	return nil
}

// Stop gracefully stops the processor, waiting for all workers to finish.
func (p *Processor) Stop() {
	p.workerPool.WaitAll()
	klog.InfoS("All workers have finished")
}

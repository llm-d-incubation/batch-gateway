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
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"github.com/llm-d-incubation/batch-gateway/internal/util/semaphore"
	"github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
)

type Processor struct {
	cfg    *config.ProcessorConfig
	tokens semaphore.Semaphore
	wg     sync.WaitGroup

	// globalSem limits total in-flight inference requests across all workers.
	globalSem semaphore.Semaphore

	poller  *Poller
	updater *StatusUpdater

	event     db.BatchEventChannelClient // cancel-event subscription
	inference *inference.GatewayResolver // model → gateway routing
	files     *fileManager
}

func NewProcessor(
	cfg *config.ProcessorConfig,
	clients *clientset.Clientset,
) (*Processor, error) {
	tokenSem, err := semaphore.New(cfg.NumWorkers)
	if err != nil {
		return nil, fmt.Errorf("worker semaphore (NumWorkers=%d): %w", cfg.NumWorkers, err)
	}
	globalSem, err := semaphore.New(cfg.GlobalConcurrency)
	if err != nil {
		return nil, fmt.Errorf("global semaphore (GlobalConcurrency=%d): %w", cfg.GlobalConcurrency, err)
	}
	poller := NewPoller(clients.Queue, clients.BatchDB)
	updater := NewStatusUpdater(clients.BatchDB, clients.Status, cfg.ProgressTTLSeconds)
	return &Processor{
		cfg:       cfg,
		tokens:    tokenSem,
		globalSem: globalSem,
		poller:    poller,
		updater:   updater,
		event:     clients.Event,
		inference: clients.Inference,
		files:     newFileManager(clients.File, clients.FileDB),
	}, nil
}

// Run starts processor orchestration and enters the polling loop.
// If onReady is provided, it is called after pre-flight checks succeed and
// right before the polling loop begins accepting work.
func (p *Processor) Run(ctx context.Context, onReady func()) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}
	if onReady != nil {
		onReady()
	}

	p.recoverStaleJobs(ctx)

	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info(
		"Processor run started",
		"loopInterval", p.cfg.PollInterval,
		"maxWorkers", p.cfg.NumWorkers,
	)

	return p.runPollingLoop(ctx)
}

// Stop gracefully stops the processor, waiting for all workers to finish.
func (p *Processor) Stop(ctx context.Context) {
	logger := klog.FromContext(ctx)
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done(): // context cancelled
		logger.V(logging.INFO).Info("Processor stopped due to context cancellation")

	case <-done: // all workers have finished
		logger.V(logging.INFO).Info("All workers have finished")
	}
}

// runPollingLoop runs the job polling loop and dispatches jobs to workers.
func (p *Processor) runPollingLoop(ctx context.Context) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Polling loop started")
	// worker driven non-busy wait
	for {
		if !p.acquire(ctx) {
			return nil
		}

		// check queue for available tasks
		logger.V(logging.DEBUG).Info("Checking queue for available tasks")
		task, err := p.poller.dequeueOne(ctx)

		// when there's no waiting tasks in the queue or poller returned an error
		if task == nil || err != nil {
			// wait for poll interval to protect db from frequent queueing
			if !p.releaseAndWaitPollInterval(ctx) {
				return nil
			}
			continue
		}

		// create a new logger for the job with job ID
		jlogger := klog.FromContext(ctx).WithValues("jobId", task.ID)
		jctx := klog.NewContext(ctx, jlogger)

		// get job item from db
		jobItem, err := p.poller.fetchJobItemByID(jctx, task.ID)
		if err != nil {
			jlogger.Error(err, "Failed to fetch job item from DB")
			p.releaseForNextPoll()
			// error is due to system issue (db connection, etc.)
			// re-enqueue the job to the queue so this job can be picked up later by another worker
			// best-effort
			jlogger.V(logging.DEBUG).Info("Re-enqueue the job to the queue")
			reEnqueueErr := p.poller.enqueueOne(jctx, task)
			if reEnqueueErr != nil {
				jlogger.Error(reEnqueueErr, "Failed to re-enqueue the job to the queue")
				metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonDBTransient)
			}
			continue
		}

		// job item is not found in the db.
		if jobItem == nil {
			jlogger.Error(fmt.Errorf("job item is not found in the DB"), "Ignoring job (data inconsistency)")
			// ignore the job (data inconsistency) and continue polling
			p.releaseForNextPoll()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonDBInconsistency)
			continue
		}

		jlogger.V(logging.TRACE).Info("Job item found in the DB")

		// queue wait metrics recording
		if jobPriorityData, err := batch_utils.GetJobPriorityDataFromQueueItem(task); err == nil {
			queueWait := time.Since(time.Unix(jobPriorityData.CreatedAt, 0))
			metrics.RecordQueueWaitDuration(queueWait, jobItem.TenantID)
			jlogger.V(logging.TRACE).Info("Queue wait duration recorded", "duration", queueWait)
		} else {
			// queue createdAt is not available.
			// log the error and continue processing as createdAt is only for metrics recording.
			jlogger.Error(err, "Failed to get job priority data from queue item")
		}

		// db job item to job info object conversion
		jobInfo, err := batch_utils.FromDBItemToJobInfoObject(jobItem)

		if err != nil {
			jlogger.Error(err, "Failed to convert job object in DB to job info object")
			p.releaseForNextPoll()
			metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			continue
		}

		// create a new logger including tenant ID (Job ID is already in the logger)
		jlogger = jlogger.WithValues("tenantId", jobInfo.TenantID)
		// update the context with the new logger
		jctx = klog.NewContext(jctx, jlogger)

		jlogger.V(logging.TRACE).Info("Job info object converted")

		if batch_utils.IsJobExpired(task) {
			jlogger.V(logging.INFO).Info("Job is expired.")

			// persistent status update (to expired status)
			if err := p.updater.UpdatePersistentStatus(jctx, jobItem, openai.BatchStatusExpired, nil, nil); err != nil {
				jlogger.V(logging.ERROR).Error(err, "Failed to update job status in DB", "newStatus", openai.BatchStatusExpired, "slo", task.SLO)
			}

			// do not need to delete the task from the queue.
			// ignore the job and continue polling
			p.releaseForNextPoll()
			metrics.RecordJobProcessed(metrics.ResultExpired, metrics.ReasonExpiredDequeue)
			continue
		}

		// job is not in runnable state.
		if !batch_utils.IsJobRunnable(jobInfo.BatchJob) {
			jlogger.V(logging.INFO).Info("job is not in processible state. skipping this job.", "status", jobInfo.BatchJob.Status)

			// persistent status update is not needed.
			// do not need to delete the task from the queue.
			// ignore the job and continue polling
			p.releaseForNextPoll()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonNotRunnableState)
			continue
		}

		// process job
		p.wg.Add(1)
		go p.runJob(jctx, &jobExecutionParams{
			updater: p.updater,
			jobItem: jobItem,
			jobInfo: jobInfo,
			task:    task,
		})
	}
}

func (p *Processor) acquire(ctx context.Context) bool {
	if err := p.tokens.Acquire(ctx); err != nil {
		return false
	}
	return true
}

func (p *Processor) release() {
	p.tokens.Release()
}

func (p *Processor) releaseAndWaitPollInterval(ctx context.Context) bool {
	p.release()
	select {
	case <-ctx.Done():
		return false
	case <-time.After(p.cfg.PollInterval):
		return true
	}
}

func (p *Processor) releaseForNextPoll() {
	p.release()
}

// pre-flight check
func (p *Processor) prepare(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	if err := p.validate(); err != nil {
		return fmt.Errorf("critical clients are missing in processor: %w", err)
	}

	logger.V(logging.DEBUG).Info("Processor pre-flight check done", "max_workers", p.cfg.NumWorkers)
	return nil
}

func (p *Processor) validate() error {
	if p.poller == nil {
		return fmt.Errorf("poller is missing")
	}
	if err := p.poller.validate(); err != nil {
		return err
	}
	if p.updater == nil {
		return fmt.Errorf("status updater is missing")
	}
	if err := p.updater.validate(); err != nil {
		return err
	}
	if p.event == nil {
		return fmt.Errorf("event channel client is missing")
	}
	if p.inference == nil {
		return fmt.Errorf("inference client is missing")
	}
	if p.files == nil {
		return fmt.Errorf("file manager is missing")
	}
	return p.files.validate()
}

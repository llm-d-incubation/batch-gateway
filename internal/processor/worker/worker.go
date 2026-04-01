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
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
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

	// guardCallback is called when any semaphore detects a double-release.
	// Set during Run(); passed to per-model semaphores in processModel.
	guardCallback func()
}

func NewProcessor(
	cfg *config.ProcessorConfig,
	clients *clientset.Clientset,
	logger logr.Logger,
) (*Processor, error) {
	if cfg.NumWorkers <= 0 {
		return nil, fmt.Errorf("worker semaphore (NumWorkers=%d): %w", cfg.NumWorkers, semaphore.ErrCap)
	}
	if cfg.GlobalConcurrency <= 0 {
		return nil, fmt.Errorf("global semaphore (GlobalConcurrency=%d): %w", cfg.GlobalConcurrency, semaphore.ErrCap)
	}
	poller := NewPoller(clients.Queue, clients.BatchDB)
	updater := NewStatusUpdater(clients.BatchDB, clients.Status, cfg.ProgressTTLSeconds)
	return &Processor{
		cfg:       cfg,
		poller:    poller,
		updater:   updater,
		event:     clients.Event,
		inference: clients.Inference,
		files:     newFileManager(clients.File, clients.FileDB),
	}, nil
}

// Run starts processor orchestration and enters the polling loop.
// If onReady is provided, it is called after pre-flight checks succeed and
// stale job recovery completes, right before the polling loop begins accepting work.
func (p *Processor) Run(ctx context.Context, onReady func()) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}

	p.recoverStaleJobs(ctx)

	if onReady != nil {
		onReady()
	}

	// Two context branches:
	//   pollingCtx — controls the polling loop; cancelled by semaphore guard or SIGTERM.
	//   ctx (unchanged) — parent for job contexts; only cancelled by SIGTERM.
	// This separation ensures stopAccepting() halts new-job intake without
	// cancelling in-flight jobs.
	pollingCtx, stopAccepting := context.WithCancel(ctx)
	defer stopAccepting()

	logger := logr.FromContextOrDiscard(ctx)

	// Create semaphores here (not in NewProcessor) so the double-release guard
	// callback can capture stopAccepting. This keeps semaphores immutable after
	// construction — no mutex, no OnDoubleRelease method.
	makeGuard := func(name string) func() {
		return func() {
			logger.Error(fmt.Errorf("semaphore double-release"), "Initiating graceful shutdown", "semaphore", name)
			stopAccepting()
		}
	}
	var err error
	p.tokens, err = semaphore.New(p.cfg.NumWorkers, makeGuard("num-workers"))
	if err != nil {
		return fmt.Errorf("worker semaphore (NumWorkers=%d): %w", p.cfg.NumWorkers, err)
	}
	p.globalSem, err = semaphore.New(p.cfg.GlobalConcurrency, makeGuard("global-concurrency"))
	if err != nil {
		return fmt.Errorf("global semaphore (GlobalConcurrency=%d): %w", p.cfg.GlobalConcurrency, err)
	}
	p.guardCallback = makeGuard("per-model-concurrency")

	logger.V(logging.INFO).Info(
		"Processor run started",
		"loopInterval", p.cfg.PollInterval,
		"maxWorkers", p.cfg.NumWorkers,
	)

	return p.runPollingLoop(pollingCtx, ctx)
}

// Stop gracefully stops the processor, waiting for all workers to finish.
func (p *Processor) Stop(ctx context.Context) {
	logger := logr.FromContextOrDiscard(ctx)
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
//
// pollingCtx controls the loop: cancelled by semaphore guard or SIGTERM.
// jobBaseCtx is the parent for per-job contexts: only cancelled by SIGTERM.
// This ensures stopAccepting() halts new-job intake without killing in-flight jobs.
func (p *Processor) runPollingLoop(pollingCtx, jobBaseCtx context.Context) error {
	logger := logr.FromContextOrDiscard(pollingCtx)
	logger.V(logging.INFO).Info("Polling loop started")
	// worker driven non-busy wait
	for {
		if !p.acquire(pollingCtx) {
			return nil
		}

		// check queue for available tasks
		logger.V(logging.DEBUG).Info("Checking queue for available tasks")
		task, err := p.poller.dequeueOne(pollingCtx)

		// when there's no waiting tasks in the queue or poller returned an error
		if task == nil || err != nil {
			// wait for poll interval to protect db from frequent queueing
			if !p.releaseAndWaitPollInterval(pollingCtx) {
				return nil
			}
			continue
		}

		// Pre-launch: use pollingCtx so guard cancel / SIGTERM interrupts
		// DB fetch and validation promptly. jobBaseCtx is only used once
		// we commit to launching the job goroutine.
		pollLogger := logr.FromContextOrDiscard(pollingCtx).WithValues("jobId", task.ID)
		pollCtx := logr.NewContext(pollingCtx, pollLogger)

		// get job item from db
		jobItem, err := p.poller.fetchJobItemByID(pollCtx, task.ID)
		if err != nil {
			pollLogger.Error(err, "Failed to fetch job item from DB")
			p.releaseForNextPoll()
			pollLogger.V(logging.DEBUG).Info("Re-enqueue the job to the queue")
			// Use a detached context because pollCtx may already be cancelled
			// (e.g. guard fired or SIGTERM arrived during the DB call).
			bgCtx, bgSpan := uotel.DetachedContext(pollCtx, "re-enqueue-fetch-failure")
			if reEnqueueErr := p.poller.enqueueOne(bgCtx, task); reEnqueueErr != nil {
				pollLogger.Error(reEnqueueErr, "Failed to re-enqueue the job to the queue")
				metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonDBTransient)
				pollLogger.V(logging.INFO).Info("Re-enqueued the job to the queue")
			}
			bgSpan.End()
			continue
		}

		// job item is not found in the db.
		if jobItem == nil {
			pollLogger.Error(fmt.Errorf("job item is not found in the DB"), "Ignoring job (data inconsistency)")
			p.releaseForNextPoll()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonDBInconsistency)
			continue
		}

		pollLogger.V(logging.TRACE).Info("Job item found in the DB")

		// queue wait metrics recording
		if jobPriorityData, err := batch_utils.GetJobPriorityDataFromQueueItem(task); err == nil {
			queueWait := time.Since(time.Unix(jobPriorityData.CreatedAt, 0))
			metrics.RecordQueueWaitDuration(queueWait)
			pollLogger.V(logging.TRACE).Info("Queue wait duration recorded", "duration", queueWait)
		} else {
			pollLogger.Error(err, "Failed to get job priority data from queue item")
		}

		// db job item to job info object conversion
		jobInfo, err := batch_utils.FromDBItemToJobInfoObject(jobItem)
		if err != nil {
			pollLogger.Error(err, "Failed to convert job object in DB to job info object")
			p.releaseForNextPoll()
			if failErr := p.handleFailed(pollCtx, p.updater, jobItem, nil, nil); failErr != nil {
				pollLogger.Error(failErr, "Failed to mark malformed job as failed")
			}
			continue
		}

		pollLogger = pollLogger.WithValues("tenantId", jobInfo.TenantID)
		pollCtx = logr.NewContext(pollCtx, pollLogger)

		pollLogger.V(logging.TRACE).Info("Job info object converted")

		if batch_utils.IsJobExpired(task) {
			pollLogger.V(logging.INFO).Info("Job is expired.")

			if err := p.updater.UpdatePersistentStatus(pollCtx, jobItem, openai.BatchStatusExpired, nil, nil); err != nil {
				pollLogger.Error(err, "Failed to update job status in DB", "newStatus", openai.BatchStatusExpired, "slo", task.SLO)
				recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
				p.releaseForNextPoll()
				metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
				continue
			}

			p.releaseForNextPoll()
			recordE2ELatency(jobInfo, metrics.E2EStatusExpired)
			metrics.RecordJobProcessed(metrics.ResultExpired, metrics.ReasonExpiredDequeue)
			continue
		}

		// job is not in runnable state.
		if !batch_utils.IsJobRunnable(jobInfo.BatchJob) {
			if jobInfo.BatchJob.Status == openai.BatchStatusCancelling {
				pollLogger.V(logging.INFO).Info("Job is in cancelling state after dequeue, transitioning to cancelled")
				if err := p.updater.UpdateCancelledStatus(pollCtx, jobItem, nil, "", ""); err != nil {
					pollLogger.Error(err, "Failed to update job status to cancelled")
					recordE2ELatency(jobInfo, metrics.E2EStatusFailed)
					p.releaseForNextPoll()
					metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
					continue
				}
				p.releaseForNextPoll()
				recordE2ELatency(jobInfo, metrics.E2EStatusCancelled)
				metrics.RecordCancellation(metrics.CancelPhaseQueued)
				metrics.RecordJobProcessed(metrics.ResultSuccess, metrics.ReasonNone)
				continue
			}

			pollLogger.V(logging.INFO).Info("job is not in processible state. skipping this job.", "status", jobInfo.BatchJob.Status)

			p.releaseForNextPoll()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonNotRunnableState)
			continue
		}

		// Guard: if pollingCtx was cancelled between dequeue and here
		// (e.g. semaphore double-release or SIGTERM), re-enqueue instead of launching.
		// Use a detached context because both pollingCtx and jobBaseCtx may already
		// be cancelled (SIGTERM cancels the parent of both).
		if pollingCtx.Err() != nil {
			pollLogger.V(logging.INFO).Info("Polling context cancelled before job launch, re-enqueueing")
			p.releaseForNextPoll()
			bgCtx, bgSpan := uotel.DetachedContext(pollCtx, "re-enqueue-guard")
			defer bgSpan.End()
			if reEnqueueErr := p.poller.enqueueOne(bgCtx, task); reEnqueueErr != nil {
				pollLogger.Error(reEnqueueErr, "Failed to re-enqueue job during graceful shutdown, marking as failed")
				if failErr := p.handleFailed(bgCtx, p.updater, jobItem, nil, jobInfo); failErr != nil {
					pollLogger.Error(failErr, "Failed to mark dequeued job as failed after re-enqueue failure")
				}
				metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonGuardShutdown)
			} else {
				metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonGuardShutdown)
				pollLogger.V(logging.INFO).Info("Re-enqueued the job to the queue during graceful shutdown")
			}
			return nil
		}

		// Commit to launching: create job context from jobBaseCtx so in-flight
		// jobs survive pollingCtx cancellation (guard shutdown).
		jobLogger := logr.FromContextOrDiscard(jobBaseCtx).WithValues("jobId", task.ID, "tenantId", jobInfo.TenantID)
		jobCtx := logr.NewContext(jobBaseCtx, jobLogger)

		p.wg.Add(1)
		go p.runJob(jobCtx, &jobExecutionParams{
			updater:         p.updater,
			jobItem:         jobItem,
			jobInfo:         jobInfo,
			task:            task,
			cancelRequested: &atomic.Bool{},
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
	logger := logr.FromContextOrDiscard(ctx)

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

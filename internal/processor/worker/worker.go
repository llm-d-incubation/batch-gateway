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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	files "github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

type ProcessorClients struct {
	database      db.BatchDBClient
	files         files.BatchFilesClient
	priorityQueue db.BatchPriorityQueueClient
	status        db.BatchStatusClient
	event         db.BatchEventChannelClient
	inference     inference.Client
}

func NewProcessorClients(
	db db.BatchDBClient,
	files files.BatchFilesClient,
	pq db.BatchPriorityQueueClient,
	status db.BatchStatusClient,
	event db.BatchEventChannelClient,
	inference inference.Client,
) ProcessorClients {
	return ProcessorClients{
		database:      db,
		files:         files,
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
	if pc.files == nil {
		return fmt.Errorf("files client is missing")
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
		var workerID int
		select {
		case <-ctx.Done():
			return nil
		case id, ok := <-p.workerPool.workerIDs: // wait until at least one worker is available
			if !ok {
				return nil
			}
			workerID = id
		}

		// check queue for available tasks
		task := p.getTaskFromQueue(ctx)

		// when there's no waiting tasks in the queue
		if task == nil {
			p.workerPool.Release(workerID)
			// wait for poll interval to protect db from frequent queueing
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(p.cfg.PollInterval): // wait for poll interval to protect db from frequent queueing if no tasks are available
				continue
			}
		}

		// queue wait should be recorded here
		// TODO:: metrics.RecordQueueWait(time.Since(task.EnqueuedAt), tenantID)

		// get detailed job info from db for processor
		jobDbData, err := p.getJobData(ctx, task)
		if err != nil {
			// this task should be skipped as the data is not in db
			// don't need to re-enqueue the task.
			p.workerPool.Release(workerID)
			metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			// we don't have enough information to record job processing duration and errored model (missing tenantID, modelID)
			// we don't have enough information to update the job status in the db
			continue
		}

		// process job (read downloaded file, process requests line by line, write responses to the output file)
		go func(c context.Context, wid int, j *db.BatchItem) {
			defer func() {
				if r := recover(); r != nil {
					recoverErr := fmt.Errorf("%v", r)
					logger.V(logging.ERROR).Error(recoverErr, "Panic recovered", "workerID", wid)
				}
				p.workerPool.Release(wid)
				metrics.DecActiveWorkers()
			}()

			metrics.IncActiveWorkers()
			p.processJob(c, wid, j)
		}(ctx, workerID, jobDbData)
	}
}

// getTask is executed when at least one worker is available
func (p *Processor) getTaskFromQueue(ctx context.Context) *db.BatchJobPriority {
	logger := klog.FromContext(ctx)

	tasks, err := p.clients.priorityQueue.PQDequeue(ctx, 0, 1) // get only one job without blocking the queue
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
func (p *Processor) getJobData(ctx context.Context, task *db.BatchJobPriority) (*db.BatchItem, error) {
	logger := klog.FromContext(ctx)

	// get only one job data
	ids := []string{task.ID}
	jobs, _, _, err := p.clients.database.DBGet(ctx,
		&db.BatchDBQuery{
			IDs: ids,
		},
		true, 0, 1)

	// job db data does not exist or failed to fetch the data
	if err != nil || len(jobs) == 0 {
		jobDataErr := err
		if len(jobs) == 0 {
			jobDataErr = fmt.Errorf("Job data for %s does not exist", task.ID)
		}
		logger.V(logging.ERROR).Error(jobDataErr, "Failed to fetch detailed job info. re-queueing ID", "jobID", task.ID)

		// can't process the job. put the task back to the queue.
		if enqueueErr := p.clients.priorityQueue.PQEnqueue(ctx, task); enqueueErr != nil {
			logger.V(logging.ERROR).Error(enqueueErr, "CRITICAL: Failed to re-enqueue job", "jobID", task.ID)
		}
		return nil, jobDataErr
	}

	logger.V(logging.TRACE).Info("Job DB Data retrieved", "jobID", task.ID)
	return jobs[0], nil
}

// TODO:: status updates and re-enqueue the job if needed
func (p *Processor) processJob(ctx context.Context, workerID int, jobDbData *db.BatchItem) {
	// logger and ctx
	logger := klog.FromContext(ctx).WithValues("jobID", jobDbData.ID, "workerID", workerID)
	jobctx := klog.NewContext(ctx, logger)
	logger.V(logging.DEBUG).Info("Worker started")

	// convert db job data to openai batch object
	job, err := batch_utils.FromDBToJobInfo(jobDbData)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to convert job object in DB to batch object")
		metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
		// skipped metrics: job processing duration / job error details
		// job processing duration recording (missing tenantID, sizeBucket)
		// job error recording (missing modelID)
		batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusFailed, nil, nil)
		return
	}

	// check if the job is expired
	if batch_utils.IsJobExpired(job.BatchJob) {
		logger.V(logging.INFO).Info("Job is expired.")
		// update the job status to expired
		if err := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusExpired, nil, nil); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusExpired)
		}
		// metrics
		metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
		metrics.RecordJobError(job.BatchJob.BatchStatusInfo.Model)
		return
	}

	// get event channel for the job
	eventsChan, err := p.clients.event.ECConsumerGetChannel(ctx, job.JobID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to get event channel")
		metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
		metrics.RecordJobError(job.BatchJob.BatchStatusInfo.Model)
		// we don't have enough information to record job processing duration(missing sizeBucket)
		if err = batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, ctx, jobDbData, openai.BatchStatusFailed, nil, nil); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusFailed)
		}
		return
	}

	// atomic request counts
	var (
		totalRequests     int64 = 0
		completedRequests int64 = 0
		failedRequests    int64 = 0
	)

	requestCounts := &openai.BatchRequestCounts{
		Total:     atomic.LoadInt64(&totalRequests),
		Completed: atomic.LoadInt64(&completedRequests),
		Failed:    atomic.LoadInt64(&failedRequests),
	}

	// initialize job result record values
	jobFailureReason := metrics.ReasonNone
	jobResult := metrics.ResultSuccess
	startTime := time.Now()

	// check if the job is in processible status
	if !batch_utils.IsJobProcessible(job.BatchJob) {
		logger.V(logging.ERROR).Error(fmt.Errorf("job is not in processible status"), "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusInProgress)
		// skip metrics recording and status update as they've been done in another process
		return
	}

	// status update - in_progress
	if err := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusInProgress, requestCounts, nil); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB. updating to failed instead.", openai.BatchStatusInProgress)
	}

	// limit goroutines using config's max job concurrency
	// limit + 2: 1 for file reader/dispatcher, 1 for writer, max job concurrency for inference requests
	processPipelineGroup, processPipelineCtx := errgroup.WithContext(ctx)
	processPipelineGroup.SetLimit(p.cfg.MaxJobConcurrency + 2)

	resultChan := make(chan *batch_utils.Response, p.cfg.MaxJobConcurrency)

	// file download - get file reader
	fileReader, _, err := p.openInputFileStream(jobctx, job.BatchJob.InputFileID)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to open input file stream")
		metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
		// skipped metrics: job processing duration / job error details
		// job processing duration recording (missing tenantID, sizeBucket)
		// job error recording (missing modelID)
		updateErr := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusFailed, nil, nil)
		if updateErr != nil {
			logger.V(logging.ERROR).Error(updateErr, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusFailed)
		}
		return
	}

	// writer goroutine to write the responses to the output file
	var localoutputPath string
	processPipelineGroup.Go(func() error {
		localoutputPath, err = p.writeResultsToFileLoop(processPipelineCtx, job.JobID, resultChan)
		if err != nil {
			return err // critical error. processPipelineCtx should be cancelled.
		}
		job.BatchJob.OutputFileID = localoutputPath
		return nil
	})

	// reader + dispatcher goroutine to process the requests
	processPipelineGroup.Go(func() error {
		// defer closing the file reader & result channel
		defer func() {
			fileReader.Close()
			close(resultChan)
		}()

		// read the input file line by line
		fileStreamScanner := bufio.NewScanner(fileReader)
		for fileStreamScanner.Scan() {
			// context check before each line processing
			select {
			case <-processPipelineCtx.Done(): // critical error occured in the group. need to stop the loop immediately
				return processPipelineCtx.Err() // stops the whole errGroup
			case <-jobctx.Done(): // process received cancellation request. need to stop the loop immediately
				logger.V(logging.INFO).Info("Job processing stopped due to system shutdown signal.")
				if err := batch_utils.UpdateRequestCountsStatus(p.clients.status, processPipelineCtx, job.JobID, requestCounts); err != nil {
					logger.V(logging.ERROR).Error(err, "Failed to update request counts status", "requestCounts", requestCounts)
					// non critical error. continue the shutdown process
				}

				// local file is deleted by the writer goroutine
				// status update in-progress and re-enqueue the job so we can try again
				if err := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusInProgress, requestCounts, nil); err != nil {
					logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusInProgress)
					// non critical error. continue the shutdown process
				}
				taskToEnqueue := &db.BatchJobPriority{
					ID:  jobDbData.ID,
					SLO: jobDbData.SLO,
				}
				if err := p.clients.priorityQueue.Enqueue(processPipelineCtx, taskToEnqueue); err != nil {
					logger.V(logging.ERROR).Error(err, "Failed to re-enqueue job", "jobID", jobDbData.ID)
					// critial error but we can't do anything about it. continue the shutdown process
				}

				// metrics: skipped as the job is not completed successfully

				// exit the loop safely
				return nil
			case ev := <-eventsChan.Events:
				switch ev.Type {
				case db.BatchEventCancel:
					logger.V(logging.INFO).Info("Received cancellation request. cancelling the processing pipeline")

					// status update to cancelling
					if err := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusCancelling, requestCounts, nil); err != nil {
						logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusCancelling)
						// critical error. but we can't do anything about it. continue the cancel process
					}

					// exit the loop safely
					return fmt.Errorf("job cancelled")
					/*case db.BatchEventPause:
						logger.Info("Job paused by user. Waiting for resume request.")

						// wait for resume request
					PauseLoop:
						for {
							select {
							case <-processPipelineCtx.Done():
								return processPipelineCtx.Err()
							case resEv := <-eventsChan.Events:
								if resEv.Type == db.BatchEventResume {
									break PauseLoop
								}
								if resEv.Type == db.BatchEventCancel {
									logger.V(logging.INFO).Info("Received cancellation request. cancelling the processing pipeline")

									// status update to cancelling
									if err := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusCancelling, requestCounts, nil); err != nil {
										logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusCancelling)
										// critical error. but we can't do anything about it. continue the cancel process
									}
									// exit the loop safely
									return fmt.Errorf("job cancelled")
								}
							}
						}
					*/
				}

			default:
			}
			// read one line at a time
			line := fileStreamScanner.Text()

			// validate the request line's method and request body format
			if err := p.validateLine(line); err != nil {
				logger.V(logging.ERROR).Error(err, "Failed to validate request line")
				atomic.AddInt64(&failedRequests, 1)
				atomic.AddInt64(&totalRequests, 1)
				resultChan <- &batch_utils.Response{
					ID:       "",
					CustomID: "",
					Response: nil,
					Error: &inference.ClientError{
						Category: inference.ErrCategoryInvalidReq,
						Message:  err.Error(),
						RawError: err,
					},
				}
				continue
			}
			// increase request counts
			atomic.AddInt64(&totalRequests, 1)
			// update request counts status
			if err := batch_utils.UpdateRequestCountsStatus(p.clients.status, processPipelineCtx, job.JobID, requestCounts); err != nil {
				logger.V(logging.ERROR).Error(err, "Failed to update request counts status", "requestCounts", requestCounts)
				// non critical error. continue the loop
			}
			// send the request to the inference server
			processPipelineGroup.Go(func() error {
				currentLine := line
				// send the request to the inference server >> result sent to resultchan
				p.doInferenceRequest(processPipelineCtx, currentLine, resultChan, &completedRequests, &failedRequests)
				// update request counts status
				if err := batch_utils.UpdateRequestCountsStatus(p.clients.status, processPipelineCtx, job.JobID, requestCounts); err != nil {
					logger.V(logging.ERROR).Error(err, "Failed to update temp status", "requestCounts", requestCounts)
					// non critical error. continue the loop
				}
				return nil
			})
		}
		return nil
	})

	if err := processPipelineGroup.Wait(); err != nil {
		if err.Error() == "job cancelled" {
			logger.V(logging.INFO).Info("Job processing stopped due to user cancellation.")

			// cancelled jobs are considered as successful jobs (no failure)
			jobResult = metrics.ResultSuccess

			// metrics recording
			metrics.RecordJobProcessed(jobResult, jobFailureReason)
			metrics.RecordJobProcessingDuration(time.Since(startTime), job.TenantID, metrics.GetSizeBucket(int(requestCounts.Total)))

			// status update to cancelled
			batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusCancelled, requestCounts, nil)
			return
		}
		logger.V(logging.ERROR).Error(err, "Failed to execute job processing pipeline")
		jobResult = metrics.ResultFailed
		metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
		// skipped metrics: job processing duration / job error details
		// job processing duration recording (missing tenantID, sizeBucket)
		// job error recording (missing modelID)
		updateErr := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusFailed, nil, nil)
		if updateErr != nil {
			logger.V(logging.ERROR).Error(updateErr, "Failed to update job status to %s in DB. skipping this job.", openai.BatchStatusFailed)
		}
		return
	}

	logger.V(logging.INFO).Info("Job processing completed successfully.")
	jobResult = metrics.ResultSuccess
	metrics.RecordJobProcessed(jobResult, jobFailureReason)
	metrics.RecordJobProcessingDuration(time.Since(startTime), job.TenantID, metrics.GetSizeBucket(int(requestCounts.Total)))
	metrics.RecordJobError(job.BatchJob.BatchStatusInfo.Model)
	if err := batch_utils.UpdateDBJobStatus(p.clients.database, p.clients.status, jobctx, jobDbData, openai.BatchStatusCompleted, requestCounts, nil); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update job status to %s in DB.", openai.BatchStatusCompleted)
	}
}

func (p *Processor) doInferenceRequest(
	ctx context.Context,
	line string,
	resultChan chan *batch_utils.Response,
	completedRequests *int64,
	failedRequests *int64,
) error {
	logger := klog.FromContext(ctx)

	// request parsing
	var req *batch_utils.Request
	if err := json.Unmarshal([]byte(line), &req); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to unmarshal request line")
		resultChan <- &batch_utils.Response{
			ID:       "unknown",
			CustomID: "unknown",
			Response: nil,
			Error: &inference.ClientError{
				Category: inference.ErrCategoryInvalidReq,
				Message:  err.Error(),
				RawError: err,
			},
		}
		atomic.AddInt64(failedRequests, 1)
		return nil
	}

	// send the request to the inference server
	resp, err := p.clients.inference.Generate(ctx, &inference.GenerateRequest{})

	// initialize final response
	finalResp := &batch_utils.Response{
		CustomID: req.CustomID,
	}

	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to send inference request")
		atomic.AddInt64(failedRequests, 1)
		finalResp.Error = err
	} else {
		logger.V(logging.TRACE).Info("Inference request successful", "customID", req.CustomID, "requestID", resp.RequestID)
		atomic.AddInt64(completedRequests, 1)
		finalResp.Response = resp

		// send the response to the result channel
		resultChan <- finalResp
	}

	return nil
}

func (p *Processor) validateLine(line string) error {
	// TODO:: validate the request line's method and request body format
	return nil
}

func (p *Processor) handleError(ctx context.Context, err error) {
	// TODO:: error handling.
	logger := klog.FromContext(ctx)
	logger.V(logging.ERROR).Error(err, "Inference request failed")
}

func (p *Processor) handleResponse(ctx context.Context, response *batch_utils.Response) error {
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

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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	files "github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
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
	database db.BatchDBClient,
	files files.BatchFilesClient,
	pq db.BatchPriorityQueueClient,
	status db.BatchStatusClient,
	event db.BatchEventChannelClient,
	inference inference.Client,
) ProcessorClients {
	return ProcessorClients{
		database:      database,
		files:         files,
		priorityQueue: pq,
		status:        status,
		event:         event,
		inference:     inference,
	}
}

type Processor struct {
	cfg    *config.ProcessorConfig
	tokens chan struct{}
	wg     sync.WaitGroup

	clients *ProcessorClients
}

var ErrCancelled = errors.New("batch job cancelled")

func NewProcessor(
	cfg *config.ProcessorConfig,
	clients *ProcessorClients,
) *Processor {
	sem := make(chan struct{}, cfg.NumWorkers)
	for i := 0; i < cfg.NumWorkers; i++ {
		sem <- struct{}{}
	}
	return &Processor{
		cfg:     cfg,
		tokens:  sem,
		clients: clients,
	}
}

func (p *Processor) acquire(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-p.tokens:
		return true
	}
}

func (p *Processor) release() {
	select {
	case p.tokens <- struct{}{}:
		return
	default:
		panic("token channel is full (double release?)")
	}
}

// TODO: need to add detailed validation here for each client.
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

// pre-flight check
func (p *Processor) prepare(ctx context.Context) error {
	logger := klog.FromContext(ctx)

	if err := p.clients.Validate(); err != nil {
		return fmt.Errorf("critical clients are missing in processor: %w", err)
	}

	logger.V(logging.DEBUG).Info("Processor pre-flight check done", "max_workers", p.cfg.NumWorkers)
	return nil
}

// RunPollingLoop runs the main job polling loop for the processor
// it waits until a worker is available,
// then dequeues one job from the queue,
// assign the job to the available worker
// fetches the job item from the db,
// validates the job checking if it is runnable and not expired,
// then proceed with processing
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

	poller := NewPoller(p.clients.priorityQueue, p.clients.database)
	updater := NewStatusUpdater(p.clients.database, p.clients.status)

	// worker driven non-busy wait
	for {
		if !p.acquire(ctx) {
			return nil
		}

		// check queue for available tasks
		task, err := poller.DequeueOne(ctx)

		// polling error
		if err != nil {
			p.release()
			continue
		}

		// when there's no waiting tasks in the queue
		if task == nil {
			p.release()
			// wait for poll interval to protect db from frequent queueing
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(p.cfg.PollInterval):
				continue
			}
		}

		// create a new logger for the job
		jlogger := klog.FromContext(ctx).WithValues("jobId", task.ID)
		jctx := klog.NewContext(ctx, jlogger)

		// get job item from db
		jobItem, err := poller.FetchJobItem(jctx, task)
		if err != nil {
			p.release()
			// poller re-enqueued the task if the error is due to temporary issue (db connection, etc.)
			metrics.RecordJobProcessed(metrics.ResultReEnqueued, metrics.ReasonDBTransient)
			continue
		}

		// job item is not found in the db.
		if jobItem == nil {
			// poller deleted the task from the queue.
			p.release()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonDBInconsistency)
			continue
		}

		// queue wait metrics recording
		if jobPriorityData, err := batch_utils.GetJobPriorityDataFromQueueItem(task); err == nil {
			queueWait := time.Since(time.Unix(jobPriorityData.CreatedAt, 0))
			metrics.RecordQueueWaitDuration(queueWait, jobItem.TenantID)
		} else {
			// queue createdAt is not available.
			// log the error and continue processing as createdAt is only for metrics recording.
			jlogger.V(logging.ERROR).Error(err, "Failed to get job priority data from queue item")
		}

		// job status validation
		jobInfo, err := batch_utils.FromDBItemToJobInfoObject(jobItem)

		if err != nil {
			jlogger.V(logging.ERROR).Error(err, "Failed to convert job object in DB to job info object")
			p.release()
			metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)
			continue
		}

		if !task.SLO.IsZero() && time.Now().After(task.SLO) {
			jlogger.V(logging.INFO).Info("Job is expired.")

			// persistent status update
			if err := updater.UpdatePersistentStatus(jctx, jobItem, openai.BatchStatusExpired, nil, nil); err != nil {
				jlogger.V(logging.ERROR).Error(err, "Failed to update job status in DB", "newStatus", openai.BatchStatusExpired, "slo", task.SLO)
			}

			// delete the task from the queue.
			if _, err := p.clients.priorityQueue.PQDelete(jctx, task); err != nil {
				jlogger.V(logging.ERROR).Error(err, "Failed to delete the task from the queue", "slo", task.SLO)
			}

			p.release()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonExpired)
			continue
		}

		// job is not in runnable state.
		if !batch_utils.IsJobRunnable(jobInfo.BatchJob) {
			jlogger.V(logging.INFO).Info("job is not in processible state. skipping this job.", "status", jobInfo.BatchJob.BatchStatusInfo.Status)

			// persistent status update is not needed.
			// delete the task from the queue as it is not processible.
			if _, err := p.clients.priorityQueue.PQDelete(jctx, task); err != nil {
				jlogger.V(logging.ERROR).Error(err, "Failed to delete the task from the queue", "slo", task.SLO)
			}

			p.release()
			metrics.RecordJobProcessed(metrics.ResultSkipped, metrics.ReasonNotRunnableState)
			continue
		}

		// process job
		p.wg.Add(1)
		go func(c context.Context, jobItem *db.BatchItem, jobInfo *batch_types.JobInfo, task *db.BatchJobPriority) {
			defer p.wg.Done()
			defer p.release()
			defer func() {
				if r := recover(); r != nil {
					recoverErr := fmt.Errorf("%v", r)
					klog.FromContext(c).Error(recoverErr, "Panic recovered")
				}
			}()

			metrics.IncActiveWorkers()
			defer metrics.DecActiveWorkers()

			// event watcher for cancel event
			eventWatcher, err := p.clients.event.ECConsumerGetChannel(c, jobInfo.JobID)
			if err != nil {
				jlogger.V(logging.ERROR).Error(err, "Failed to get event watcher")
				return
			}
			defer eventWatcher.CloseFn()

			// cancel requested flag and cancelling once
			var cancelRequested atomic.Bool
			var cancellingOnce sync.Once

			// watch for cancel event
			go p.watchCancel(c, eventWatcher, updater, jobItem, &cancelRequested, &cancellingOnce)

			// pre-process job
			if err := p.preProcessJob(c, jobInfo, &cancelRequested); err != nil {
				switch {
				case errors.Is(err, ErrCancelled):
					if cancelErr := p.handleCancelled(c, jobItem, updater, task); cancelErr != nil {
						jlogger.V(logging.ERROR).Error(cancelErr, "Failed to handle cancelled event")
					}
					return

				case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
					// processor shutdown / job context cancelled
					// re-enqueue the job to the queue so this job can be picked up by another worker
					// use background context to avoid context cancellation error
					if task != nil {
						if enqErr := p.clients.priorityQueue.PQEnqueue(context.Background(), task); enqErr != nil {
							jlogger.V(logging.ERROR).Error(enqErr, "Failed to re-enqueue the job to the queue")
						} else {
							jlogger.V(logging.INFO).Info("Re-enqueued the job to the queue")
						}
					}
					return

				default:
					// treat as failed job
					if failUpdateErr := updater.UpdatePersistentStatus(c, jobItem, openai.BatchStatusFailed, nil, nil); failUpdateErr != nil {
						jlogger.V(logging.ERROR).Error(failUpdateErr, "Failed to update status to failed in DB")
					}

					// best-effort delete the job from the queue if it's still there
					if task != nil {
						if nDeleted, delErr := p.clients.priorityQueue.PQDelete(context.Background(), task); delErr != nil {
							jlogger.V(logging.ERROR).Error(delErr, "Failed to delete the job from the queue")
						} else if nDeleted == 0 {
							jlogger.V(logging.INFO).Info("Job is not in the queue anymore")
						}
					}

					metrics.RecordJobProcessed(metrics.ResultFailed, metrics.ReasonSystemError)

					return
				}
			} else {
				// phase 2
				// TODO: process plans and execute requests
			}
		}(jctx, jobItem, jobInfo, task)
	}
}

// preProcessJob performs the pre-processing steps for the job
// it downloads the input file from the files store in job work folder : jobs/<jobid>/input.jsonl,
// creates the plan per model, while saving the input file in the work folder.
// temp plan file is saved in the work folder's subfolder while creating the plan (jobs/<jobid>/plans/<modelid>.plan.tmp)
// then the temp plan file is renamed to the final plan file (jobs/<jobid>/plans/<modelid>.plan)
func (p *Processor) preProcessJob(ctx context.Context, jobInfo *batch_types.JobInfo, cancelRequested *atomic.Bool) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Pre-processing job") // job id is in the logger already
	jobID := jobInfo.JobID
	inputFileId := jobInfo.BatchJob.BatchSpec.InputFileID
	if inputFileId == "" {
		err := fmt.Errorf("input file ID is empty")
		logger.V(logging.ERROR).Error(err, "Input file ID is empty")
		return err
	}

	jobRootDir := p.jobRootDir(jobID)

	// job directory creation
	if err := os.MkdirAll(jobRootDir, 0o700); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to create job root directory", "jobRootDir", jobRootDir)
		return err
	}

	// input file stream open
	reader, metadata, err := p.openInputFileStream(ctx, inputFileId)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to open input file stream", "inputFileId", inputFileId)
		return err
	}
	defer reader.Close()

	if metadata != nil {
		logger.V(logging.INFO).Info("Input file metadata", "metadata", metadata)
	}

	// create local input file
	localInputFilePath := p.jobInputFilePath(jobID)
	if localInputFilePath == "" {
		err := fmt.Errorf("local input file path is empty")
		logger.V(logging.ERROR).Error(err, "Local input file path is empty")
		return err
	}
	localInputFile, err := os.OpenFile(localInputFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to create local input file", "localInputFilePath", localInputFilePath)
		return err
	}
	defer localInputFile.Close()

	// copy input file stream to local input file
	writer := bufio.NewWriterSize(localInputFile, 1024*1024)

	// plan writer creation
	planWriter := newPlanWriter(jobRootDir, p.cfg.MaxOpenFiles)
	defer func() {
		// best-effort
		_ = planWriter.CloseAll()
	}()

	// model intern tables
	used := make(map[string]int)           // to prevent duplicate model IDs
	modelToSafe := make(map[string]string) // to map the model ID to a safe file name

	// streaming loop
	var offset int64 = 0
	var lineCount int64 = 0 // to count the number of lines in the input file for logging
	inputFileReader := bufio.NewReaderSize(reader, 1024*1024)

	for {
		// context cancel (system-level cancel)
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// user cancel
		if cancelRequested.Load() {
			logger.V(logging.INFO).Info("preProcess: cancel requested")
			return ErrCancelled
		}

		// read the line from the input file
		line, lineErr := inputFileReader.ReadBytes('\n')
		if lineErr != nil && lineErr != io.EOF {
			logger.V(logging.ERROR).Error(lineErr, "Failed to read line from input file")
			return lineErr
		}
		if len(line) == 0 && lineErr == io.EOF {
			break
		}

		lineCount++

		// if last line is not terminated with '\n', append '\n' to the line
		if line[len(line)-1] != '\n' {
			line = append(line, '\n')
		}

		// write the line to the input file
		if _, err := writer.Write(line); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to write line to input file", "path", localInputFilePath, "lineCount", lineCount)
			return err
		}

		// parse the line (minimal check for the model id) for plan file writing
		var req planRequestLine
		trimmedLine := bytes.TrimSuffix(line, []byte{'\n'})
		if err := json.Unmarshal(trimmedLine, &req); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to unmarshal request line", "lineCount", lineCount)
			return err
		}

		// get the model id
		modelID := req.Body.Model
		if modelID == "" {
			logger.V(logging.ERROR).Error(fmt.Errorf("model id is empty"), "Model id is empty", "lineCount", lineCount)
			return fmt.Errorf("model id is empty")
		}

		// model id interning for safe file name and for preventing conflicts with other model IDs)
		safeModelID, ok := modelToSafe[modelID]
		if !ok {
			safeModelID = internModelID(modelID, used)
			modelToSafe[modelID] = safeModelID
		}

		// plan entry append
		length := uint32(len(line))
		if err := planWriter.AppendEntry(safeModelID, planEntry{Offset: offset, Length: length}); err != nil {
			logger.V(logging.ERROR).Error(err, "Failed to append plan entry", "modelID", modelID, "safeModelID", safeModelID, "offset", offset, "length", length, "lineCount", lineCount)
			return err
		}
		offset += int64(length)

		if lineErr == io.EOF {
			break
		}
	}

	// flush input.jsonl file
	if err := writer.Flush(); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to flush input file", "path", localInputFilePath)
		return err
	}

	// finalize the plan files
	modelIDs := make([]string, 0, len(modelToSafe))
	for _, safeID := range modelToSafe {
		modelIDs = append(modelIDs, safeID)
	}

	sort.Strings(modelIDs) // to see predictable order of model ids (for debugging)

	if err := planWriter.Finalize(modelIDs); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to finalize plan files")
		return err
	}

	// model map file writing
	safeToModel := make(map[string]string, len(modelToSafe))
	for modelID, safeID := range modelToSafe {
		safeToModel[safeID] = modelID
	}
	modelMapFile := modelMapFile{
		ModelToSafe: modelToSafe,
		SafeToModel: safeToModel,
		LineCount:   lineCount,
	}
	if err := writeModelMapFile(jobRootDir, modelMapFile); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to write model map file")
		return err
	}

	// log info
	logger.V(logging.INFO).Info("Processor Pre-processing job completed", "inputFilePath", localInputFilePath, "planFilePath", planWriter.plansDir(), "lineCount", lineCount)

	return nil
}

func (p *Processor) watchCancel(
	ctx context.Context,
	eventWatcher *db.BatchEventsChan,
	updater *StatusUpdater,
	jobItem *db.BatchItem,
	cancelRequested *atomic.Bool,
	cancellingOnce *sync.Once,
) {
	logger := klog.FromContext(ctx)
	for {
		select {
		case <-ctx.Done():
			logger.V(logging.DEBUG).Info("watchCancel: context done")
			return

		case event, ok := <-eventWatcher.Events:
			if !ok {
				logger.V(logging.DEBUG).Info("watchCancel: event channel closed")
				return
			}

			if event.Type == db.BatchEventCancel {
				logger.V(logging.INFO).Info("watchCancel: cancel event received")

				// signal
				cancelRequested.Store(true)

				// update status to cancelling
				cancellingOnce.Do(func() {
					err := updater.UpdatePersistentStatus(
						ctx,
						jobItem,
						openai.BatchStatusCancelling,
						nil,
						nil,
					)
					if err != nil {
						logger.V(logging.ERROR).Error(err, "Failed to update status to cancelling in DB")
					}
				})
			}
		}
	}
}

func (p *Processor) handleCancelled(
	ctx context.Context,
	jobItem *db.BatchItem,
	updater *StatusUpdater,
	task *db.BatchJobPriority,
) error {
	logger := klog.FromContext(ctx)

	// 1: cleanup local artifacts (best-effort)
	jobDir := p.jobRootDir(jobItem.ID)
	if jobDir != "" {
		if err := os.RemoveAll(jobDir); err != nil {
			// keep going: status update is more important than cleanup.
			logger.V(logging.ERROR).Error(err, "Failed to remove job directory", "path", jobDir)
		} else {
			logger.V(logging.INFO).Info("Removed job directory", "path", jobDir)
		}
	}

	// 2: update persistent status -> cancelled
	if err := updater.UpdatePersistentStatus(ctx, jobItem, openai.BatchStatusCancelled, nil, nil); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to update status to cancelled")
		return err
	}

	// 3: Delete item from PQ
	if task != nil {
		if _, err := p.clients.priorityQueue.PQDelete(ctx, task); err != nil {
			// best effort: don't fail the whole cancel handling because of PQ delete.
			// if failed and this item remains in the queue, it's deleted when picked up
			logger.V(logging.ERROR).Error(err, "Failed to delete cancelled job from Priority Queue")
		} else {
			logger.V(logging.INFO).Info("Deleted cancelled job from priority queue")
		}
	}
	logger.V(logging.INFO).Info("Job cancelled handled")
	return nil
}

// Stop gracefully stops the processor, waiting for all workers to finish.
func (p *Processor) Stop(ctx context.Context) {
	logger := klog.Background()
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-ctx.Done():
		logger.V(logging.INFO).Info("All workers have finished")

	case <-done:
		logger.V(logging.INFO).Info("Stop timed out or cancelled; giving up waiting for workers")
	}
}

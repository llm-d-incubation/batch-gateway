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

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
)

// uploadFileAndStoreFileRecord uploads a job output or error file to shared storage and creates a file
// record in the database. Returns the assigned file ID, or an empty string if the file is empty.
// fileType distinguishes output files from error files for upload, metrics, and tracing.
func (p *Processor) uploadFileAndStoreFileRecord(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	dbJob *db.BatchItem,
	fileType metrics.FileType,
) (string, error) {
	var fileName string
	var fileSize int64
	var err error
	var attrKey string

	fileID := ucom.NewFileID()

	if fileType == metrics.FileTypeOutput {
		fileName = jobOutputStorageName(jobInfo.JobID)
		storageName := ucom.FileStorageName(fileID, fileName)
		fileSize, err = p.uploadOutputFile(ctx, jobInfo, storageName)
		attrKey = uotel.AttrOutputFileID
	} else {
		fileName = jobErrorStorageName(jobInfo.JobID)
		storageName := ucom.FileStorageName(fileID, fileName)
		fileSize, err = p.uploadErrorFile(ctx, jobInfo, storageName)
		attrKey = uotel.AttrErrorFileID
	}
	if err != nil {
		return "", err
	}
	if fileSize == 0 {
		return "", nil
	}

	uotel.SetAttr(ctx, attribute.String(attrKey, fileID))
	if err := p.storeFileRecord(ctx, fileID, fileName, jobInfo.TenantID, fileSize, dbJob.Tags); err != nil {
		return "", err
	}
	return fileID, nil
}

// finalizeJob performs finalization: uploads output and error files to shared storage,
// creates file records in the database, and updates job status to completed.
func (p *Processor) finalizeJob(
	ctx context.Context,
	updater *StatusUpdater,
	dbJob *db.BatchItem,
	jobInfo *batch_types.JobInfo,
	requestCounts *openai.BatchRequestCounts,
	cancelRequested *atomic.Bool,
) error {
	logger := klog.FromContext(ctx)
	logger.V(logging.INFO).Info("Starting finalization: finalizing job")

	// in_progress → finalizing
	// Written before file uploads so the API server can reject cancel requests once
	// finalization has begun, narrowing the cancel-vs-complete race window.
	if err := updater.UpdatePersistentStatus(ctx, dbJob, openai.BatchStatusFinalizing, requestCounts, nil); err != nil {
		return fmt.Errorf("failed to update job status to finalizing: %w", err)
	}

	// Per the OpenAI batch spec, output_file_id and error_file_id are both optional:
	// output_file_id is omitted when all requests failed; error_file_id is omitted when no
	// requests failed. We skip uploading and recording empty files accordingly.
	outputFileID, err := p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeOutput)
	if err != nil {
		return err
	}

	errorFileID, err := p.uploadFileAndStoreFileRecord(ctx, jobInfo, dbJob, metrics.FileTypeError)
	if err != nil {
		return err
	}

	// Best-effort cancel check: if a cancel event arrived during file uploads,
	// finalize as cancelled instead of completed. This covers the narrow window
	// between executeJob's last cancelRequested check and this point.
	// A residual TOCTOU race remains (cancel arriving between this check and the
	// completed write below), but at this stage all requests have already completed
	// and output files are uploaded — the user receives the same results either way.
	if cancelRequested != nil && cancelRequested.Load() {
		logger.V(logging.INFO).Info("Cancel requested during finalization; finalizing as cancelled")
		if err := updater.UpdateCancelledStatus(ctx, dbJob, requestCounts, outputFileID, errorFileID); err != nil {
			return fmt.Errorf("failed to update job status to cancelled: %w", err)
		}
		setRequestCountAttrs(ctx, requestCounts)
		return nil
	}

	// finalizing → completed
	if err := updater.UpdateCompletedStatus(ctx, dbJob, requestCounts, outputFileID, errorFileID); err != nil {
		return fmt.Errorf("failed to update job status to completed: %w", err)
	}

	setRequestCountAttrs(ctx, requestCounts)

	logger.V(logging.INFO).Info("Finalization completed", "outputFileID", outputFileID, "errorFileID", errorFileID)
	return nil
}

// uploadOutputFile uploads the local output file to shared storage with retry.
// Returns the file size; returns 0 without error if the file is empty (all requests failed).
// Per the OpenAI batch spec, output_file_id is optional and may be omitted when there are no
// successful requests.
func (p *Processor) uploadOutputFile(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	fileName string,
) (int64, error) {
	filePath, err := p.jobOutputFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return 0, err
	}
	return p.uploadJobFile(ctx, filePath, fileName, jobInfo.TenantID, metrics.FileTypeOutput)
}

// uploadErrorFile uploads the local error file to shared storage with retry.
// Returns the file size; returns 0 without error if the file is empty (no errors occurred).
// Per the OpenAI batch spec, error_file_id is optional and may be omitted when no requests failed.
func (p *Processor) uploadErrorFile(
	ctx context.Context,
	jobInfo *batch_types.JobInfo,
	fileName string,
) (int64, error) {
	filePath, err := p.jobErrorFilePath(jobInfo.JobID, jobInfo.TenantID)
	if err != nil {
		return 0, err
	}
	return p.uploadJobFile(ctx, filePath, fileName, jobInfo.TenantID, metrics.FileTypeError)
}

// uploadJobFile uploads a local file to shared storage with retry.
// Returns the file size; returns 0 without error if the file does not exist or is empty.
func (p *Processor) uploadJobFile(
	ctx context.Context,
	filePath, fileName, tenantID string,
	fileType metrics.FileType,
) (int64, error) {
	logger := klog.FromContext(ctx)

	stat, err := os.Stat(filePath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("failed to stat file %s: %w", filePath, err)
	}
	if stat.Size() == 0 {
		return 0, nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return 0, fmt.Errorf("failed to open file %s for upload: %w", filePath, err)
	}
	defer f.Close()

	folderName, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		return 0, fmt.Errorf("failed to get folder name: %w", err)
	}

	retryCfg := p.cfg.UploadRetry
	maxAttempts := retryCfg.MaxRetries + 1

	// TODO: distinguish retryable (network/storage transient) vs non-retryable (auth, permission)
	// errors and skip retries for the latter. Deferred until we have more storage backends
	// or see real non-transient failures in production.
	fileMeta, err := p.files.storage.Store(ctx, fileName, folderName, 0, 0, f)
	for attempt := 1; err != nil && attempt < maxAttempts; attempt++ {
		metrics.RecordFileUploadRetry(fileType)
		backoff := min(retryCfg.InitialBackoff*(1<<(attempt-1)), retryCfg.MaxBackoff)
		logger.V(logging.WARNING).Info("Retrying file upload",
			"file", fileName, "attempt", attempt+1, "maxAttempts", maxAttempts, "backoff", backoff, "error", err)

		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("upload retry cancelled: %w", ctx.Err())
		case <-time.After(backoff):
		}

		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return 0, fmt.Errorf("failed to seek file %s for retry: %w", fileName, seekErr)
		}
		fileMeta, err = p.files.storage.Store(ctx, fileName, folderName, 0, 0, f)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to upload file %s after %d attempts: %w", fileName, maxAttempts, err)
	}

	return fileMeta.Size, nil
}

// storeFileRecord creates a file metadata record in the database.
// Used for both output and error files.
// If the batch has a user-provided output_expires_after_seconds tag, it takes
// precedence over the config default (DefaultOutputExpirationSeconds).
func (p *Processor) storeFileRecord(
	ctx context.Context,
	fileID, fileName, tenantID string,
	size int64,
	batchTags db.Tags,
) error {
	now := time.Now().Unix()

	expiresAt := p.resolveOutputExpiration(now, batchTags)

	fileObj := &openai.FileObject{
		ID:        fileID,
		Bytes:     size,
		CreatedAt: now,
		ExpiresAt: expiresAt,
		Filename:  fileName,
		Object:    "file",
		Purpose:   openai.FileObjectPurposeBatchOutput,
		Status:    openai.FileObjectStatusProcessed,
	}
	fileItem, err := converter.FileToDBItem(fileObj, tenantID, db.Tags{})
	if err != nil {
		return fmt.Errorf("failed to convert file to db item: %w", err)
	}

	if err := p.files.db.DBStore(ctx, fileItem); err != nil {
		return fmt.Errorf("failed to store file record: %w", err)
	}
	return nil
}

// resolveOutputExpiration returns the ExpiresAt timestamp for an output or error file.
// Priority: user-provided output_expires_after_seconds tag > config default.
// Returns 0 (no expiration) if neither is set.
func (p *Processor) resolveOutputExpiration(now int64, batchTags db.Tags) int64 {
	if s, ok := batchTags[batch_types.TagOutputExpiresAfterSeconds]; ok {
		if ttl, err := strconv.ParseInt(s, 10, 64); err == nil && ttl > 0 {
			return now + ttl
		}
	}
	if ttl := p.cfg.DefaultOutputExpirationSeconds; ttl > 0 {
		return now + ttl
	}
	return 0
}

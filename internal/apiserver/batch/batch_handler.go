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

// The file provides HTTP handlers for batch-related API endpoints.
// It implements the OpenAI compatible Batch API endpoints for creating, listing, retrieving, and canceling batches.
package batch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

const (
	pathParamBatchID = "batch_id"
	pathParamLimit   = "limit"
	pathParamAfter   = "after"
)

func jobToBatch(job *api.BatchItem) (*openai.Batch, error) {
	batch := &openai.Batch{
		ID: job.ID,
	}

	if err := json.Unmarshal(job.Spec, &batch.BatchSpec); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch spec: %w", err)
	}

	if err := json.Unmarshal(job.Status, &batch.BatchStatusInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch status: %w", err)
	}

	return batch, nil
}

type BatchApiHandler struct {
	config       *common.ServerConfig
	dbClient     api.BatchDBClient
	queueClient  api.BatchPriorityQueueClient
	eventClient  api.BatchEventChannelClient
	statusClient api.BatchStatusClient
}

func NewBatchApiHandler(config *common.ServerConfig, dbClient api.BatchDBClient, queueClient api.BatchPriorityQueueClient, eventClient api.BatchEventChannelClient, statusClient api.BatchStatusClient) *BatchApiHandler {
	return &BatchApiHandler{
		config:       config,
		dbClient:     dbClient,
		queueClient:  queueClient,
		eventClient:  eventClient,
		statusClient: statusClient,
	}
}

func (c *BatchApiHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodPost,
			Pattern:     "/v1/batches",
			HandlerFunc: c.CreateBatch,
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/batches",
			HandlerFunc: c.ListBatches,
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/batches/{batch_id}",
			HandlerFunc: c.RetrieveBatch,
		},
		{
			Method:      http.MethodPost,
			Pattern:     "/v1/batches/{batch_id}/cancel",
			HandlerFunc: c.CancelBatch,
		},
	}
}

func (c *BatchApiHandler) CreateBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	createdAt := time.Now().UTC().Unix()

	// parse request
	batchReq := &openai.CreateBatchRequest{}
	if err := common.DecodeJSON(r.Body, batchReq); err != nil {
		logger.Error(err, "failed to decode request")
		apiErr := openai.NewAPIError(http.StatusBadRequest, "", err.Error(), nil)
		common.WriteAPIError(w, r, apiErr)
		return
	}

	// validate request
	if err := batchReq.Validate(); err != nil {
		logger.Error(err, "failed to validate request")
		apiErr := openai.NewAPIError(http.StatusBadRequest, "", err.Error(), nil)
		common.WriteAPIError(w, r, apiErr)
		return
	}

	batchID := fmt.Sprintf("batch_%s", uuid.NewString())

	// construct batch spec
	batchSpec := openai.BatchSpec{
		Object:           "batch",
		Endpoint:         batchReq.Endpoint,
		InputFileID:      batchReq.InputFileID,
		CompletionWindow: batchReq.CompletionWindow,
		Metadata:         batchReq.Metadata,
		CreatedAt:        createdAt,
	}
	batchSpecData, err := json.Marshal(batchSpec)
	if err != nil {
		logger.Error(err, "failed to marshal batch spec")
		common.WriteInternalServerError(w, r)
		return
	}

	// construct batch status
	batchStatus := openai.BatchStatusInfo{
		Status: openai.BatchStatusValidating,
	}
	batchStatusData, err := json.Marshal(batchStatus)
	if err != nil {
		logger.Error(err, "failed to marshal batch status")
		common.WriteInternalServerError(w, r)
		return
	}

	// store batch job
	completionDuration, err := time.ParseDuration(batchReq.CompletionWindow)
	if err != nil {
		logger.Error(err, "failed to parse completion window duration")
		common.WriteInternalServerError(w, r)
		return
	}
	slo := time.Now().UTC().Add(completionDuration)

	// ttl := c.config.BatchTTLSeconds
	// if batchReq.OutputExpiresAfter != nil {
	// 	if batchReq.OutputExpiresAfter.Anchor == "" || batchReq.OutputExpiresAfter.Anchor == "created_at" {
	// 		// TODO: get the batch file create time, and set TTL to batch file create time + OutputExpiresAfter.Seconds
	// 		ttl = int(completionDuration.Seconds()) + int(batchReq.OutputExpiresAfter.Seconds)
	// 	}
	// }

	// Get tenant ID from context
	tenantID := common.GetTenantIDFromContext(ctx)

	// TODO: add field Expiry
	job := &api.BatchItem{
		ID:       batchID,
		TenantID: tenantID,
		// SLO:    slo,
		// TTL:    ttl,
		Tags:   nil,
		Spec:   batchSpecData,
		Status: batchStatusData,
	}

	_, err = c.dbClient.DBStore(ctx, job)
	if err != nil {
		logger.Error(err, "failed to store batch job")
		common.WriteInternalServerError(w, r)
		return
	}

	// enqueue job
	bjpData := &batch_utils.BatchJobPriorityData{
		CreatedAt: createdAt,
	}
	bjpDataBytes, err := json.Marshal(bjpData)
	if err != nil {
		logger.Error(err, "failed to marshal batch job priority data")
		if _, delErr := c.dbClient.DBDelete(ctx, []string{batchID}); delErr != nil {
			logger.Error(delErr, "failed to cleanup batch job after marshal failure", "batch_id", batchID)
		}
		common.WriteInternalServerError(w, r)
		return
	}
	bjp := &api.BatchJobPriority{
		ID:   batchID,
		SLO:  slo,
		Data: bjpDataBytes,
	}
	if err := c.queueClient.PQEnqueue(ctx, bjp); err != nil {
		logger.Error(err, "failed to enqueue batch job priority")
		if _, delErr := c.dbClient.DBDelete(ctx, []string{batchID}); delErr != nil {
			logger.Error(delErr, "failed to cleanup batch job after enqueue failure", "batch_id", batchID)
		}
		common.WriteInternalServerError(w, r)
		return
	}

	// construct create response
	batch := openai.Batch{
		ID:              batchID,
		BatchSpec:       batchSpec,
		BatchStatusInfo: batchStatus,
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

func (c *BatchApiHandler) ListBatches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	// Parse query parameters
	query := r.URL.Query()
	limit := 20
	if limitStr := query.Get(pathParamLimit); limitStr != "" {
		var parsedLimit int
		if _, err := fmt.Sscanf(limitStr, "%d", &parsedLimit); err != nil {
			apiErr := openai.NewAPIError(http.StatusBadRequest, "", "invalid limit parameter: must be an integer", nil)
			common.WriteAPIError(w, r, apiErr)
			return
		}

		if parsedLimit < 1 || parsedLimit > 100 {
			apiErr := openai.NewAPIError(http.StatusBadRequest, "", "invalid limit parameter: must be between 1 and 100", nil)
			common.WriteAPIError(w, r, apiErr)
			return
		}
		limit = parsedLimit
	}

	after := 0
	if afterStr := query.Get(pathParamAfter); afterStr != "" {
		var parsedAfter int
		if _, err := fmt.Sscanf(afterStr, "%d", &parsedAfter); err != nil {
			apiErr := openai.NewAPIError(http.StatusBadRequest, "", "invalid after parameter: must be an integer", nil)
			common.WriteAPIError(w, r, apiErr)
			return
		}

		if parsedAfter < 0 {
			apiErr := openai.NewAPIError(http.StatusBadRequest, "", "invalid after parameter: must be equal to or greater than 0", nil)
			common.WriteAPIError(w, r, apiErr)
			return
		}
		after = parsedAfter
	}

	// Get tenant ID from context
	tenantID := common.GetTenantIDFromContext(ctx)

	// Request limit+1 to check if there are more results
	items, _, hasMore, err := c.dbClient.DBGet(ctx,
		&api.BatchDBQuery{
			TenantID: tenantID,
		},
		true, after, limit)
	if err != nil {
		logger.Error(err, "failed to list batches from database")
		common.WriteInternalServerError(w, r)
		return
	}

	// Convert jobs to batch responses
	batches := make([]openai.Batch, 0, len(items))
	for _, item := range items {
		batch, err := jobToBatch(item)
		if err != nil {
			logger.Error(err, "failed to convert job to batch", "batch_id", item.ID)
			continue
		}

		batches = append(batches, *batch)
	}

	resp := openai.ListBatchResponse{
		Object:  "list",
		Data:    batches,
		HasMore: hasMore,
	}
	if len(batches) > 0 {
		resp.FirstID = batches[0].ID
		resp.LastID = batches[len(batches)-1].ID
	}

	common.WriteJSONResponse(w, r, http.StatusOK, resp)
}

// getBatchItemFromDB retrieves and validates a batch item by ID.
func (c *BatchApiHandler) getBatchItemFromDB(w http.ResponseWriter, r *http.Request, operation string) (*api.BatchItem, *openai.APIError) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	batchID := r.PathValue(pathParamBatchID)
	if batchID == "" {
		apiErr := openai.NewAPIError(
			http.StatusBadRequest,
			"",
			pathParamBatchID+" is required",
			nil,
		)
		return nil, &apiErr
	}

	logger.V(logging.DEBUG).Info(operation + " batch request")

	// Get tenant ID from context
	tenantID := common.GetTenantIDFromContext(ctx)

	// Get batch from database
	items, _, _, err := c.dbClient.DBGet(ctx,
		&api.BatchDBQuery{
			IDs:      []string{batchID},
			TenantID: tenantID,
		},
		true, 0, 1)
	if err != nil {
		logger.Error(err, "failed to get batch from database")
		apiErr := openai.NewAPIError(http.StatusInternalServerError, "", "Internal Server Error", nil)
		return nil, &apiErr
	}

	if len(items) == 0 {
		logger.Info("batch not found")
		apiErr := openai.NewAPIError(
			http.StatusNotFound,
			"",
			fmt.Sprintf("Batch with ID %s not found", batchID),
			nil,
		)
		return nil, &apiErr
	}

	item := items[0]

	// Validate tenant isolation if tenant ID is present in request
	if item.TenantID != tenantID {
		logger.Info("batch not found - tenant mismatch", "request_tenant", tenantID, "batch_tenant", item.TenantID)
		apiErr := openai.NewAPIError(
			http.StatusNotFound,
			"",
			fmt.Sprintf("Batch with ID %s not found", item.ID),
			nil,
		)
		return nil, &apiErr
	}

	return item, nil
}

func (c *BatchApiHandler) RetrieveBatch(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromRequest(r)

	item, apiErr := c.getBatchItemFromDB(w, r, "retrieve")
	if apiErr != nil {
		common.WriteAPIError(w, r, *apiErr)
		return
	}

	batch, err := jobToBatch(item)
	if err != nil {
		logger.Error(err, "failed to convert job to batch")
		common.WriteInternalServerError(w, r)
		return
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

func (c *BatchApiHandler) CancelBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	item, apiErr := c.getBatchItemFromDB(w, r, "cancel")
	if apiErr != nil {
		common.WriteAPIError(w, r, *apiErr)
		return
	}

	batch, err := jobToBatch(item)
	if err != nil {
		logger.Error(err, "failed to convert job to batch")
		common.WriteInternalServerError(w, r)
		return
	}

	// Check if batch can be cancelled
	if batch.Status.IsFinal() {
		apiErr := openai.NewAPIError(http.StatusBadRequest, "", fmt.Sprintf("Batch with status %s cannot be cancelled", batch.Status), nil)
		common.WriteAPIError(w, r, apiErr)
		return
	}

	// Try to remove from the priority queue first
	jobPriority := &api.BatchJobPriority{
		ID: item.ID,
	}
	removed, err := c.queueClient.PQDelete(ctx, jobPriority)
	if err != nil {
		logger.Error(err, "failed to remove batch from queue")
		common.WriteInternalServerError(w, r)
		return
	}

	if removed > 0 {
		// Job was in queue (not yet being processed) - directly cancel it
		batch.Status = openai.BatchStatusCancelled
		cancelledAt := time.Now().UTC().Unix()
		batch.CancelledAt = &cancelledAt
	} else {
		// Job is being processed - mark as cancelling and send cancel event
		batch.Status = openai.BatchStatusCancelling
		cancellingAt := time.Now().UTC().Unix()
		batch.CancellingAt = &cancellingAt

		event := []api.BatchEvent{
			{
				ID:   item.ID,
				Type: api.BatchEventCancel,
				TTL:  c.config.GetBatchTTLSeconds(),
			},
		}
		_, err = c.eventClient.ECProducerSendEvents(ctx, event)
		if err != nil {
			logger.Error(err, "failed to send cancel event")
			common.WriteInternalServerError(w, r)
			return
		}
	}

	// Update batch status in database
	updatedStatusData, err := json.Marshal(batch.BatchStatusInfo)
	if err != nil {
		logger.Error(err, "failed to marshal updated status")
		common.WriteInternalServerError(w, r)
		return
	}
	item.Status = updatedStatusData
	if err := c.dbClient.DBUpdate(ctx, item); err != nil {
		logger.Error(err, "failed to update batch in database")
		common.WriteInternalServerError(w, r)
		return
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

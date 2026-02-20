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
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

const (
	pathParamBatchID = "batch_id"
	pathParamLimit   = "limit"
	pathParamAfter   = "after"
)

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

	// store batch job
	completionDuration, err := time.ParseDuration(batchReq.CompletionWindow)
	if err != nil {
		logger.Error(err, "failed to parse completion window duration")
		common.WriteInternalServerError(w, r)
		return
	}
	slo := time.Now().UTC().Add(completionDuration)

	// Get tenant ID from context
	tenantID := common.GetTenantIDFromContext(ctx)

	// Create openai.Batch object
	batch := &openai.Batch{
		ID: batchID,
		BatchSpec: openai.BatchSpec{
			Object:           "batch",
			Endpoint:         batchReq.Endpoint,
			InputFileID:      batchReq.InputFileID,
			CompletionWindow: batchReq.CompletionWindow,
			Metadata:         batchReq.Metadata,
			CreatedAt:        createdAt,
		},
		BatchStatusInfo: openai.BatchStatusInfo{
			Status: openai.BatchStatusValidating,
		},
	}

	// Convert to database item
	dbItem, err := converter.BatchToDBItem(batch, tenantID, api.Tags{})
	if err != nil {
		logger.Error(err, "failed to convert batch to database item")
		common.WriteInternalServerError(w, r)
		return
	}

	if err := c.dbClient.DBStore(ctx, dbItem); err != nil {
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

	// Request items
	items, _, expectMore, err := c.dbClient.DBGet(ctx,
		&api.BatchQuery{
			BaseQuery: api.BaseQuery{TenantID: tenantID},
		},
		true, after, limit)
	if err != nil {
		logger.Error(err, "failed to list batches from database")
		common.WriteInternalServerError(w, r)
		return
	}

	// Convert to batch responses
	batches := make([]openai.Batch, 0, len(items))
	for _, item := range items {
		batch, err := converter.DBItemToBatch(item)
		if err != nil {
			logger.Error(err, "failed to convert database item to batch")
			common.WriteInternalServerError(w, r)
			return
		}
		batches = append(batches, *batch)
	}

	resp := openai.ListBatchResponse{
		Object:  "list",
		Data:    batches,
		HasMore: expectMore,
	}
	if len(batches) > 0 {
		resp.FirstID = batches[0].ID
		resp.LastID = batches[len(batches)-1].ID
	}

	common.WriteJSONResponse(w, r, http.StatusOK, resp)
}

func (c *BatchApiHandler) getBatchItemFromDB(r *http.Request, operation string) (*api.BatchItem, *openai.APIError) {
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

	tenantID := common.GetTenantIDFromContext(ctx)

	items, _, _, err := c.dbClient.DBGet(ctx,
		&api.BatchQuery{
			BaseQuery: api.BaseQuery{
				IDs:      []string{batchID},
				TenantID: tenantID,
			},
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

	if item.TenantID != tenantID {
		logger.Info("batch not found - tenant mismatch", "request_tenant", tenantID, "batch_tenant", item.TenantID)
		apiErr := openai.NewAPIError(
			http.StatusNotFound,
			"",
			fmt.Sprintf("Batch with ID %s not found", batchID),
			nil,
		)
		return nil, &apiErr
	}

	return item, nil
}

func (c *BatchApiHandler) RetrieveBatch(w http.ResponseWriter, r *http.Request) {
	logger := logging.FromRequest(r)

	item, apiErr := c.getBatchItemFromDB(r, "retrieve")
	if apiErr != nil {
		common.WriteAPIError(w, r, *apiErr)
		return
	}

	batch, err := converter.DBItemToBatch(item)
	if err != nil {
		logger.Error(err, "failed to convert database item to batch")
		common.WriteInternalServerError(w, r)
		return
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

func (c *BatchApiHandler) CancelBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	item, apiErr := c.getBatchItemFromDB(r, "cancel")
	if apiErr != nil {
		common.WriteAPIError(w, r, *apiErr)
		return
	}

	batch, err := converter.DBItemToBatch(item)
	if err != nil {
		logger.Error(err, "failed to convert database item to batch")
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
		ID: batch.ID,
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
				ID:   batch.ID,
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

	tenantID := common.GetTenantIDFromContext(ctx)

	dbItem, err := converter.BatchToDBItem(batch, tenantID, item.Tags)
	if err != nil {
		logger.Error(err, "failed to convert batch to database item")
		common.WriteInternalServerError(w, r)
		return
	}

	if err := c.dbClient.DBUpdate(ctx, dbItem); err != nil {
		logger.Error(err, "failed to update batch in database")
		common.WriteInternalServerError(w, r)
		return
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

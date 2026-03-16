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
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
)

// Compile-time check: BatchAPIHandler implements common.ApiHandler.
var _ common.ApiHandler = (*BatchAPIHandler)(nil)

type BatchAPIHandler struct {
	config  *common.ServerConfig
	clients *clientset.Clientset
}

func NewBatchAPIHandler(config *common.ServerConfig, clients *clientset.Clientset) *BatchAPIHandler {
	return &BatchAPIHandler{
		config:  config,
		clients: clients,
	}
}

func (c *BatchAPIHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodPost,
			Pattern:     "/v1/batches",
			HandlerFunc: c.CreateBatch,
			SpanName:    "api-create-batch",
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/batches",
			HandlerFunc: c.ListBatches,
			SpanName:    "api-list-batch",
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/batches/{batch_id}",
			HandlerFunc: c.RetrieveBatch,
			SpanName:    "api-get-batch",
		},
		{
			Method:      http.MethodPost,
			Pattern:     "/v1/batches/{batch_id}/cancel",
			HandlerFunc: c.CancelBatch,
			SpanName:    "api-cancel-batch",
		},
	}
}

func (c *BatchAPIHandler) CreateBatch(w http.ResponseWriter, r *http.Request) {
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

	// Get tenant ID from context
	tenantID := common.GetTenantIDFromContext(ctx)

	// Verify input file exists
	fileQuery := &api.FileQuery{
		BaseQuery: api.BaseQuery{
			IDs:      []string{batchReq.InputFileID},
			TenantID: tenantID,
		},
	}
	fileItems, _, _, err := c.clients.FileDB.DBGet(ctx, fileQuery, true, 0, 1)
	if err != nil {
		logger.Error(err, "failed to query input file", "file_id", batchReq.InputFileID)
		common.WriteInternalServerError(w, r)
		return
	}
	if len(fileItems) == 0 {
		logger.Info("input file not found", "file_id", batchReq.InputFileID)
		apiErr := openai.NewAPIError(
			http.StatusBadRequest,
			"invalid_request_error",
			fmt.Sprintf("Input file with ID '%s' not found", batchReq.InputFileID),
			nil,
		)
		common.WriteAPIError(w, r, apiErr)
		return
	}

	batchID := ucom.NewBatchID()

	// add attributes to span
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String(uotel.AttrInputFileID, batchReq.InputFileID),
		attribute.String(uotel.AttrBatchID, batchID),
	)

	// store batch job
	completionDuration, err := time.ParseDuration(batchReq.CompletionWindow)
	if err != nil {
		logger.Error(err, "failed to parse completion window duration")
		common.WriteInternalServerError(w, r)
		return
	}
	slo := time.Now().UTC().Add(completionDuration)

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

	// TODO: output_expires_after_anchor and output_expires_after_seconds are saved to database as tag. The cleanup service should delete the output file by this value
	// Note that the output_expires_after_anchor is the file creation time, not the time the batch is created.
	tags := api.Tags{
		batch_types.TagSLO: fmt.Sprintf("%d", slo.UnixMicro()),
	}
	if batchReq.OutputExpiresAfter != nil {
		tags[batch_types.TagOutputExpiresAfterAnchor] = batchReq.OutputExpiresAfter.Anchor
		tags[batch_types.TagOutputExpiresAfterSeconds] = fmt.Sprintf("%d", batchReq.OutputExpiresAfter.Seconds)
		logger.V(logging.DEBUG).Info("output expiration configured",
			"anchor", batchReq.OutputExpiresAfter.Anchor,
			"seconds", batchReq.OutputExpiresAfter.Seconds,
		)
	}

	// Capture configured pass-through headers into tags with "pth:" prefix
	for _, headerName := range c.config.BatchAPI.PassThroughHeaders {
		// The external auth service (via Envoy ext_authz) may append
		// request headers as separate entries instead of overwriting them. If a client
		// sends a spoofed pass-through header, the auth service appends the real value as a
		// second entry. We take the last entry from r.Header.Values() because Envoy's
		// ext_authz pipeline guarantees auth-injected entries come after client-supplied
		// ones.
		if values := r.Header.Values(headerName); len(values) > 0 {
			// Skip empty last values to avoid persisting blank tags (e.g. when the
			// auth service clears a spoofed header by appending an empty entry).
			if last := values[len(values)-1]; last != "" {
				tags[batch_types.TagPrefixPassThroughHeader+headerName] = last
			}
		}
	}

	// Inject OTel trace context into tags with "otel:" prefix
	propagator := otel.GetTextMapPropagator()
	carrier := propagation.MapCarrier{}
	propagator.Inject(ctx, carrier)
	for k, v := range carrier {
		tags[batch_types.TagPrefixOTel+k] = v
	}

	// Convert to database item
	dbItem, err := converter.BatchToDBItem(batch, tenantID, tags)
	if err != nil {
		logger.Error(err, "failed to convert batch to database item")
		common.WriteInternalServerError(w, r)
		return
	}

	if err := c.clients.BatchDB.DBStore(ctx, dbItem); err != nil {
		logger.Error(err, "failed to store batch job")
		common.WriteInternalServerError(w, r)
		return
	}

	// enqueue job
	bjpData := &batch_types.BatchJobPriorityData{
		CreatedAt: createdAt,
	}
	bjpDataBytes, err := json.Marshal(bjpData)
	if err != nil {
		logger.Error(err, "failed to marshal batch job priority data")
		if _, delErr := c.clients.BatchDB.DBDelete(ctx, []string{batchID}); delErr != nil {
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
	if err := c.clients.Queue.PQEnqueue(ctx, bjp); err != nil {
		logger.Error(err, "failed to enqueue batch job priority")
		if _, delErr := c.clients.BatchDB.DBDelete(ctx, []string{batchID}); delErr != nil {
			logger.Error(delErr, "failed to cleanup batch job after enqueue failure", "batch_id", batchID)
		}
		common.WriteInternalServerError(w, r)
		return
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

func (c *BatchAPIHandler) ListBatches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	// Parse query parameters
	query := r.URL.Query()
	limit := 20
	if limitStr := query.Get(common.QueryParamLimit); limitStr != "" {
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
	if afterStr := query.Get(common.QueryParamAfter); afterStr != "" {
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
	items, _, expectMore, err := c.clients.BatchDB.DBGet(ctx,
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

func (c *BatchAPIHandler) getBatchItemFromDB(r *http.Request, operation string) (*api.BatchItem, *openai.APIError) {
	ctx := r.Context()
	logger := logging.FromRequest(r)

	batchID := r.PathValue(common.PathParamBatchID)
	if batchID == "" {
		apiErr := openai.NewAPIError(
			http.StatusBadRequest,
			"",
			common.PathParamBatchID+" is required",
			nil,
		)
		return nil, &apiErr
	}

	logger.V(logging.DEBUG).Info(operation + " batch request")

	tenantID := common.GetTenantIDFromContext(ctx)

	items, _, _, err := c.clients.BatchDB.DBGet(ctx,
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

func (c *BatchAPIHandler) RetrieveBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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

	spanAttrs := []attribute.KeyValue{attribute.String(uotel.AttrInputFileID, batch.BatchSpec.InputFileID)}
	if batch.OutputFileID != "" {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrOutputFileID, batch.OutputFileID))
	}
	if batch.ErrorFileID != "" {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrErrorFileID, batch.ErrorFileID))
	}
	trace.SpanFromContext(ctx).SetAttributes(spanAttrs...)

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

func (c *BatchAPIHandler) CancelBatch(w http.ResponseWriter, r *http.Request) {
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

	spanAttrs := []attribute.KeyValue{attribute.String(uotel.AttrInputFileID, batch.BatchSpec.InputFileID)}
	if batch.OutputFileID != "" {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrOutputFileID, batch.OutputFileID))
	}
	if batch.ErrorFileID != "" {
		spanAttrs = append(spanAttrs, attribute.String(uotel.AttrErrorFileID, batch.ErrorFileID))
	}
	trace.SpanFromContext(ctx).SetAttributes(spanAttrs...)

	// Check if batch can be cancelled
	if batch.Status.IsFinal() {
		apiErr := openai.NewAPIError(http.StatusBadRequest, "", fmt.Sprintf("Batch with status %s cannot be cancelled", batch.Status), nil)
		common.WriteAPIError(w, r, apiErr)
		return
	}

	// Try to remove from the priority queue first.
	// Reconstruct the exact SLO score from the stored tag.
	removedFromQueue := false
	sloStr, hasSLO := item.Tags[batch_types.TagSLO]
	sloMicro, parseErr := strconv.ParseInt(sloStr, 10, 64)
	if hasSLO && parseErr == nil {
		slo := time.UnixMicro(sloMicro).UTC()
		jobPriority := &api.BatchJobPriority{
			ID:  batch.ID,
			SLO: slo,
		}
		nDeleted, err := c.clients.Queue.PQDelete(ctx, jobPriority)
		if err != nil {
			logger.Error(err, "failed to remove batch from queue")
			common.WriteInternalServerError(w, r)
			return
		}
		removedFromQueue = nDeleted > 0
	} else {
		logger.V(logging.WARNING).Info("SLO tag missing or malformed, skipping queue removal", "key", batch_types.TagSLO, "hasSLO", hasSLO, "error", parseErr)
	}

	if removedFromQueue {
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
				TTL:  c.config.BatchAPI.GetBatchEventTTLSeconds(),
			},
		}
		_, err = c.clients.Event.ECProducerSendEvents(ctx, event)
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

	if err := c.clients.BatchDB.DBUpdate(ctx, dbItem); err != nil {
		logger.Error(err, "failed to update batch in database")
		common.WriteInternalServerError(w, r)
		return
	}

	common.WriteJSONResponse(w, r, http.StatusOK, batch)
}

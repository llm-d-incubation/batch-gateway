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
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
)

type BatchApiHandler struct {
}

func NewBatchApiHandler() *BatchApiHandler {
	return &BatchApiHandler{}
}

func (c *BatchApiHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodPost,
			Pattern:     "/v1/batches/{batch_id}/cancel",
			HandlerFunc: c.CancelBatch,
		},
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
	}
}

func (c *BatchApiHandler) CancelBatch(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *BatchApiHandler) CreateBatch(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *BatchApiHandler) ListBatches(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *BatchApiHandler) RetrieveBatch(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

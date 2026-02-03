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

// The file contains unit tests for batch handler.
package batch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockapi "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

func setupBatchApiHandlerForTest() *BatchApiHandler {
	config := &common.ServerConfig{}
	dbClient := mockapi.NewMockBatchDBClient()
	eventClient := mockapi.NewMockBatchEventChannelClient()
	queueClient := mockapi.NewMockBatchPriorityQueueClient()
	statusClient := mockapi.NewMockBatchStatusClient()
	handler := NewBatchApiHandler(config, dbClient, queueClient, eventClient, statusClient)
	return handler
}

func TestBatchHandler(t *testing.T) {

	t.Run("CreateBatch", func(t *testing.T) {
		handler := setupBatchApiHandlerForTest()

		// create batch
		reqBody := openai.CreateBatchRequest{
			InputFileID:      "file-abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
		}

		body, err := json.Marshal(reqBody)
		if err != nil {
			t.Fatalf("Failed to marshal request body: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/batches", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		handler.CreateBatch(rr, req)

		// verify response
		if status := rr.Code; status != http.StatusOK {
			t.Errorf("Handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}
		t.Logf("Response Body: %s", rr.Body.String())

		var batch openai.Batch
		if err := json.NewDecoder(rr.Body).Decode(&batch); err != nil {
			t.Fatalf("Failed to decode response body: %v", err)
		}

		if batch.Object != "batch" {
			t.Errorf("Expected object to be 'batch', got %v", batch.Object)
		}
		if batch.Endpoint != openai.EndpointChatCompletions {
			t.Errorf("Expected endpoint to be '%s', got %v", openai.EndpointChatCompletions, batch.Endpoint)
		}
		if batch.InputFileID != "file-abc123" {
			t.Errorf("Expected input_file_id to be 'file-abc123', got %v", batch.InputFileID)
		}
		if batch.CompletionWindow != "24h" {
			t.Errorf("Expected completion_window to be '24h', got %v", batch.CompletionWindow)
		}
		if batch.BatchStatusInfo.Status != openai.BatchStatusValidating {
			t.Errorf("Expected status to be '%s', got %v", openai.BatchStatusValidating, batch.BatchStatusInfo)
		}
		if batch.RequestCounts.Total != 0 {
			t.Errorf("Expected request_counts.total to be 0, got %v", batch.RequestCounts.Total)
		}
		if batch.ID == "" {
			t.Error("Expected batch ID to be generated")
		}
	})

	t.Run("RetrieveBatch", func(t *testing.T) {
		handler := setupBatchApiHandlerForTest()
		dbClient := handler.dbClient

		// create a batch first
		batchID := "batch-test-123"
		specData, _ := json.Marshal(openai.BatchSpec{
			InputFileID:      "file-abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
			CreatedAt:        time.Now().UTC().Unix(),
		})
		statusData, _ := json.Marshal(openai.BatchStatusInfo{
			Status: openai.BatchStatusValidating,
			RequestCounts: openai.BatchRequestCounts{
				Total:     0,
				Completed: 0,
				Failed:    0,
			},
		})
		dbClient.DBStore(context.Background(), &api.BatchItem{
			ID: batchID,
			// SLO:    time.Now().UTC().Add(24 * time.Hour),
			// TTL:    86400,
			Tags:   map[string]string{},
			Spec:   specData,
			Status: statusData,
		})

		// get batch
		req := httptest.NewRequest(http.MethodGet, "/v1/batches/"+batchID, nil)
		req.SetPathValue("batch_id", batchID)
		rr := httptest.NewRecorder()
		handler.RetrieveBatch(rr, req)

		// verify response
		if status := rr.Code; status != http.StatusOK {
			t.Errorf("Handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}
		t.Logf("Response Body: %s", rr.Body.String())

		var batch openai.Batch
		if err := json.NewDecoder(rr.Body).Decode(&batch); err != nil {
			t.Fatalf("Failed to decode response body: %v", err)
		}

		if batch.ID != batchID {
			t.Errorf("Expected batch ID to be %s, got %s", batchID, batch.ID)
		}
		if batch.Status != openai.BatchStatusValidating {
			t.Errorf("Expected status to be '%s', got %s", openai.BatchStatusValidating, batch.Status)
		}
	})

	t.Run("ListBatches", func(t *testing.T) {
		handler := setupBatchApiHandlerForTest()
		dbClient := handler.dbClient

		// create two batches
		for i := range 2 {
			batchID := fmt.Sprintf("batch-test-%d", i)
			specData, _ := json.Marshal(openai.BatchSpec{
				InputFileID:      fmt.Sprintf("file-%d", i),
				Endpoint:         openai.EndpointChatCompletions,
				CompletionWindow: "24h",
				CreatedAt:        time.Now().UTC().Unix(),
			})
			statusData, _ := json.Marshal(openai.BatchStatusInfo{
				Status: openai.BatchStatusValidating,
				RequestCounts: openai.BatchRequestCounts{
					Total:     0,
					Completed: 0,
					Failed:    0,
				},
			})
			dbClient.DBStore(context.Background(), &api.BatchItem{
				ID: batchID,
				// SLO:    time.Now().UTC().Add(24 * time.Hour),
				// TTL:    86400,
				Tags:   map[string]string{},
				Spec:   specData,
				Status: statusData,
			})
		}

		// list batches
		req := httptest.NewRequest(http.MethodGet, "/v1/batches?limit=10", nil)
		rr := httptest.NewRecorder()
		handler.ListBatches(rr, req)

		// verify response
		if status := rr.Code; status != http.StatusOK {
			t.Errorf("Handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}
		t.Logf("Response Body: %s", rr.Body.String())

		var resp openai.ListBatchResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("Failed to decode response body: %v", err)
		}

		if resp.Object != "list" {
			t.Errorf("Expected object to be 'list', got %v", resp.Object)
		}

		if len(resp.Data) != 2 {
			t.Errorf("Expected 2 batches, got %d", len(resp.Data))
		}

		// Verify pagination fields
		if resp.HasMore != false {
			t.Errorf("Expected has_more to be false, got %v", resp.HasMore)
		}

		if resp.FirstID == "" {
			t.Errorf("Expected first_id to be set, got %v", resp.FirstID)
		}

		if resp.LastID == "" {
			t.Errorf("Expected last_id to be set, got %v", resp.LastID)
		}
	})

	t.Run("CancelBatch", func(t *testing.T) {
		handler := setupBatchApiHandlerForTest()
		dbClient := handler.dbClient

		// create a batch first
		batchID := "batch-test-cancel"
		specData, _ := json.Marshal(openai.BatchSpec{
			InputFileID:      "file-abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
			CreatedAt:        time.Now().UTC().Unix(),
		})
		statusData, _ := json.Marshal(openai.BatchStatusInfo{
			Status: openai.BatchStatusInProgress,
			RequestCounts: openai.BatchRequestCounts{
				Total:     10,
				Completed: 5,
				Failed:    0,
			},
		})
		dbClient.DBStore(context.Background(), &api.BatchItem{
			ID: batchID,
			// SLO:    time.Now().UTC().Add(24 * time.Hour),
			// TTL:    86400,
			Tags:   map[string]string{},
			Spec:   specData,
			Status: statusData,
		})

		req := httptest.NewRequest(http.MethodPost, "/v1/batches/"+batchID+"/cancel", nil)
		req.SetPathValue("batch_id", batchID)
		rr := httptest.NewRecorder()
		handler.CancelBatch(rr, req)

		// verify response
		if status := rr.Code; status != http.StatusOK {
			t.Errorf("Handler returned wrong status code: got %v want %v", status, http.StatusOK)
		}
		t.Logf("Response Body: %s", rr.Body.String())

		var batch openai.Batch
		if err := json.NewDecoder(rr.Body).Decode(&batch); err != nil {
			t.Fatalf("Failed to decode response body: %v", err)
		}

		if batch.ID != batchID {
			t.Errorf("Expected batch ID to be %s, got %s", batchID, batch.ID)
		}
		if batch.Status != openai.BatchStatusCancelling {
			t.Errorf("Expected status to be '%s', got %s", openai.BatchStatusCancelling, batch.Status)
		}
		if batch.CancellingAt == nil {
			t.Error("Expected cancelling_at to be set")
		}
	})
}

// Benchmark tests for batch handler
func BenchmarkBatchHandler(b *testing.B) {
	handler := setupBatchApiHandlerForTest()
	dbClient := handler.dbClient

	b.Run("CreateBatch", func(b *testing.B) {
		reqBody := openai.CreateBatchRequest{
			InputFileID:      "file-abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
		}

		bodyBytes, _ := json.Marshal(reqBody)

		b.ResetTimer()
		for b.Loop() {
			req := httptest.NewRequest(http.MethodPost, "/v1/batches", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			handler.CreateBatch(rr, req)
		}
	})

	b.Run("RetrieveBatch", func(b *testing.B) {

		// Setup: create a batch first
		batchID := "batch-benchmark-123"
		specData, _ := json.Marshal(openai.BatchSpec{
			InputFileID:      "file-abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
		})
		statusData, _ := json.Marshal(openai.BatchStatusInfo{
			Status: openai.BatchStatusValidating,
			RequestCounts: openai.BatchRequestCounts{
				Total:     0,
				Completed: 0,
				Failed:    0,
			},
		})
		dbClient.DBStore(context.Background(), &api.BatchItem{
			ID: batchID,
			// SLO:    time.Now().UTC().Add(24 * time.Hour),
			// TTL:    86400,
			Tags:   map[string]string{},
			Spec:   specData,
			Status: statusData,
		})

		b.ResetTimer()
		for b.Loop() {
			req := httptest.NewRequest(http.MethodGet, "/v1/batches/"+batchID, nil)
			req.SetPathValue("batch_id", batchID)
			rr := httptest.NewRecorder()
			handler.RetrieveBatch(rr, req)
		}
	})

	b.Run("ListBatches", func(b *testing.B) {

		// Setup: create multiple batches
		for i := range 10 {
			batchID := fmt.Sprintf("batch-benchmark-%d", i)
			specData, _ := json.Marshal(openai.BatchSpec{
				InputFileID:      fmt.Sprintf("file-%d", i),
				Endpoint:         openai.EndpointChatCompletions,
				CompletionWindow: "24h",
				CreatedAt:        time.Now().UTC().Unix(),
			})
			statusData, _ := json.Marshal(openai.BatchStatusInfo{
				Status: openai.BatchStatusValidating,
				RequestCounts: openai.BatchRequestCounts{
					Total:     0,
					Completed: 0,
					Failed:    0,
				},
			})
			dbClient.DBStore(context.Background(), &api.BatchItem{
				ID: batchID,
				// SLO:    time.Now().UTC().Add(24 * time.Hour),
				// TTL:    86400,
				Tags:   map[string]string{},
				Spec:   specData,
				Status: statusData,
			})
		}

		b.ResetTimer()
		for b.Loop() {
			req := httptest.NewRequest(http.MethodGet, "/v1/batches?limit=10", nil)
			rr := httptest.NewRecorder()
			handler.ListBatches(rr, req)
		}
	})

	b.Run("CancelBatch", func(b *testing.B) {

		specData, _ := json.Marshal(openai.BatchSpec{
			InputFileID:      "file-abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
			CreatedAt:        time.Now().UTC().Unix(),
		})

		b.ResetTimer()
		for i := range b.N {
			// Create a new batch for each iteration
			b.StopTimer()
			batchID := fmt.Sprintf("batch-benchmark-cancel-%d", i)
			statusData, _ := json.Marshal(openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
				RequestCounts: openai.BatchRequestCounts{
					Total:     10,
					Completed: 5,
					Failed:    0,
				},
			})
			dbClient.DBStore(context.Background(), &api.BatchItem{
				ID: batchID,
				// SLO:    time.Now().UTC().Add(24 * time.Hour),
				// TTL:    86400,
				Tags:   map[string]string{},
				Spec:   specData,
				Status: statusData,
			})
			b.StartTimer()

			req := httptest.NewRequest(http.MethodPost, "/v1/batches/"+batchID+"/cancel", nil)
			req.SetPathValue("batch_id", batchID)
			rr := httptest.NewRecorder()
			handler.CancelBatch(rr, req)
		}
	})
}

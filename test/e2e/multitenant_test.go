// Copyright 2026 The llm-d Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
)

func testMultiTenant(t *testing.T) {
	t.Run("Isolation", doTestMultiTenantIsolation)
}

// doTestMultiTenantIsolation verifies that resources created by tenant A
// are not visible to tenant B.
func doTestMultiTenantIsolation(t *testing.T) {
	t.Helper()

	ctx := context.Background()
	tenantA := fmt.Sprintf("tenant-a-%s", testRunID)
	tenantB := fmt.Sprintf("tenant-b-%s", testRunID)
	clientA := newClientForTenant(tenantA)
	clientB := newClientForTenant(tenantB)

	// ── Files isolation ──────────────────────────────────────────────────

	// Tenant A creates a file
	filenameA := fmt.Sprintf("tenant-a-file-%s.jsonl", testRunID)
	fileAID := mustCreateUniqueFileWithClient(t, clientA, filenameA, testJSONL)
	t.Logf("tenant A created file: %s", fileAID)

	// Tenant B should not see tenant A's file
	_, err := clientB.Files.Get(ctx, fileAID)
	if err == nil {
		t.Error("tenant B was able to retrieve tenant A's file; expected 404")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 when tenant B accesses tenant A's file, got %d", apiErr.StatusCode)
		}
	}

	// Tenant B's file list should not contain tenant A's file
	pageB, err := clientB.Files.List(ctx, openai.FileListParams{})
	if err != nil {
		t.Fatalf("tenant B: list files failed: %v", err)
	}
	for _, f := range pageB.Data {
		if f.ID == fileAID {
			t.Error("tenant B's file list contains tenant A's file")
		}
	}

	// Tenant B should not be able to create a batch using tenant A's file
	_, err = clientB.Batches.New(ctx, openai.BatchNewParams{
		InputFileID:      fileAID,
		Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
		CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
	})
	if err == nil {
		t.Error("tenant B was able to create a batch with tenant A's file; expected error")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 when tenant B uses tenant A's file_id, got %d", apiErr.StatusCode)
		}
	}

	// ── Batches isolation ────────────────────────────────────────────────

	// Tenant A creates a batch
	batchA, err := clientA.Batches.New(ctx, openai.BatchNewParams{
		InputFileID:      fileAID,
		Endpoint:         openai.BatchNewParamsEndpointV1ChatCompletions,
		CompletionWindow: openai.BatchNewParamsCompletionWindow24h,
	})
	if err != nil {
		t.Fatalf("tenant A: create batch failed: %v", err)
	}
	t.Logf("tenant A created batch: %s", batchA.ID)

	// Tenant B should not see tenant A's batch
	_, err = clientB.Batches.Get(ctx, batchA.ID)
	if err == nil {
		t.Error("tenant B was able to retrieve tenant A's batch; expected 404")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 when tenant B accesses tenant A's batch, got %d", apiErr.StatusCode)
		}
	}

	// Tenant B's batch list should not contain tenant A's batch
	batchPageB, err := clientB.Batches.List(ctx, openai.BatchListParams{})
	if err != nil {
		t.Fatalf("tenant B: list batches failed: %v", err)
	}
	for _, b := range batchPageB.Data {
		if b.ID == batchA.ID {
			t.Error("tenant B's batch list contains tenant A's batch")
		}
	}

	// Tenant B should not be able to cancel tenant A's batch
	_, err = clientB.Batches.Cancel(ctx, batchA.ID)
	if err == nil {
		t.Error("tenant B was able to cancel tenant A's batch; expected 404")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 when tenant B cancels tenant A's batch, got %d", apiErr.StatusCode)
		}
	}
}

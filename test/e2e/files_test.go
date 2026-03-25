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
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

func testFiles(t *testing.T) {
	t.Run("Lifecycle", doTestFileLifecycle)
	t.Run("Create", func(t *testing.T) {
		t.Run("DuplicateFilename", doTestDuplicateFileUpload)
	})
	t.Run("List", func(t *testing.T) {
		t.Run("Pagination", doTestFilePagination)
		t.Run("PurposeFilter", doTestFilePurposeFilter)
	})
}

// doTestDuplicateFileUpload verifies that uploading two files with the same
// filename under the same tenant succeeds. Each upload should get a unique
// fileID and storage path. Reproduces https://github.com/llm-d-incubation/batch-gateway/issues/214
func doTestDuplicateFileUpload(t *testing.T) {
	t.Helper()

	client := newClient()
	filename := fmt.Sprintf("duplicate-%s.jsonl", testRunID)

	// First upload — should succeed
	fileID1 := mustCreateFileWithClient(t, client, filename, testJSONL)
	t.Logf("first upload: fileID=%s, filename=%s", fileID1, filename)

	// Second upload with the same filename — should also succeed with a different fileID
	fileID2 := mustCreateFileWithClient(t, client, filename, testJSONL)
	t.Logf("second upload: fileID=%s, filename=%s", fileID2, filename)

	if fileID1 == fileID2 {
		t.Errorf("expected different file IDs, both got %s", fileID1)
	}

	// Both files should be retrievable and preserve the original filename
	got1, err := client.Files.Get(context.Background(), fileID1)
	if err != nil {
		t.Fatalf("retrieve first file failed: %v", err)
	}
	if got1.Filename != filename {
		t.Errorf("first file: expected filename %q, got %q", filename, got1.Filename)
	}

	got2, err := client.Files.Get(context.Background(), fileID2)
	if err != nil {
		t.Fatalf("retrieve second file failed: %v", err)
	}
	if got2.Filename != filename {
		t.Errorf("second file: expected filename %q, got %q", filename, got2.Filename)
	}
}

// doTestFileLifecycle uploads a file, verifies list, retrieve, download, then deletes it.
func doTestFileLifecycle(t *testing.T) {
	t.Helper()

	client := newClient()

	// Create
	filename := fmt.Sprintf("test-file-lifecycle-%s.jsonl", testRunID)
	fileID := mustCreateFile(t, filename, testJSONL)

	// List
	page, err := client.Files.List(context.Background(), openai.FileListParams{})
	if err != nil {
		t.Fatalf("list files failed: %v", err)
	}
	t.Logf("list files: got %d items", len(page.Data))

	// Retrieve
	got, err := client.Files.Get(context.Background(), fileID)
	if err != nil {
		t.Fatalf("retrieve file failed: %v", err)
	}
	if got.ID != fileID {
		t.Errorf("expected ID %q, got %q", fileID, got.ID)
	}
	if got.Filename != filename {
		t.Errorf("expected filename %q, got %q", filename, got.Filename)
	}
	if got.Purpose != openai.FileObjectPurposeBatch {
		t.Errorf("expected purpose %q, got %q", openai.FileObjectPurposeBatch, got.Purpose)
	}
	if got.Bytes != int64(len(testJSONL)) {
		t.Errorf("expected bytes %d, got %d", len(testJSONL), got.Bytes)
	}
	if got.Status != openai.FileObjectStatusUploaded {
		t.Errorf("expected status %q, got %q", openai.FileObjectStatusUploaded, got.Status)
	}

	// Download
	resp, err := client.Files.Content(context.Background(), fileID)
	if err != nil {
		t.Fatalf("download file failed: %v", err)
	}
	// Verify download filename matches the original upload filename
	cd := resp.Header.Get("Content-Disposition")
	wantCD := fmt.Sprintf(`attachment; filename=%q`, filename)
	if cd != wantCD {
		t.Errorf("Content-Disposition mismatch\ngot:  %s\nwant: %s", cd, wantCD)
	}

	content, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("failed to read file content: %v", err)
	}
	if strings.TrimSpace(string(content)) != strings.TrimSpace(testJSONL) {
		t.Errorf("downloaded content does not match uploaded content\ngot:  %q\nwant: %q", string(content), testJSONL)
	}

	// Delete and verify a subsequent Get returns 404.
	result, err := client.Files.Delete(context.Background(), fileID)
	if err != nil {
		t.Fatalf("delete file failed: %v", err)
	}
	if !result.Deleted {
		t.Error("expected deleted to be true")
	}

	_, err = client.Files.Get(context.Background(), fileID)
	if err == nil {
		t.Error("expected error after deletion, got nil")
	} else {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode != http.StatusNotFound {
			t.Errorf("expected 404 after deletion, got %d", apiErr.StatusCode)
		}
	}
}

// doTestFilePagination creates 3 files under an isolated tenant and verifies
// that limit/after pagination returns correct pages with no duplicates.
func doTestFilePagination(t *testing.T) {
	t.Helper()

	tenant := fmt.Sprintf("pagination-files-%s", testRunID)
	client := newClientForTenant(tenant)
	ctx := context.Background()

	// Create 3 files under a dedicated tenant to avoid interference.
	const count = 3
	createdIDs := make([]string, count)
	for i := range count {
		filename := fmt.Sprintf("pagination-file-%d-%s.jsonl", i, testRunID)
		fileID := mustCreateUniqueFileWithClient(t, client, filename, testJSONL)
		createdIDs[i] = fileID
		t.Logf("created file %d: %s", i, fileID)
	}

	// Page 1: limit=2, no after → expect 2 items, has_more=true
	page1, err := client.Files.List(ctx, openai.FileListParams{
		Limit: param.NewOpt(int64(2)),
	})
	if err != nil {
		t.Fatalf("list files page 1 failed: %v", err)
	}
	if len(page1.Data) != 2 {
		t.Fatalf("page 1: expected 2 items, got %d", len(page1.Data))
	}
	if !page1.HasMore {
		t.Error("page 1: expected has_more=true")
	}

	page1IDs := make([]string, len(page1.Data))
	for i, f := range page1.Data {
		page1IDs[i] = f.ID
	}
	t.Logf("page 1 IDs: %v (has_more=%v)", page1IDs, page1.HasMore)

	// Page 2: limit=2, after="2" (offset) → expect 1 item, has_more=false
	page2, err := client.Files.List(ctx, openai.FileListParams{
		Limit: param.NewOpt(int64(2)),
		After: param.NewOpt("2"),
	})
	if err != nil {
		t.Fatalf("list files page 2 failed: %v", err)
	}
	if len(page2.Data) != 1 {
		t.Fatalf("page 2: expected 1 item, got %d", len(page2.Data))
	}
	if page2.HasMore {
		t.Error("page 2: expected has_more=false")
	}

	page2IDs := make([]string, len(page2.Data))
	for i, f := range page2.Data {
		page2IDs[i] = f.ID
	}
	t.Logf("page 2 IDs: %v (has_more=%v)", page2IDs, page2.HasMore)

	// Verify no overlap and full coverage.
	allIDs := append(page1IDs, page2IDs...)
	assertSliceEqual(t, createdIDs, allIDs)
}

// doTestFilePurposeFilter creates a file with purpose=batch, then lists with
// purpose filter and verifies the file appears (or not) as expected.
func doTestFilePurposeFilter(t *testing.T) {
	t.Helper()

	tenant := fmt.Sprintf("purpose-filter-%s", testRunID)
	client := newClientForTenant(tenant)
	ctx := context.Background()

	// Create a file with purpose=batch
	filename := fmt.Sprintf("purpose-filter-%s.jsonl", testRunID)
	fileID := mustCreateUniqueFileWithClient(t, client, filename, testJSONL)

	// List with purpose=batch → should contain our file
	batchPage, err := client.Files.List(ctx, openai.FileListParams{
		Purpose: param.NewOpt("batch"),
	})
	if err != nil {
		t.Fatalf("list files with purpose=batch failed: %v", err)
	}
	found := false
	for _, f := range batchPage.Data {
		if f.ID == fileID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("file %s not found when listing with purpose=batch", fileID)
	}

	// List with purpose=fine-tune → should NOT contain our file
	ftPage, err := client.Files.List(ctx, openai.FileListParams{
		Purpose: param.NewOpt("fine-tune"),
	})
	if err != nil {
		t.Fatalf("list files with purpose=fine-tune failed: %v", err)
	}
	for _, f := range ftPage.Data {
		if f.ID == fileID {
			t.Errorf("file %s should not appear when listing with purpose=fine-tune", fileID)
		}
	}
}

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
)

func testFiles(t *testing.T) {
	t.Run("Lifecycle", doTestFileLifecycle)
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

	// Download
	resp, err := client.Files.Content(context.Background(), fileID)
	if err != nil {
		t.Fatalf("download file failed: %v", err)
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

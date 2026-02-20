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

package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	dbapi "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	dbmock "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	fsmock "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"k8s.io/klog/v2"
)

func TestFileHandler(t *testing.T) {
	// Setup logging once for all subtests
	klog.InitFlags(nil)

	t.Run("CreateFile", doTestCreateFile)
	t.Run("ListFiles", doTestListFiles)
	t.Run("RetrieveFile", doTestRetrieveFile)
	t.Run("DownloadFile", doTestDownloadFile)
	t.Run("DeleteFile", doTestDeleteFile)
}

// setupTestHandler creates a test handler with mocked dependencies
func setupTestHandler(t *testing.T) (*FileApiHandler, *dbmock.MockDBClient[dbapi.FileItem, dbapi.FileQuery], *fsmock.MockBatchFilesClient, context.Context) {
	t.Helper()

	dbClient := dbmock.NewMockDBClient[dbapi.FileItem, dbapi.FileQuery](
		func(f *dbapi.FileItem) string { return f.ID },
		func(q *dbapi.FileQuery) *dbapi.BaseQuery { return &q.BaseQuery },
	)
	filesClient := fsmock.NewMockBatchFilesClient()

	config := &common.ServerConfig{
		FilesAPI: common.FilesAPIConfig{
			MaxSizeBytes:             common.DefaultMaxFileSizeBytes,
			MaxLineCount:             common.DefaultMaxFileLineCount,
			DefaultExpirationSeconds: 30 * 24 * 60 * 60, // 30 days (test value)
		},
	}

	handler := NewFileApiHandler(config, dbClient, filesClient)

	ctx := context.Background()
	logger := klog.FromContext(ctx)
	ctx = klog.NewContext(ctx, logger)

	return handler, dbClient, filesClient, ctx
}

// createTestFile is a helper function that creates a test file and returns the created FileObject.
func createTestFile(t *testing.T, handler *FileApiHandler, ctx context.Context, filename, purpose, content string) openai.FileObject {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	fileWriter, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := io.WriteString(fileWriter, content); err != nil {
		t.Fatalf("failed to write file content: %v", err)
	}

	if err := writer.WriteField("purpose", purpose); err != nil {
		t.Fatalf("failed to write purpose field: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/files", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req = req.WithContext(ctx)

	w := httptest.NewRecorder()
	handler.CreateFile(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("failed to create file %s: status %d, body: %s", filename, w.Code, w.Body.String())
	}

	var fileObj openai.FileObject
	if err := json.Unmarshal(w.Body.Bytes(), &fileObj); err != nil {
		t.Fatalf("failed to parse create file response: %v", err)
	}

	return fileObj
}

func doTestCreateFile(t *testing.T) {
	handler, dbClient, filesClient, ctx := setupTestHandler(t)

	// Test file content - each line will get a newline appended by scanner during storage
	testLines := []string{
		`{"custom_id":"request-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4","messages":[{"role":"user","content":"Hello"}]}}`,
		`{"custom_id":"request-2","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4","messages":[{"role":"user","content":"World"}]}}`,
	}
	fileContent := strings.Join(testLines, "\n")

	// Calculate expected size after scanner processing (each line gets a newline appended)
	expectedBytes := int64(0)
	for _, line := range testLines {
		expectedBytes += int64(len(line)) + 1 // +1 for newline
	}

	// Create file using helper
	fileObj := createTestFile(t, handler, ctx, "test-batch.jsonl", "batch", fileContent)

	// Validate response fields
	if fileObj.Object != "file" {
		t.Errorf("expected object 'file', got '%s'", fileObj.Object)
	}
	if fileObj.Purpose != openai.FileObjectPurposeBatch {
		t.Errorf("expected purpose 'batch', got '%s'", fileObj.Purpose)
	}
	if fileObj.Status != openai.FileObjectStatusUploaded {
		t.Errorf("expected status 'uploaded', got '%s'", fileObj.Status)
	}
	if fileObj.Filename != "test-batch.jsonl" {
		t.Errorf("expected filename 'test-batch.jsonl', got '%s'", fileObj.Filename)
	}
	if !strings.HasPrefix(fileObj.ID, "file_") {
		t.Errorf("expected file ID to start with 'file_', got '%s'", fileObj.ID)
	}
	if fileObj.Bytes != expectedBytes {
		t.Errorf("expected bytes %d, got %d", expectedBytes, fileObj.Bytes)
	}
	if fileObj.CreatedAt <= 0 {
		t.Errorf("expected createdAt > 0, got %d", fileObj.CreatedAt)
	}
	if fileObj.ExpiresAt <= 0 {
		t.Errorf("expected expiresAt > 0, got %d", fileObj.ExpiresAt)
	}

	// Verify file was stored in DB
	items, _, _, err := dbClient.DBGet(ctx, &dbapi.FileQuery{
		BaseQuery: dbapi.BaseQuery{IDs: []string{fileObj.ID}},
	}, true, 0, 10)
	if err != nil {
		t.Fatalf("failed to get file from DB: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item in DB, got %d", len(items))
	}
	if items[0].ID != fileObj.ID {
		t.Errorf("expected DB item ID '%s', got '%s'", fileObj.ID, items[0].ID)
	}

	// Verify file was actually uploaded to storage
	// Mock stores files at /tmp/batch-gateway-files/{folderName}/{fileName}
	fileName := fileObj.Filename
	folderName := common.DefaultTenantID
	fileReader, fileMeta, err := filesClient.Retrieve(ctx, fileName, folderName)
	if err != nil {
		t.Fatalf("failed to retrieve file from storage: %v", err)
	}
	defer fileReader.Close()

	// Read uploaded file content
	uploadedContent, err := io.ReadAll(fileReader)
	if err != nil {
		t.Fatalf("failed to read uploaded file: %v", err)
	}

	// Verify file size matches
	if fileMeta.Size != expectedBytes {
		t.Errorf("expected storage file size %d, got %d", expectedBytes, fileMeta.Size)
	}

	// Verify content matches (scanner adds newline to each line)
	expectedStoredContent := strings.Join(testLines, "\n") + "\n"
	if string(uploadedContent) != expectedStoredContent {
		t.Errorf("uploaded content doesn't match expected.\nExpected:\n%s\nGot:\n%s",
			expectedStoredContent, string(uploadedContent))
	}
}

func doTestListFiles(t *testing.T) {
	handler, _, _, ctx := setupTestHandler(t)

	// Create multiple test files with different purposes
	testFiles := []struct {
		filename string
		purpose  string
		content  string
	}{
		{"batch-file-1.jsonl", "batch", `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{}}`},
		{"batch-file-2.jsonl", "batch", `{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{}}`},
		{"finetune-file.jsonl", "fine-tune", `{"prompt":"hello","completion":"world"}`},
	}

	createdFileIDs := []string{}

	// Create each test file using helper
	for _, tf := range testFiles {
		fileObj := createTestFile(t, handler, ctx, tf.filename, tf.purpose, tf.content)
		createdFileIDs = append(createdFileIDs, fileObj.ID)
	}

	// Test 1: List all files (no filter)
	t.Run("ListAll", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.ListFiles(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
			t.Logf("response body: %s", w.Body.String())
		}

		var listResp openai.ListFilesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
			t.Fatalf("failed to parse list response: %v", err)
		}

		if listResp.Object != "list" {
			t.Errorf("expected object 'list', got '%s'", listResp.Object)
		}

		if len(listResp.Data) != len(testFiles) {
			t.Errorf("expected %d files, got %d", len(testFiles), len(listResp.Data))
		}

		// Verify all created files are in the list
		for _, expectedID := range createdFileIDs {
			found := false
			for _, file := range listResp.Data {
				if file.ID == expectedID {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expected file ID %s not found in list", expectedID)
			}
		}

		// Verify FirstID and LastID
		if len(listResp.Data) > 0 {
			if listResp.FirstID != listResp.Data[0].ID {
				t.Errorf("expected first_id '%s', got '%s'", listResp.Data[0].ID, listResp.FirstID)
			}
			if listResp.LastID != listResp.Data[len(listResp.Data)-1].ID {
				t.Errorf("expected last_id '%s', got '%s'", listResp.Data[len(listResp.Data)-1].ID, listResp.LastID)
			}
		}
	})

	// Test 2: Test limit parameter
	t.Run("LimitParameter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files?limit=1", nil)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.ListFiles(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
		}

		var listResp openai.ListFilesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
			t.Fatalf("failed to parse list response: %v", err)
		}

		if len(listResp.Data) != 1 {
			t.Errorf("expected 1 file with limit=1, got %d", len(listResp.Data))
		}

		if listResp.HasMore != true {
			t.Errorf("expected has_more=true when limiting results, got %v", listResp.HasMore)
		}
	})

	// Test 4: Invalid purpose parameter
	t.Run("InvalidPurpose", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files?purpose=invalid", nil)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.ListFiles(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for invalid purpose, got %d", http.StatusBadRequest, w.Code)
		}
	})

	// Test 5: Test after parameter (cursor-based pagination)
	t.Run("AfterParameter", func(t *testing.T) {
		// First request: get first 2 items
		req := httptest.NewRequest(http.MethodGet, "/v1/files?limit=2", nil)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.ListFiles(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
		}

		var listResp1 openai.ListFilesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &listResp1); err != nil {
			t.Fatalf("failed to parse list response: %v", err)
		}

		if len(listResp1.Data) != 2 {
			t.Errorf("expected 2 files in first page, got %d", len(listResp1.Data))
		}

		if !listResp1.HasMore {
			t.Errorf("expected has_more=true for first page, got false")
		}

		// Second request: use after cursor to get remaining items
		req = httptest.NewRequest(http.MethodGet, "/v1/files?limit=2&after=2", nil)
		req = req.WithContext(ctx)

		w = httptest.NewRecorder()
		handler.ListFiles(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
		}

		var listResp2 openai.ListFilesResponse
		if err := json.Unmarshal(w.Body.Bytes(), &listResp2); err != nil {
			t.Fatalf("failed to parse list response: %v", err)
		}

		// Should get remaining 1 file
		if len(listResp2.Data) != 1 {
			t.Errorf("expected 1 file in second page, got %d", len(listResp2.Data))
		}

		if listResp2.HasMore {
			t.Errorf("expected has_more=false for last page, got true")
		}

		// Verify no overlap between pages
		for _, file1 := range listResp1.Data {
			for _, file2 := range listResp2.Data {
				if file1.ID == file2.ID {
					t.Errorf("found duplicate file ID %s across pages", file1.ID)
				}
			}
		}
	})

	// Test 6: Invalid after parameter
	t.Run("InvalidAfter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files?after=invalid", nil)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.ListFiles(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for invalid after, got %d", http.StatusBadRequest, w.Code)
		}
	})
}

func doTestRetrieveFile(t *testing.T) {
	handler, _, _, ctx := setupTestHandler(t)

	// Create a test file using helper
	testContent := `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{}}`
	createdFile := createTestFile(t, handler, ctx, "test-retrieve.jsonl", "batch", testContent)

	// Test 1: Retrieve existing file
	t.Run("RetrieveExistingFile", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files/"+createdFile.ID, nil)
		req.SetPathValue(pathParamFileID, createdFile.ID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.RetrieveFile(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
			t.Logf("response body: %s", w.Body.String())
		}

		var fileObj openai.FileObject
		if err := json.Unmarshal(w.Body.Bytes(), &fileObj); err != nil {
			t.Fatalf("failed to parse retrieve response: %v", err)
		}

		// Verify all fields match the created file
		if fileObj.ID != createdFile.ID {
			t.Errorf("expected ID '%s', got '%s'", createdFile.ID, fileObj.ID)
		}
		if fileObj.Filename != createdFile.Filename {
			t.Errorf("expected filename '%s', got '%s'", createdFile.Filename, fileObj.Filename)
		}
		if fileObj.Purpose != createdFile.Purpose {
			t.Errorf("expected purpose '%s', got '%s'", createdFile.Purpose, fileObj.Purpose)
		}
		if fileObj.Bytes != createdFile.Bytes {
			t.Errorf("expected bytes %d, got %d", createdFile.Bytes, fileObj.Bytes)
		}
		if fileObj.CreatedAt != createdFile.CreatedAt {
			t.Errorf("expected created_at %d, got %d", createdFile.CreatedAt, fileObj.CreatedAt)
		}
		if fileObj.ExpiresAt != createdFile.ExpiresAt {
			t.Errorf("expected expires_at %d, got %d", createdFile.ExpiresAt, fileObj.ExpiresAt)
		}
		if fileObj.Status != createdFile.Status {
			t.Errorf("expected status '%s', got '%s'", createdFile.Status, fileObj.Status)
		}
		if fileObj.Object != "file" {
			t.Errorf("expected object 'file', got '%s'", fileObj.Object)
		}
	})

	// Test 2: Retrieve non-existent file
	t.Run("RetrieveNonExistentFile", func(t *testing.T) {
		nonExistentID := "file_nonexistent"
		req := httptest.NewRequest(http.MethodGet, "/v1/files/"+nonExistentID, nil)
		req.SetPathValue(pathParamFileID, nonExistentID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.RetrieveFile(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected status %d for non-existent file, got %d", http.StatusNotFound, w.Code)
		}

		// Verify error message
		var errResp map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("failed to parse error response: %v", err)
		}

		if errObj, ok := errResp["error"].(map[string]interface{}); ok {
			if msg, ok := errObj["message"].(string); ok {
				expectedMsg := fmt.Sprintf("File with ID %s not found", nonExistentID)
				if msg != expectedMsg {
					t.Errorf("expected error message '%s', got '%s'", expectedMsg, msg)
				}
			}
		}
	})

	// Test 3: Missing file_id parameter
	t.Run("MissingFileID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files/", nil)
		// Don't set path value to simulate missing file_id
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.RetrieveFile(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for missing file_id, got %d", http.StatusBadRequest, w.Code)
		}
	})
}

func doTestDownloadFile(t *testing.T) {
	handler, _, _, ctx := setupTestHandler(t)

	// Create a test file with specific content using helper
	testLines := []string{
		`{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{}}`,
		`{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{}}`,
	}
	testContent := strings.Join(testLines, "\n")
	createdFile := createTestFile(t, handler, ctx, "test-download.jsonl", "batch", testContent)

	// Test 1: Download existing file
	t.Run("DownloadExistingFile", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files/"+createdFile.ID+"/content", nil)
		req.SetPathValue(pathParamFileID, createdFile.ID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.DownloadFile(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
			t.Logf("response body: %s", w.Body.String())
		}

		// Verify response headers
		contentType := w.Header().Get("Content-Type")
		if contentType != "application/octet-stream" {
			t.Errorf("expected Content-Type 'application/octet-stream', got '%s'", contentType)
		}

		contentDisposition := w.Header().Get("Content-Disposition")
		expectedDisposition := fmt.Sprintf("attachment; filename=%q", createdFile.Filename)
		if contentDisposition != expectedDisposition {
			t.Errorf("expected Content-Disposition '%s', got '%s'", expectedDisposition, contentDisposition)
		}

		contentLength := w.Header().Get("Content-Length")
		expectedLength := strconv.FormatInt(createdFile.Bytes, 10)
		if contentLength != expectedLength {
			t.Errorf("expected Content-Length '%s', got '%s'", expectedLength, contentLength)
		}

		// Verify downloaded content matches (scanner adds newline to each line)
		downloadedContent := w.Body.String()
		expectedContent := strings.Join(testLines, "\n") + "\n"
		if downloadedContent != expectedContent {
			t.Errorf("downloaded content doesn't match expected.\nExpected:\n%s\nGot:\n%s",
				expectedContent, downloadedContent)
		}

		// Verify content length
		if int64(len(downloadedContent)) != createdFile.Bytes {
			t.Errorf("expected downloaded size %d, got %d", createdFile.Bytes, len(downloadedContent))
		}
	})

	// Test 2: Download non-existent file
	t.Run("DownloadNonExistentFile", func(t *testing.T) {
		nonExistentID := "file_nonexistent"
		req := httptest.NewRequest(http.MethodGet, "/v1/files/"+nonExistentID+"/content", nil)
		req.SetPathValue(pathParamFileID, nonExistentID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.DownloadFile(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected status %d for non-existent file, got %d", http.StatusNotFound, w.Code)
		}
	})

	// Test 3: Missing file_id parameter
	t.Run("MissingFileID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/files//content", nil)
		// Don't set path value to simulate missing file_id
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.DownloadFile(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for missing file_id, got %d", http.StatusBadRequest, w.Code)
		}
	})
}

func doTestDeleteFile(t *testing.T) {
	handler, dbClient, filesClient, ctx := setupTestHandler(t)

	// Create a test file using helper
	testLines := []string{
		`{"custom_id":"request-1","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4","messages":[{"role":"user","content":"Hello"}]}}`,
		`{"custom_id":"request-2","method":"POST","url":"/v1/chat/completions","body":{"model":"gpt-4","messages":[{"role":"user","content":"World"}]}}`,
	}
	testContent := strings.Join(testLines, "\n")
	createdFile := createTestFile(t, handler, ctx, "test.jsonl", "batch", testContent)

	// Test 1: Delete existing file
	t.Run("DeleteExistingFile", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/files/"+createdFile.ID, nil)
		req.SetPathValue(pathParamFileID, createdFile.ID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.DeleteFile(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
			t.Logf("response body: %s", w.Body.String())
		}

		// Verify response structure
		var deleteResp openai.FileDeleteResponse
		if err := json.Unmarshal(w.Body.Bytes(), &deleteResp); err != nil {
			t.Fatalf("failed to parse delete response: %v", err)
		}

		if deleteResp.ID != createdFile.ID {
			t.Errorf("expected ID %s, got %s", createdFile.ID, deleteResp.ID)
		}

		if deleteResp.Object != "file" {
			t.Errorf("expected object 'file', got '%s'", deleteResp.Object)
		}

		if deleteResp.Deleted != true {
			t.Errorf("expected deleted=true, got %v", deleteResp.Deleted)
		}

		// Verify file is actually deleted from database
		items, _, _, err := dbClient.DBGet(ctx, &dbapi.FileQuery{
			BaseQuery: dbapi.BaseQuery{IDs: []string{createdFile.ID}},
		}, true, 0, 1)
		if err != nil {
			t.Fatalf("failed to query database: %v", err)
		}

		if len(items) != 0 {
			t.Errorf("expected file to be deleted from database, but still exists")
		}

		// Verify physical file is deleted from storage
		_, _, err = filesClient.Retrieve(ctx, createdFile.Filename, common.DefaultTenantID)
		if err == nil {
			t.Errorf("expected physical file to be deleted, but still exists")
		}
	})

	// Test 2: Delete non-existent file
	t.Run("DeleteNonExistentFile", func(t *testing.T) {
		nonExistentID := "file_nonexistent"
		req := httptest.NewRequest(http.MethodDelete, "/v1/files/"+nonExistentID, nil)
		req.SetPathValue(pathParamFileID, nonExistentID)
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.DeleteFile(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected status %d for non-existent file, got %d", http.StatusNotFound, w.Code)
		}
	})

	// Test 3: Missing file_id parameter
	t.Run("MissingFileID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/files/", nil)
		// Don't set path value to simulate missing file_id
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		handler.DeleteFile(w, req)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected status %d for missing file_id, got %d", http.StatusBadRequest, w.Code)
		}
	})
}

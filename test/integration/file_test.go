//go:build integration

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

package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

func TestFileUploadAndDownload(t *testing.T) {
	ts := newTestServer(t)

	content := `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"hello"}]}}` + "\n" +
		`{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"m","messages":[{"role":"user","content":"world"}]}}` + "\n"

	// Upload
	resp := ts.uploadFile(t, "test-input.jsonl", "batch", content)
	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp)
		t.Fatalf("upload: expected 200, got %d: %s", resp.StatusCode, body)
	}
	fileObj := decodeJSON[openai.FileObject](t, resp)

	if fileObj.ID == "" {
		t.Fatal("upload: expected non-empty file ID")
	}
	if fileObj.Object != "file" {
		t.Errorf("upload: expected object=file, got %q", fileObj.Object)
	}
	if fileObj.Filename != "test-input.jsonl" {
		t.Errorf("upload: expected filename=test-input.jsonl, got %q", fileObj.Filename)
	}
	if fileObj.Purpose != openai.FileObjectPurposeBatch {
		t.Errorf("upload: expected purpose=batch, got %q", fileObj.Purpose)
	}

	// Download content
	resp = ts.doRequest(t, http.MethodGet, "/v1/files/"+fileObj.ID+"/content")
	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp)
		t.Fatalf("download: expected 200, got %d: %s", resp.StatusCode, body)
	}
	downloaded := string(readBody(t, resp))

	if downloaded != content {
		t.Errorf("download: content mismatch\nwant: %q\ngot:  %q", content, downloaded)
	}
}

func TestFileRetrieve(t *testing.T) {
	ts := newTestServer(t)

	content := `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{}}` + "\n"
	resp := ts.uploadFile(t, "retrieve-test.jsonl", "batch", content)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload: expected 200, got %d", resp.StatusCode)
	}
	fileObj := decodeJSON[openai.FileObject](t, resp)

	// Retrieve metadata
	resp = ts.doRequest(t, http.MethodGet, "/v1/files/"+fileObj.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retrieve: expected 200, got %d", resp.StatusCode)
	}
	retrieved := decodeJSON[openai.FileObject](t, resp)

	if retrieved.ID != fileObj.ID {
		t.Errorf("retrieve: expected ID=%q, got %q", fileObj.ID, retrieved.ID)
	}
	if retrieved.Filename != "retrieve-test.jsonl" {
		t.Errorf("retrieve: expected filename=retrieve-test.jsonl, got %q", retrieved.Filename)
	}
	if retrieved.Purpose != openai.FileObjectPurposeBatch {
		t.Errorf("retrieve: expected purpose=batch, got %q", retrieved.Purpose)
	}
	if retrieved.Bytes <= 0 {
		t.Errorf("retrieve: expected positive bytes, got %d", retrieved.Bytes)
	}
	if retrieved.CreatedAt <= 0 {
		t.Errorf("retrieve: expected positive created_at, got %d", retrieved.CreatedAt)
	}
	if retrieved.Status != openai.FileObjectStatusUploaded {
		t.Errorf("retrieve: expected status=uploaded, got %q", retrieved.Status)
	}
}

func TestFileList(t *testing.T) {
	ts := newTestServer(t)

	content := `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{}}` + "\n"

	// Upload 3 files
	fileIDs := make(map[string]bool)
	for i := range 3 {
		names := []string{"a.jsonl", "b.jsonl", "c.jsonl"}
		resp := ts.uploadFile(t, names[i], "batch", content)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload %d: expected 200, got %d", i, resp.StatusCode)
		}
		f := decodeJSON[openai.FileObject](t, resp)
		fileIDs[f.ID] = true
	}

	// List all files
	resp := ts.doRequest(t, http.MethodGet, "/v1/files")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", resp.StatusCode)
	}
	listResp := decodeJSON[openai.ListFilesResponse](t, resp)

	if listResp.Object != "list" {
		t.Errorf("list: expected object=list, got %q", listResp.Object)
	}
	if len(listResp.Data) < 3 {
		t.Errorf("list: expected at least 3 files, got %d", len(listResp.Data))
	}

	for id := range fileIDs {
		found := false
		for _, f := range listResp.Data {
			if f.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("list: file %q not found in response", id)
		}
	}
}

func TestFileDelete(t *testing.T) {
	ts := newTestServer(t)

	content := `{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{}}` + "\n"
	resp := ts.uploadFile(t, "to-delete.jsonl", "batch", content)
	fileObj := decodeJSON[openai.FileObject](t, resp)

	// Delete
	resp = ts.doRequest(t, http.MethodDelete, "/v1/files/"+fileObj.ID)
	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp)
		t.Fatalf("delete: expected 200, got %d: %s", resp.StatusCode, body)
	}
	deleteResp := decodeJSON[openai.FileDeleteResponse](t, resp)

	if deleteResp.ID != fileObj.ID {
		t.Errorf("delete: expected ID=%q, got %q", fileObj.ID, deleteResp.ID)
	}
	if !deleteResp.Deleted {
		t.Error("delete: expected deleted=true")
	}

	// Verify retrieve returns 404
	resp = ts.doRequest(t, http.MethodGet, "/v1/files/"+fileObj.ID)
	if resp.StatusCode != http.StatusNotFound {
		body := readBody(t, resp)
		t.Fatalf("retrieve after delete: expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Verify download returns 404
	resp = ts.doRequest(t, http.MethodGet, "/v1/files/"+fileObj.ID+"/content")
	if resp.StatusCode != http.StatusNotFound {
		body := readBody(t, resp)
		t.Fatalf("download after delete: expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestFileUploadTooManyLines(t *testing.T) {
	ts := newTestServer(t)

	var sb strings.Builder
	line := `{"custom_id":"req","method":"POST","url":"/v1/chat/completions","body":{}}` + "\n"
	for range 50001 {
		sb.WriteString(line)
	}

	resp := ts.uploadFile(t, "too-many-lines.jsonl", "batch", sb.String())
	if resp.StatusCode != http.StatusBadRequest {
		body := readBody(t, resp)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

func TestFileDeleteNonExistent(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.doRequest(t, http.MethodDelete, "/v1/files/file-does-not-exist")
	if resp.StatusCode != http.StatusNotFound {
		body := readBody(t, resp)
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()
}

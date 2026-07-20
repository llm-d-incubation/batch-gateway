package benchmark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/go-logr/logr"

	db "github.com/llm-d/llm-d-batch-gateway/internal/database/api"
	mockdb "github.com/llm-d/llm-d-batch-gateway/internal/database/mock"
	filesapi "github.com/llm-d/llm-d-batch-gateway/internal/files_store/api"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	ucom "github.com/llm-d/llm-d-batch-gateway/internal/util/com"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// MockInferenceClient implements inference.InferenceClient with a fixed response.
type MockInferenceClient struct{}

func (m *MockInferenceClient) Generate(_ context.Context, _ *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return &inference.GenerateResponse{
		RequestID: "bench-req-1",
		Response:  []byte(`{"choices":[{"message":{"content":"ok"}}]}`),
	}, nil
}

// GenJSONLInput generates count JSONL request lines cycling through models.
func GenJSONLInput(count int, models []string) []byte {
	var buf bytes.Buffer
	for i := range count {
		m := models[i%len(models)]
		req := map[string]any{
			"custom_id": fmt.Sprintf("req-%d", i),
			"method":    "POST",
			"url":       "/v1/chat/completions",
			"body": map[string]any{
				"model": m,
				"messages": []map[string]string{
					{"role": "system", "content": fmt.Sprintf("system-prompt-%d", i%10)},
					{"role": "user", "content": fmt.Sprintf("question %d", i)},
				},
			},
		}
		line, _ := json.Marshal(req)
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// SeedInputFile stores input data in the files client and seeds a FileDB record.
func SeedInputFile(b *testing.B, filesClient filesapi.BatchFilesClient, fileDBClient db.FileDBClient, inputFileID, tenantID string, data []byte) {
	b.Helper()
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		b.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	filename := "input.jsonl"
	storageName := ucom.FileStorageName(inputFileID, filename)
	if _, err := filesClient.Store(context.Background(), storageName, folder, 0, 0, bytes.NewReader(data)); err != nil {
		b.Fatalf("files.Store: %v", err)
	}
	fileSpec := &openai.FileObject{Filename: filename}
	specBytes, _ := json.Marshal(fileSpec)
	fileItem := &db.FileItem{
		BaseIndexes:  db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{Spec: specBytes},
	}
	if err := fileDBClient.DBStore(context.Background(), fileItem); err != nil {
		b.Fatalf("DBStore file item: %v", err)
	}
}

// NewMockBatchDBClient creates a mock BatchDBClient.
func NewMockBatchDBClient() db.BatchDBClient {
	return mockdb.NewMockDBClient[db.BatchItem, db.BatchQuery](
		func(b *db.BatchItem) string { return b.ID },
		func(q *db.BatchQuery) *db.BaseQuery { return &q.BaseQuery },
	)
}

// NewMockFileDBClient creates a mock FileDBClient.
func NewMockFileDBClient() db.FileDBClient {
	return mockdb.NewMockDBClient[db.FileItem, db.FileQuery](
		func(f *db.FileItem) string { return f.ID },
		func(q *db.FileQuery) *db.BaseQuery { return &q.BaseQuery },
	)
}

// DiscardLogger returns a logr.Logger that discards all output.
func DiscardLogger() logr.Logger {
	return logr.Discard()
}

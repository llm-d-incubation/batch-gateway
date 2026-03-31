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

// Integration tests for the S3 client using a real S3-compatible service (e.g. MinIO).
// These tests are skipped when S3_TEST_ENDPOINT is not set.
//
// Option 1: Standalone MinIO via Docker
//
//   docker run -d --name minio -p 9000:9000 \
//     -e MINIO_ROOT_USER=minioadmin \
//     -e MINIO_ROOT_PASSWORD=minioadmin \
//     minio/minio server /data
//
//   S3_TEST_ENDPOINT=http://localhost:9000 go test -v -run TestIntegration ./internal/files_store/s3/
//
// Option 2: After "make dev-deploy" (MinIO is exposed on localhost:9002)
//
//   S3_TEST_ENDPOINT=http://localhost:9002 go test -v -run TestIntegration ./internal/files_store/s3/

package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
)

const (
	integrationBucket = "integration-test"
)

func newIntegrationClient(t *testing.T) *Client {
	t.Helper()

	endpoint := os.Getenv("S3_TEST_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_TEST_ENDPOINT not set, skipping integration test")
	}

	accessKey := os.Getenv("S3_TEST_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("S3_TEST_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}

	client, err := New(context.Background(), Config{
		Region:           "us-east-1",
		Endpoint:         endpoint,
		AccessKeyID:      accessKey,
		SecretAccessKey:  secretKey,
		UsePathStyle:     true,
		AutoCreateBucket: true,
	})
	if err != nil {
		t.Fatalf("failed to create S3 client: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestIntegrationStoreAndRetrieve(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()
	content := []byte("hello integration test\nline2\nline3\n")
	fileName := fmt.Sprintf("test-store-retrieve-%s-%s", t.Name(), uuid.NewString()[:8])

	// Store
	md, err := client.Store(ctx, fileName, integrationBucket, 1024, 0, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Delete(ctx, fileName, integrationBucket) })

	if md.Size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), md.Size)
	}
	if md.LinesNumber != 3 {
		t.Errorf("expected 3 lines, got %d", md.LinesNumber)
	}

	// Retrieve
	reader, md2, err := client.Retrieve(ctx, fileName, integrationBucket)
	if err != nil {
		t.Fatalf("Retrieve failed: %v", err)
	}
	defer reader.Close()

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if !bytes.Equal(data, content) {
		t.Errorf("content mismatch: got %q, want %q", data, content)
	}
	if md2.Size != int64(len(content)) {
		t.Errorf("expected size %d, got %d", len(content), md2.Size)
	}
}

func TestIntegrationStoreExistingFile(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()
	fileName := fmt.Sprintf("test-existing-%s-%s", t.Name(), uuid.NewString()[:8])

	_, err := client.Store(ctx, fileName, integrationBucket, 1024, 0, bytes.NewReader([]byte("first")))
	if err != nil {
		t.Fatalf("first Store failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Delete(ctx, fileName, integrationBucket) })

	_, err = client.Store(ctx, fileName, integrationBucket, 1024, 0, bytes.NewReader([]byte("second")))
	if !errors.Is(err, api.ErrFileExists) {
		t.Errorf("expected ErrFileExists, got %v", err)
	}
}

func TestIntegrationStoreFileTooLarge(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()
	fileName := fmt.Sprintf("test-toolarge-%s-%s", t.Name(), uuid.NewString()[:8])

	_, err := client.Store(ctx, fileName, integrationBucket, 5, 0, bytes.NewReader([]byte("this is too large")))
	if !errors.Is(err, api.ErrFileTooLarge) {
		t.Errorf("expected ErrFileTooLarge, got %v", err)
	}

	// Verify the rejected file did not leave a partial object in storage.
	_, _, err = client.Retrieve(ctx, fileName, integrationBucket)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected rejected file to not exist in storage, got: %v", err)
	}
}

func TestIntegrationStoreTooManyLines(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()
	fileName := fmt.Sprintf("test-toomanylines-%s-%s", t.Name(), uuid.NewString()[:8])

	_, err := client.Store(ctx, fileName, integrationBucket, 1024, 2, bytes.NewReader([]byte("l1\nl2\nl3\n")))
	if !errors.Is(err, api.ErrTooManyLines) {
		t.Errorf("expected ErrTooManyLines, got %v", err)
	}

	// Verify the rejected file did not leave a partial object in storage.
	_, _, err = client.Retrieve(ctx, fileName, integrationBucket)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected rejected file to not exist in storage, got: %v", err)
	}
}

func TestIntegrationDelete(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()
	fileName := fmt.Sprintf("test-delete-%s-%s", t.Name(), uuid.NewString()[:8])

	_, err := client.Store(ctx, fileName, integrationBucket, 1024, 0, bytes.NewReader([]byte("delete me")))
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	// No t.Cleanup here — the test itself verifies deletion.

	err = client.Delete(ctx, fileName, integrationBucket)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, _, err = client.Retrieve(ctx, fileName, integrationBucket)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist after delete, got %v", err)
	}
}

func TestIntegrationDeleteNonExistent(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()

	// Deleting a file that was never created should succeed (S3 DeleteObject is idempotent).
	// The file delete API handler relies on this behavior to tolerate retries and
	// races where the blob has already been removed.
	fileName := fmt.Sprintf("test-delete-nonexistent-%s-%s", t.Name(), uuid.NewString()[:8])
	err := client.Delete(ctx, fileName, integrationBucket)
	if err != nil {
		t.Fatalf("Delete of non-existent file should succeed (S3 idempotent delete), got: %v", err)
	}
}

func TestIntegrationRetrieveNonExistent(t *testing.T) {
	client := newIntegrationClient(t)
	ctx := context.Background()

	_, _, err := client.Retrieve(ctx, "nonexistent-file-xyz", integrationBucket)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

func TestIntegrationPrefix(t *testing.T) {
	client := newIntegrationClient(t)
	client.prefix = "testprefix"
	ctx := context.Background()
	fileName := fmt.Sprintf("test-prefix-%s-%s", t.Name(), uuid.NewString()[:8])

	md, err := client.Store(ctx, fileName, integrationBucket, 1024, 0, bytes.NewReader([]byte("prefixed")))
	if err != nil {
		t.Fatalf("Store failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Delete(ctx, fileName, integrationBucket) })

	expectedLocation := "testprefix/" + fileName
	if md.Location != expectedLocation {
		t.Errorf("expected location %s, got %s", expectedLocation, md.Location)
	}

	reader, _, err := client.Retrieve(ctx, fileName, integrationBucket)
	if err != nil {
		t.Fatalf("Retrieve failed: %v", err)
	}
	defer reader.Close()

	data, _ := io.ReadAll(reader)
	if string(data) != "prefixed" {
		t.Errorf("expected 'prefixed', got %q", data)
	}
}

func TestIntegrationGetContext(t *testing.T) {
	client := newIntegrationClient(t)

	ctx, cancel := client.GetContext(context.Background(), 5*time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context to have deadline")
	}

	remaining := time.Until(deadline)
	if remaining < 4*time.Second || remaining > 6*time.Second {
		t.Errorf("expected ~5s remaining, got %v", remaining)
	}
}

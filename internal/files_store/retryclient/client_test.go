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

package retryclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	"github.com/llm-d-incubation/batch-gateway/internal/util/retry"
)

type mockFilesClient struct {
	storeCalls    int
	retrieveCalls int
	deleteCalls   int
	failUntil     int
}

func (m *mockFilesClient) Store(_ context.Context, _, _ string, _, _ int64, _ io.Reader) (*api.BatchFileMetadata, error) {
	m.storeCalls++
	if m.storeCalls <= m.failUntil {
		return nil, errors.New("transient store error")
	}
	return &api.BatchFileMetadata{Size: 100}, nil
}

func (m *mockFilesClient) Retrieve(_ context.Context, _, _ string) (io.ReadCloser, *api.BatchFileMetadata, error) {
	m.retrieveCalls++
	if m.retrieveCalls <= m.failUntil {
		return nil, nil, errors.New("transient retrieve error")
	}
	return io.NopCloser(bytes.NewReader([]byte("data"))), &api.BatchFileMetadata{Size: 4}, nil
}

func (m *mockFilesClient) Delete(_ context.Context, _, _ string) error {
	m.deleteCalls++
	if m.deleteCalls <= m.failUntil {
		return errors.New("transient delete error")
	}
	return nil
}

func (m *mockFilesClient) Close() error { return nil }

func retryCfg() retry.Config {
	return retry.Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}
}

type nonSeekableReader struct {
	data []byte
	pos  int
}

func (r *nonSeekableReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

type failingSeekerReader struct {
	data      []byte
	pos       int
	seekCalls int
}

func (r *failingSeekerReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *failingSeekerReader) Seek(offset int64, whence int) (int64, error) {
	r.seekCalls++
	return 0, errors.New("seek failed")
}

func TestStore_SucceedsAfterRetry(t *testing.T) {
	mock := &mockFilesClient{failUntil: 2}
	c := New(mock, retryCfg())

	meta, err := c.Store(context.Background(), "f.txt", "folder", 0, 0, bytes.NewReader([]byte("data")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Size != 100 {
		t.Fatalf("expected size 100, got %d", meta.Size)
	}
	if mock.storeCalls != 3 {
		t.Fatalf("expected 3 store calls, got %d", mock.storeCalls)
	}
}

func TestStore_ExhaustsRetries(t *testing.T) {
	mock := &mockFilesClient{failUntil: 10}
	c := New(mock, retryCfg())

	_, err := c.Store(context.Background(), "f.txt", "folder", 0, 0, bytes.NewReader([]byte("data")))
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if mock.storeCalls != 4 { // 1 initial + 3 retries
		t.Fatalf("expected 4 store calls, got %d", mock.storeCalls)
	}
}

func TestStore_NoRetryOnZeroConfig(t *testing.T) {
	mock := &mockFilesClient{failUntil: 1}
	c := New(mock, retry.Config{MaxRetries: 0})

	_, err := c.Store(context.Background(), "f.txt", "folder", 0, 0, bytes.NewReader([]byte("data")))
	if err == nil {
		t.Fatal("expected error")
	}
	if mock.storeCalls != 1 {
		t.Fatalf("expected 1 store call, got %d", mock.storeCalls)
	}
}

func TestStore_NonSeekableReaderDoesNotRetry(t *testing.T) {
	mock := &mockFilesClient{failUntil: 10}
	c := New(mock, retryCfg())

	reader := &nonSeekableReader{data: []byte("data")}
	_, err := c.Store(context.Background(), "f.txt", "folder", 0, 0, reader)
	if err == nil {
		t.Fatal("expected error")
	}
	if mock.storeCalls != 1 {
		t.Fatalf("expected 1 store call for non-seekable reader, got %d", mock.storeCalls)
	}
}

func TestStore_SeekFailureAbortsImmediately(t *testing.T) {
	mock := &mockFilesClient{failUntil: 1}
	c := New(mock, retryCfg())

	reader := &failingSeekerReader{data: []byte("data")}
	_, err := c.Store(context.Background(), "f.txt", "folder", 0, 0, reader)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "seek failed") {
		t.Fatalf("expected seek failure error, got %v", err)
	}
	if mock.storeCalls != 1 {
		t.Fatalf("expected abort after first failed store, got %d calls", mock.storeCalls)
	}
	if reader.seekCalls != 1 {
		t.Fatalf("expected 1 seek attempt, got %d", reader.seekCalls)
	}
}

func TestRetrieve_SucceedsAfterRetry(t *testing.T) {
	mock := &mockFilesClient{failUntil: 1}
	c := New(mock, retryCfg())

	rc, meta, err := c.Retrieve(context.Background(), "f.txt", "folder")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer rc.Close()
	if meta.Size != 4 {
		t.Fatalf("expected size 4, got %d", meta.Size)
	}
	if mock.retrieveCalls != 2 {
		t.Fatalf("expected 2 retrieve calls, got %d", mock.retrieveCalls)
	}
}

func TestDelete_SucceedsAfterRetry(t *testing.T) {
	mock := &mockFilesClient{failUntil: 2}
	c := New(mock, retryCfg())

	err := c.Delete(context.Background(), "f.txt", "folder")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.deleteCalls != 3 {
		t.Fatalf("expected 3 delete calls, got %d", mock.deleteCalls)
	}
}

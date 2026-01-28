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

package fs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Run("creates client with valid path", func(t *testing.T) {
		tmpDir := t.TempDir()
		basePath := filepath.Join(tmpDir, "storage")

		client, err := New(basePath)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if client == nil {
			t.Fatal("expected client to be non-nil")
		}

		// Verify directory was created.
		if _, err := os.Stat(basePath); os.IsNotExist(err) {
			t.Error("expected base directory to be created")
		}
	})

	t.Run("uses default timeout", func(t *testing.T) {
		client, err := New(t.TempDir())
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if client.defaultTimeout != DefaultTimeout {
			t.Errorf("expected default timeout %v, got %v", DefaultTimeout, client.defaultTimeout)
		}
	})
}

func TestSetDefaultTimeout(t *testing.T) {
	client, _ := New(t.TempDir())
	newTimeout := 60 * time.Second

	client.SetDefaultTimeout(newTimeout)

	if client.defaultTimeout != newTimeout {
		t.Errorf("expected timeout %v, got %v", newTimeout, client.defaultTimeout)
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()

	t.Run("stores file successfully", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("hello world")

		md, err := client.Store(ctx, "test.txt", 1024, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if md.Size != int64(len(content)) {
			t.Errorf("expected size %d, got %d", len(content), md.Size)
		}

		// Verify file content.
		data, _ := os.ReadFile(md.Location)
		if !bytes.Equal(data, content) {
			t.Errorf("expected content %q, got %q", content, data)
		}
	})

	t.Run("creates nested directories", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("nested content")

		md, err := client.Store(ctx, "a/b/c/file.txt", 1024, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if !strings.Contains(md.Location, filepath.Join("a", "b", "c")) {
			t.Errorf("expected nested path, got %s", md.Location)
		}
	})

	t.Run("returns error for file too large", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("this content is too large")

		_, err := client.Store(ctx, "large.txt", 5, bytes.NewReader(content))
		if !errors.Is(err, ErrFileTooLarge) {
			t.Errorf("expected ErrFileTooLarge, got %v", err)
		}

		// Verify file was not created.
		fullPath := filepath.Join(client.basePath, "large.txt")
		if _, err := os.Stat(fullPath); !os.IsNotExist(err) {
			t.Error("expected file to not exist after size limit exceeded")
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("malicious")

		_, err := client.Store(ctx, "../escape.txt", 1024, bytes.NewReader(content))
		if !errors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})

	t.Run("stores file at exact size limit", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("12345")

		md, err := client.Store(ctx, "exact.txt", 5, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if md.Size != 5 {
			t.Errorf("expected size 5, got %d", md.Size)
		}
	})

	t.Run("returns error for existing file", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("original content")

		// Store first file.
		_, err := client.Store(ctx, "existing.txt", 1024, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("expected no error on first store, got %v", err)
		}

		// Attempt to store again with same location.
		_, err = client.Store(ctx, "existing.txt", 1024, bytes.NewReader([]byte("new content")))
		if !errors.Is(err, ErrFileExists) {
			t.Errorf("expected ErrFileExists, got %v", err)
		}

		// Verify original content is unchanged.
		reader, _, _ := client.Retrieve(ctx, "existing.txt")
		defer func() {
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
		}()
		data, _ := io.ReadAll(reader)
		if !bytes.Equal(data, content) {
			t.Errorf("expected original content to be unchanged")
		}
	})
}

func TestRetrieve(t *testing.T) {
	ctx := context.Background()

	t.Run("retrieves existing file", func(t *testing.T) {
		client, _ := New(t.TempDir())
		content := []byte("retrieve me")

		// Store first.
		_, err := client.Store(ctx, "retrieve.txt", 1024, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("failed to store: %v", err)
		}

		// Retrieve.
		reader, md, err := client.Retrieve(ctx, "retrieve.txt")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		defer func() {
			if closer, ok := reader.(io.Closer); ok {
				_ = closer.Close()
			}
		}()

		if md.Size != int64(len(content)) {
			t.Errorf("expected size %d, got %d", len(content), md.Size)
		}

		data, _ := io.ReadAll(reader)
		if !bytes.Equal(data, content) {
			t.Errorf("expected content %q, got %q", content, data)
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		client, _ := New(t.TempDir())

		_, _, err := client.Retrieve(ctx, "nonexistent.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected os.ErrNotExist, got %v", err)
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		client, _ := New(t.TempDir())

		_, _, err := client.Retrieve(ctx, "../escape.txt")
		if !errors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})
}

func TestList(t *testing.T) {
	ctx := context.Background()

	t.Run("lists matching files", func(t *testing.T) {
		client, _ := New(t.TempDir())

		// Create test files.
		_, _ = client.Store(ctx, "file1.txt", 1024, bytes.NewReader([]byte("1")))
		_, _ = client.Store(ctx, "file2.txt", 1024, bytes.NewReader([]byte("2")))
		_, _ = client.Store(ctx, "other.log", 1024, bytes.NewReader([]byte("3")))

		files, err := client.List(ctx, "*.txt")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(files) != 2 {
			t.Errorf("expected 2 files, got %d", len(files))
		}
	})

	t.Run("returns empty list for no matches", func(t *testing.T) {
		client, _ := New(t.TempDir())

		files, err := client.List(ctx, "*.xyz")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(files) != 0 {
			t.Errorf("expected 0 files, got %d", len(files))
		}
	})

	t.Run("excludes directories", func(t *testing.T) {
		client, _ := New(t.TempDir())

		// Create a file in a subdirectory.
		_, _ = client.Store(ctx, "subdir/file.txt", 1024, bytes.NewReader([]byte("content")))

		// List with pattern that matches directory.
		files, err := client.List(ctx, "*")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		for _, f := range files {
			if strings.HasSuffix(f.Location, "subdir") {
				t.Error("expected directories to be excluded")
			}
		}
	})
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes existing file", func(t *testing.T) {
		client, _ := New(t.TempDir())
		_, _ = client.Store(ctx, "delete.txt", 1024, bytes.NewReader([]byte("delete me")))

		err := client.Delete(ctx, "delete.txt")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		// Verify file is gone.
		_, _, err = client.Retrieve(ctx, "delete.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Error("expected file to be deleted")
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		client, _ := New(t.TempDir())

		err := client.Delete(ctx, "nonexistent.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected os.ErrNotExist, got %v", err)
		}
	})

	t.Run("rejects path traversal", func(t *testing.T) {
		client, _ := New(t.TempDir())

		err := client.Delete(ctx, "../escape.txt")
		if !errors.Is(err, os.ErrInvalid) {
			t.Errorf("expected os.ErrInvalid, got %v", err)
		}
	})
}

func TestGetContext(t *testing.T) {
	t.Run("uses default timeout when zero", func(t *testing.T) {
		client, _ := New(t.TempDir())

		ctx, cancel := client.GetContext(context.Background(), 0)
		defer cancel()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected context to have deadline")
		}

		// Deadline should be approximately defaultTimeout from now.
		expectedDeadline := time.Now().Add(DefaultTimeout)
		if deadline.Before(expectedDeadline.Add(-time.Second)) || deadline.After(expectedDeadline.Add(time.Second)) {
			t.Errorf("deadline %v not within expected range around %v", deadline, expectedDeadline)
		}
	})

	t.Run("uses provided timeout", func(t *testing.T) {
		client, _ := New(t.TempDir())
		customTimeout := 5 * time.Second

		ctx, cancel := client.GetContext(context.Background(), customTimeout)
		defer cancel()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected context to have deadline")
		}

		expectedDeadline := time.Now().Add(customTimeout)
		if deadline.Before(expectedDeadline.Add(-time.Second)) || deadline.After(expectedDeadline.Add(time.Second)) {
			t.Errorf("deadline %v not within expected range around %v", deadline, expectedDeadline)
		}
	})
}

func TestClose(t *testing.T) {
	client, _ := New(t.TempDir())

	err := client.Close()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestResolvePath(t *testing.T) {
	client, _ := New(t.TempDir())

	tests := []struct {
		name      string
		location  string
		wantError bool
	}{
		{"simple path", "file.txt", false},
		{"nested path", "a/b/c/file.txt", false},
		{"current dir", ".", false},
		{"parent traversal", "../file.txt", true},
		{"hidden traversal", "a/../../../file.txt", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.resolvePath(tt.location)
			if tt.wantError && err == nil {
				t.Error("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("expected no error, got %v", err)
			}
		})
	}
}

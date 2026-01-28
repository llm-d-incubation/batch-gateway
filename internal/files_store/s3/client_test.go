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

package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type mockS3Client struct {
	objects map[string]mockObject
	putErr  error
	getErr  error
	headErr error
	delErr  error
	listErr error
}

type mockObject struct {
	data        []byte
	lastModTime time.Time
}

func newMockS3Client() *mockS3Client {
	return &mockS3Client{
		objects: make(map[string]mockObject),
	}
}

func (m *mockS3Client) PutObject(_ context.Context, params *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if m.putErr != nil {
		return nil, m.putErr
	}
	data, _ := io.ReadAll(params.Body)
	m.objects[*params.Key] = mockObject{
		data:        data,
		lastModTime: time.Now(),
	}
	return &s3.PutObjectOutput{}, nil
}

func (m *mockS3Client) GetObject(_ context.Context, params *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	obj, ok := m.objects[*params.Key]
	if !ok {
		return nil, &types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(obj.data)),
		ContentLength: aws.Int64(int64(len(obj.data))),
		LastModified:  aws.Time(obj.lastModTime),
	}, nil
}

func (m *mockS3Client) HeadObject(_ context.Context, params *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if m.headErr != nil {
		return nil, m.headErr
	}
	obj, ok := m.objects[*params.Key]
	if !ok {
		return nil, &types.NotFound{}
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(obj.data))),
		LastModified:  aws.Time(obj.lastModTime),
	}, nil
}

func (m *mockS3Client) DeleteObject(_ context.Context, params *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if m.delErr != nil {
		return nil, m.delErr
	}
	delete(m.objects, *params.Key)
	return &s3.DeleteObjectOutput{}, nil
}

func (m *mockS3Client) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	prefix := aws.ToString(params.Prefix)
	var contents []types.Object
	for key, obj := range m.objects {
		if len(prefix) == 0 || (len(key) >= len(prefix) && key[:len(prefix)] == prefix) {
			contents = append(contents, types.Object{
				Key:          aws.String(key),
				Size:         aws.Int64(int64(len(obj.data))),
				LastModified: aws.Time(obj.lastModTime),
			})
		}
	}
	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: aws.Bool(false),
	}, nil
}

func newTestClient(mock *mockS3Client) *Client {
	return &Client{
		s3Client:       mock,
		bucket:         "test-bucket",
		prefix:         "",
		defaultTimeout: DefaultTimeout,
	}
}

func TestStore(t *testing.T) {
	ctx := context.Background()

	t.Run("stores file successfully", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
		content := []byte("hello world")

		md, err := client.Store(ctx, "test.txt", 1024, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if md.Size != int64(len(content)) {
			t.Errorf("expected size %d, got %d", len(content), md.Size)
		}
		if md.Location != "test.txt" {
			t.Errorf("expected location test.txt, got %s", md.Location)
		}

		obj, ok := mock.objects["test.txt"]
		if !ok {
			t.Fatal("expected object to be stored")
		}
		if !bytes.Equal(obj.data, content) {
			t.Errorf("expected content %q, got %q", content, obj.data)
		}
	})

	t.Run("returns error for file too large", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
		content := []byte("this content is too large")

		_, err := client.Store(ctx, "large.txt", 5, bytes.NewReader(content))
		if !errors.Is(err, ErrFileTooLarge) {
			t.Errorf("expected ErrFileTooLarge, got %v", err)
		}

		if _, ok := mock.objects["large.txt"]; ok {
			t.Error("expected object not to be stored")
		}
	})

	t.Run("stores file at exact size limit", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
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
		mock := newMockS3Client()
		client := newTestClient(mock)

		_, err := client.Store(ctx, "existing.txt", 1024, bytes.NewReader([]byte("original")))
		if err != nil {
			t.Fatalf("expected no error on first store, got %v", err)
		}

		_, err = client.Store(ctx, "existing.txt", 1024, bytes.NewReader([]byte("new content")))
		if !errors.Is(err, ErrFileExists) {
			t.Errorf("expected ErrFileExists, got %v", err)
		}
	})

	t.Run("uses prefix", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
		client.prefix = "myprefix"

		md, err := client.Store(ctx, "test.txt", 1024, bytes.NewReader([]byte("content")))
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if md.Location != "myprefix/test.txt" {
			t.Errorf("expected location myprefix/test.txt, got %s", md.Location)
		}

		if _, ok := mock.objects["myprefix/test.txt"]; !ok {
			t.Error("expected object to be stored with prefix")
		}
	})
}

func TestRetrieve(t *testing.T) {
	ctx := context.Background()

	t.Run("retrieves existing file", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
		content := []byte("retrieve me")

		_, err := client.Store(ctx, "retrieve.txt", 1024, bytes.NewReader(content))
		if err != nil {
			t.Fatalf("failed to store: %v", err)
		}

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
		mock := newMockS3Client()
		client := newTestClient(mock)

		_, _, err := client.Retrieve(ctx, "nonexistent.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected os.ErrNotExist, got %v", err)
		}
	})
}

func TestList(t *testing.T) {
	ctx := context.Background()

	t.Run("lists matching files", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)

		_, _ = client.Store(ctx, "files/file1.txt", 1024, bytes.NewReader([]byte("1")))
		_, _ = client.Store(ctx, "files/file2.txt", 1024, bytes.NewReader([]byte("2")))
		_, _ = client.Store(ctx, "other/file3.txt", 1024, bytes.NewReader([]byte("3")))

		files, err := client.List(ctx, "files/")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(files) != 2 {
			t.Errorf("expected 2 files, got %d", len(files))
		}
	})

	t.Run("returns empty list for no matches", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)

		files, err := client.List(ctx, "nonexistent/")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(files) != 0 {
			t.Errorf("expected 0 files, got %d", len(files))
		}
	})

	t.Run("uses prefix", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
		client.prefix = "myprefix"

		_, _ = client.Store(ctx, "file.txt", 1024, bytes.NewReader([]byte("content")))

		files, err := client.List(ctx, "")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if len(files) != 1 {
			t.Errorf("expected 1 file, got %d", len(files))
		}
		if len(files) > 0 && files[0].Location != "myprefix/file.txt" {
			t.Errorf("expected location myprefix/file.txt, got %s", files[0].Location)
		}
	})
}

func TestDelete(t *testing.T) {
	ctx := context.Background()

	t.Run("deletes existing file", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
		_, _ = client.Store(ctx, "delete.txt", 1024, bytes.NewReader([]byte("delete me")))

		err := client.Delete(ctx, "delete.txt")
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}

		_, _, err = client.Retrieve(ctx, "delete.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Error("expected file to be deleted")
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)

		err := client.Delete(ctx, "nonexistent.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected os.ErrNotExist, got %v", err)
		}
	})
}

func TestGetContext(t *testing.T) {
	t.Run("uses default timeout when zero", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)

		ctx, cancel := client.GetContext(context.Background(), 0)
		defer cancel()

		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected context to have deadline")
		}

		expectedDeadline := time.Now().Add(DefaultTimeout)
		if deadline.Before(expectedDeadline.Add(-time.Second)) || deadline.After(expectedDeadline.Add(time.Second)) {
			t.Errorf("deadline %v not within expected range around %v", deadline, expectedDeadline)
		}
	})

	t.Run("uses provided timeout", func(t *testing.T) {
		mock := newMockS3Client()
		client := newTestClient(mock)
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

func TestSetDefaultTimeout(t *testing.T) {
	mock := newMockS3Client()
	client := newTestClient(mock)
	newTimeout := 60 * time.Second

	client.SetDefaultTimeout(newTimeout)

	if client.defaultTimeout != newTimeout {
		t.Errorf("expected timeout %v, got %v", newTimeout, client.defaultTimeout)
	}
}

func TestClose(t *testing.T) {
	mock := newMockS3Client()
	client := newTestClient(mock)

	err := client.Close()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestResolveKey(t *testing.T) {
	tests := []struct {
		name     string
		prefix   string
		location string
		expected string
	}{
		{"no prefix", "", "file.txt", "file.txt"},
		{"with prefix", "myprefix", "file.txt", "myprefix/file.txt"},
		{"nested location", "prefix", "a/b/file.txt", "prefix/a/b/file.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &Client{prefix: tt.prefix}
			result := client.resolveKey(tt.location)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

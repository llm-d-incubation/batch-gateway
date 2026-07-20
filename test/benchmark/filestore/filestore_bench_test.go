package filestore_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/llm-d/llm-d-batch-gateway/internal/files_store/fs"
)

func BenchmarkFSStore(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"500MB", 500 * 1024 * 1024},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchStore(b, tc.size)
		})
	}
}

func benchStore(b *testing.B, size int) {
	b.Helper()
	client, err := fs.New(b.TempDir())
	if err != nil {
		b.Fatalf("fs.New: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), size)
	folder := "bench-folder"

	b.ResetTimer()
	start := time.Now()

	for i := range b.N {
		name := fmt.Sprintf("bench-store-%d", i)
		if _, err := client.Store(ctx, name, folder, 0, 0, bytes.NewReader(data)); err != nil {
			b.Fatalf("Store: %v", err)
		}
	}

	b.StopTimer()
	elapsed := time.Since(start)
	b.ReportMetric(float64(size)*float64(b.N)/elapsed.Seconds()/(1024*1024), "MB/sec")
}

func BenchmarkFSRetrieve(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"500MB", 500 * 1024 * 1024},
	} {
		b.Run(tc.name, func(b *testing.B) {
			benchRetrieve(b, tc.size)
		})
	}
}

func benchRetrieve(b *testing.B, size int) {
	b.Helper()
	client, err := fs.New(b.TempDir())
	if err != nil {
		b.Fatalf("fs.New: %v", err)
	}
	defer func() { _ = client.Close() }()

	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), size)
	folder := "bench-folder"
	fileName := "bench-retrieve-file"

	if _, err := client.Store(ctx, fileName, folder, 0, 0, bytes.NewReader(data)); err != nil {
		b.Fatalf("Store (setup): %v", err)
	}

	b.ResetTimer()
	start := time.Now()

	for range b.N {
		reader, _, err := client.Retrieve(ctx, fileName, folder)
		if err != nil {
			b.Fatalf("Retrieve: %v", err)
		}
		if _, err := io.Copy(io.Discard, reader); err != nil {
			reader.Close()
			b.Fatalf("io.Copy: %v", err)
		}
		reader.Close()
	}

	b.StopTimer()
	elapsed := time.Since(start)
	b.ReportMetric(float64(size)*float64(b.N)/elapsed.Seconds()/(1024*1024), "MB/sec")
}

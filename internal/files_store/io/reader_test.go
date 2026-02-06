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

package io

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
)

func TestLimitedCountingReader(t *testing.T) {
	t.Run("reads within limit", func(t *testing.T) {
		data := []byte("hello world")
		reader := &LimitedCountingReader{
			Reader:    bytes.NewReader(data),
			SizeLimit: 100,
		}

		buf := make([]byte, 5)
		n, err := reader.Read(buf)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if n != 5 {
			t.Errorf("expected 5 bytes, got %d", n)
		}
		if reader.BytesRead != 5 {
			t.Errorf("expected BytesRead 5, got %d", reader.BytesRead)
		}

		// Read the rest
		rest, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if reader.BytesRead != int64(len(data)) {
			t.Errorf("expected BytesRead %d, got %d", len(data), reader.BytesRead)
		}
		if string(buf)+string(rest) != string(data) {
			t.Errorf("expected %q, got %q", data, string(buf)+string(rest))
		}
	})

	t.Run("returns error when exceeding limit", func(t *testing.T) {
		data := []byte("this is too long")
		reader := &LimitedCountingReader{
			Reader:    bytes.NewReader(data),
			SizeLimit: 5,
		}

		buf := make([]byte, 10)
		_, err := reader.Read(buf)
		if !errors.Is(err, api.ErrFileTooLarge) {
			t.Errorf("expected ErrFileTooLarge, got %v", err)
		}
	})

	t.Run("allows reading exactly at limit", func(t *testing.T) {
		data := []byte("12345")
		reader := &LimitedCountingReader{
			Reader:    bytes.NewReader(data),
			SizeLimit: 5,
		}

		result, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if string(result) != string(data) {
			t.Errorf("expected %q, got %q", data, result)
		}
		if reader.BytesRead != 5 {
			t.Errorf("expected BytesRead 5, got %d", reader.BytesRead)
		}
	})

	t.Run("tracks bytes across multiple reads", func(t *testing.T) {
		data := []byte("abcdefghij")
		reader := &LimitedCountingReader{
			Reader:    bytes.NewReader(data),
			SizeLimit: 100,
		}

		buf := make([]byte, 3)
		for i := 0; i < 3; i++ {
			_, err := reader.Read(buf)
			if err != nil {
				t.Fatalf("read %d: expected no error, got %v", i, err)
			}
		}

		if reader.BytesRead != 9 {
			t.Errorf("expected BytesRead 9, got %d", reader.BytesRead)
		}
	})

	t.Run("counts lines correctly", func(t *testing.T) {
		data := []byte("line1\nline2\nline3\n")
		reader := &LimitedCountingReader{
			Reader:    bytes.NewReader(data),
			SizeLimit: 100,
			LineLimit: 10,
		}

		_, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		if reader.LineCount != 3 {
			t.Errorf("expected LineCount 3, got %d", reader.LineCount)
		}
	})

	t.Run("returns error when exceeding line limit", func(t *testing.T) {
		data := []byte("line1\nline2\nline3\n")
		reader := &LimitedCountingReader{
			Reader:    bytes.NewReader(data),
			SizeLimit: 100,
			LineLimit: 2,
		}

		_, err := io.ReadAll(reader)
		if !errors.Is(err, api.ErrTooManyLines) {
			t.Errorf("expected ErrTooManyLines, got %v", err)
		}
	})
}

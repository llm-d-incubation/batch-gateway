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

// Package io provides I/O utilities for file storage operations.
package io

import (
	"io"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
)

// LimitedCountingReader wraps a reader to count bytes and lines while enforcing limits.
// It returns api.ErrFileTooLarge if BytesRead exceeds SizeLimit (when SizeLimit > 0).
// It returns api.ErrTooManyLines if LineCount exceeds LineLimit (when LineLimit > 0).
type LimitedCountingReader struct {
	Reader             io.Reader
	SizeLimit          int64
	LineLimit          int64
	BytesRead          int64
	LineCount          int64
	hasTrailingContent bool
}

// Read implements io.Reader, counting bytes and lines while checking limits.
func (r *LimitedCountingReader) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.BytesRead += int64(n)

	if r.SizeLimit > 0 && r.BytesRead > r.SizeLimit {
		return n, api.ErrFileTooLarge
	}

	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			r.LineCount++
			r.hasTrailingContent = false
			if r.LineLimit > 0 && r.LineCount > r.LineLimit {
				return n, api.ErrTooManyLines
			}
		} else {
			r.hasTrailingContent = true
		}
	}

	// Count the last line if it doesn't end with \n
	if err == io.EOF && r.hasTrailingContent {
		r.LineCount++
		if r.LineLimit > 0 && r.LineCount > r.LineLimit {
			return n, api.ErrTooManyLines
		}
	}

	return n, err
}

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

// This file provides utilities for counting lines in a streaming reader.
package common

import (
	"bytes"
	"fmt"
	"io"
)

// LineCountingReader wraps an io.Reader and counts newlines as data flows through.
// It returns an error if the line count exceeds the maximum allowed.
type LineCountingReader struct {
	reader   io.Reader
	maxLines int
	lines    int
}

// NewLineCountingReader creates a new LineCountingReader.
func NewLineCountingReader(reader io.Reader, maxLines int) *LineCountingReader {
	return &LineCountingReader{
		reader:   reader,
		maxLines: maxLines,
		lines:    0,
	}
}

// Read implements io.Reader interface and counts newlines.
func (r *LineCountingReader) Read(p []byte) (n int, err error) {
	n, err = r.reader.Read(p)
	if n > 0 {
		// Count newlines in the chunk we just read
		newlines := bytes.Count(p[:n], []byte{'\n'})
		r.lines += newlines

		// Check if we've exceeded the limit
		if r.lines > r.maxLines {
			return n, fmt.Errorf("file exceeds maximum allowed lines: %d (limit: %d)", r.lines, r.maxLines)
		}
	}
	return n, err
}

// Lines returns the current line count.
func (r *LineCountingReader) Lines() int {
	return r.lines
}

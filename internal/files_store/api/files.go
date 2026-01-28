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

// This file specifies the interfaces for the batch files storage.

// Package api provides interfaces for batch file storage operations.
package api

import (
	"context"
	"io"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/store"
)

type BatchClientAdmin interface {
	GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc)
	Close() error
}

type BatchFileMetadata struct {
	Location string    // Absolute location of the file.
	Size     int64     // The size of the file in bytes.
	ModTime  time.Time // Modification time.
}

type BatchFilesClient interface {
	store.BatchClientAdmin

	// Store stores a file in the files storage.
	Store(ctx context.Context, location string, fileSizeLimit int64, reader io.Reader) (
		fileMd *BatchFileMetadata, err error)

	// Retrieve retrieves a file from the files storage.
	Retrieve(ctx context.Context, location string) (reader io.Reader, fileMd *BatchFileMetadata, err error)

	// List lists the files in the specified location. Location here is a pattern.
	List(ctx context.Context, location string) (files []BatchFileMetadata, err error)

	// Delete deletes the file in the specified location.
	Delete(ctx context.Context, location string) (err error)
}

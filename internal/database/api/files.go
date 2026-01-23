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

package api

import (
	"context"
	"io"
	"time"
)

type BatchFileMetadata struct {
	Location string
	Size     int64
	ModeTime time.Time
}

type BatchFilesClient interface {
	BatchClientAdmin

	Store(ctx context.Context, location string, fileSizeLimit int64, reader io.Reader) (
		fileMd *BatchFileMetadata, err error)

	Retrieve(ctx context.Context, location string) (reader io.Reader, fileMd *BatchFileMetadata, err error)

	List(ctx context.Context, location string) (files []BatchFileMetadata, err error)

	Delete(ctx context.Context, location string) (err error)
}

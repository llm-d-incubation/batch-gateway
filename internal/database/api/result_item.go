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

package api

import (
	"context"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/store"
)

// ResultRow is one durably persisted request result: the output line a worker
// wrote for a request, keyed by batch and custom_id so a replacement worker
// can resume a batch without re-executing completed requests.
type ResultRow struct {
	BatchID  string
	CustomID string
	// IsError reports which local file the line belongs to (error.jsonl
	// vs output.jsonl), mirroring the collector's routing.
	IsError bool
	// Line is the marshaled output line without a trailing newline.
	Line []byte
}

// ResultDBClient persists per-request results as durable worker scratch space.
type ResultDBClient interface {
	store.BatchClientAdmin

	// ResultStore inserts one row; a duplicate (batch_id, custom_id) is a no-op.
	ResultStore(ctx context.Context, row *ResultRow) error
	// ResultGetAll returns every persisted row for the batch.
	ResultGetAll(ctx context.Context, batchID string) ([]*ResultRow, error)
	// ResultDelete removes every persisted row for the batch.
	ResultDelete(ctx context.Context, batchID string) error
}

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

package worker

import (
	"context"
	"sync/atomic"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

// jobExecutionParams holds the job-scoped state shared across processing stages.
// Contexts are NOT stored here — they are passed explicitly per Go convention.
//
// cancelRequested must be a non-nil *atomic.Bool — it is shared across goroutines
// (watchCancel, preProcessJob, executeJob, finalizeJob).
type jobExecutionParams struct {
	updater *StatusUpdater
	jobItem *db.BatchItem
	jobInfo *batch_types.JobInfo
	task    *db.BatchJobPriority

	eventWatcher *db.BatchEventsChan
	abortInferFn context.CancelFunc

	cancelRequested *atomic.Bool

	requestCounts *openai.BatchRequestCounts
}

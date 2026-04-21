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

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

// jobExecutionParams holds the job-scoped state shared across processing stages.
// Contexts are NOT stored here — they are passed explicitly per Go convention.
// userCancelFn and requestAbortFn are exceptions: they are cancel functions stored
// here so that the watchCancel goroutine can call them when a cancel event arrives.
// userCancelFn cancels userCancelCtx (user-cancel signal, derived from context.Background).
// requestAbortFn cancels requestAbortCtx (dispatch loop control, derived from sloCtx) to stop
// dispatch immediately when the user cancels.
type jobExecutionParams struct {
	updater *StatusUpdater
	jobItem *db.BatchItem
	jobInfo *batch_types.JobInfo
	task    *db.BatchJobPriority

	eventWatcher   *db.BatchEventsChan
	userCancelFn   context.CancelFunc
	requestAbortFn context.CancelFunc

	requestCounts *openai.BatchRequestCounts
}

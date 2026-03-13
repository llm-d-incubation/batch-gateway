package worker

import (
	"context"
	"sync"
	"sync/atomic"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

// jobExecutionParams holds the job-scoped state shared across processing stages.
type jobExecutionParams struct {
	ctx      context.Context
	sloCtx   context.Context
	inferCtx context.Context

	updater *StatusUpdater
	jobItem *db.BatchItem
	jobInfo *batch_types.JobInfo
	task    *db.BatchJobPriority

	eventWatcher  *db.BatchEventsChan
	inferCancelFn context.CancelFunc

	cancelRequested *atomic.Bool
	cancellingOnce  *sync.Once

	requestCounts *openai.BatchRequestCounts
}

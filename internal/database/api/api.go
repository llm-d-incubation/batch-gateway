package api

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

type BatchDBClient interface {

	// Store stores a batch job, and adds the job to the processing queue.
	// Returns the ID of the job in the database.
	Store(ctx context.Context, logger logging.Logger, job *BatchJob) (ID string, err error)

	// GetForProcessing retrieves batch jobs for processing.
	// This function blocks for the duration specified by timeout. If timeout is zero, the function does not block.
	// This function will attempt to pull the maximum number of jobs specified in maxJobs.
	// Returns a list of jobs and a list of event channels, corresponding to the jobs.
	// For each returned job whose processing is finished by the caller - the caller MUST call the function
	// CloseFn specified in BatchEventsChan, to close the associated resources.
	GetForProcessing(ctx context.Context, logger logging.Logger, timeout time.Duration, maxJobs int) (
		jobs []*BatchJob, events []*BatchEventsChan, err error)

	// Update updates the dynamic parts of a batch job, and optionally adds the job ID back to the processing queue.
	// The function will update in the job's record in the database - all the dynamic fields of the job which are not empty in the given job object.
	// Any dynamic field that is empty in the given job object - will not be updated in the job's record in the database.
	// If 'addJobToQueue' is set to true then the job's object SLO field must be non empty, and the function will
	// add the job's ID to the processing queue.
	Update(ctx context.Context, logger logging.Logger, job *BatchJob, addJobToQueue bool) (err error)

	// Get gets the information (static and dynamic) of batch jobs.
	// If IDs are specified, this function will get jobs by the specified IDs.
	// If tags are specified, this function will get jobs by the specified tags.
	// If no IDs nor tags are specified, the function will return an empty list of jobs.
	// tagsLogicalCond specifies the logical condition to use for when searching for the tags per job.
	// includeStatic specifies if to include the static part of a job in the returned output.
	// start and limit specify the pagination details. This is relevant only for search by tags.
	// In the first iteration with pagination specify 0 for 'start', and in any subsequent iteration specify in 'start'
	// the value that was returned by 'cursor' in the previous iteration. The value returned by 'cursor' is an opaque integer.
	// The value specified in 'limit' can be different between iterations, and is a recommendation only.
	// jobs is a slice of returned jobs.
	// cursor is an opaque integer that should be given in the next paginated call via the 'start' parameter.
	Get(ctx context.Context, logger logging.Logger, IDs []string, tags []string, tagsLogicalCond TagsLogicalCond,
		includeStatic bool, start, limit int) (
		jobs []*BatchJob, cursor int, err error)

	// SendEvent sends events for the specified jobs.
	// The events are sent and consumed in FIFO order.
	SendEvent(ctx context.Context, logger logging.Logger, events []BatchEvent) (sentIDs []string, err error)

	// Delete deletes batch jobs.
	Delete(ctx context.Context, logger logging.Logger, IDs []string) (deletedIDs []string, err error)

	// Get default context for the DB request.
	GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc)

	// Close closes the client.
	Close() error
}

type BatchJob struct {
	ID     string    // [mandatory, immutable, returned by get, parsed by DB, must be unique] User provided unique ID of the job. This ID must be unique.
	SLO    time.Time // [mandatory, immutable, returned by get, parsed by DB] The time based on which the job should be prioritized relative to other jobs.
	TTL    int       // [mandatory, immutable, not returned by get, parsed by DB] The number of seconds to set for the TTL of the DB record.
	Tags   []string  // [optional, updatable, returned by get, parsed by DB] A list of tags that enable to select jobs based on the tags' contents. The tags must not contain ';;', which is the separator.
	Spec   []byte    // [optional, immutable, returned optionally by get, opaque to DB] The static part of the batch job (serialized), including the job's specification.
	Status []byte    // [optional, updatable, returned by get, opaque to DB] The dynamic part of the batch job (serialized), including its status.
}

func (bj *BatchJob) IsValid() error {
	if len(bj.ID) == 0 {
		return fmt.Errorf("ID is empty")
	}
	if bj.SLO.IsZero() {
		return fmt.Errorf("SLO is zero for ID %s", bj.ID)
	}
	if bj.TTL <= 0 {
		return fmt.Errorf("TTL is invalid for ID %s", bj.ID)
	}
	return nil
}

type BatchEventType int

const (
	BatchEventCancel  BatchEventType = iota // Cancel a job.
	BatchEventPause                         // Pause a job.
	BatchEventUnpause                       // Unpause a job.
	BatchEventMaxVal                        // [Internal] Indicates the max value for the enum. Don't use this value.
)

type BatchEvent struct {
	ID   string         // [mandatory] ID of the job.
	Type BatchEventType // [mandatory] Event type.
	TTL  int            // [mandatory] TTL in seconds for the event. Must be the same for all the events sent for the same job ID. Set this for sending an event. This is not returned for the event consumer.
}

func (be *BatchEvent) IsValid() error {
	if len(be.ID) == 0 {
		return fmt.Errorf("ID is empty")
	}
	if be.Type < BatchEventCancel || be.Type >= BatchEventMaxVal {
		return fmt.Errorf("event type %d is invalid for ID %s", be.Type, be.ID)
	}
	if be.TTL <= 0 {
		return fmt.Errorf("TTL is invalid for ID %s", be.ID)
	}
	return nil
}

type BatchEventsChan struct {
	ID      string          // ID of the job.
	Events  chan BatchEvent // Channel for receiving events for the job.
	CloseFn func()          // Function for closing the channel and the associated resources. Must be called by the listener when the job's processing is finished.
}

type TagsLogicalCond int

const (
	TagsLogicalCondNa TagsLogicalCond = iota
	TagsLogicalCondAnd
	TagsLogicalCondOr
	TagsLogicalCondMaxVal // [Internal] Indicates the max value for the enum. Don't use this value.
)

var TagsLogicalCondNames = map[TagsLogicalCond]string{
	TagsLogicalCondAnd: "and",
	TagsLogicalCondOr:  "or",
}

//----

// BatchDB is responsible for persisting batch metadata.
type BatchDB interface {
	// Store saves a new batch run metadata.
	Store(ctx context.Context, logger logging.Logger, runMeta *RunMetadata, DBtype string, DBOptions any) (runId string, err error)

	// List batch runs by conditions
	List(ctx context.Context, logger logging.Logger, DBtype string, DBOptions any) (runs []*RunMetadata, next int, err error)

	// Get run by id
	Get(ctx context.Context, logger logging.Logger, DBtype string, DBOptions any) (run *RunMetadata, err error)

	// Update modifies mutable fields of a batch run.
	Update(ctx context.Context, logger logging.Logger, run *RunMetadata, DBtype string, DBOptions any) (err error)

	// Delete removes batch run metadata.
	Delete(ctx context.Context, logger logging.Logger, DBtype string, DBOptions any) (deletedId string, err error)

	// Cancel marks batch run as canceled.
	Cancel(ctx context.Context, logger logging.Logger, DBtype string, DBOptions any) (canceledId string, err error)

	// GetContext returns a derived context with a timeout for DB operations.
	GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc)

	// Close releases any resources held by the implementation.
	Close() error
}

type RunMetadata struct {
	Id     string // [mandatory, immutable, returned by get, parsed by DB, must be unique] User provided unique ID of the run. This ID must be unique.
	Spec   []byte // [optional, immutable, returned optionally by get, opaque to DB] The static part of the batch run (serialized), including the run's specification.
	Status []byte // [optional, updatable, returned by get, opaque to DB] The dynamic part of the batch run (serialized), including its status.
}

type JobsOptions struct {
	UserFs bool
	JobId  string
	runId  string
	Start  string
	Limit  int
	States []string
}

// RunQueue handles enqueuing, dequeuing and removal of runs from the processing queue.
type RunQueue interface {
	// Enqueue adds a run id to the processing queue.
	Enqueue(ctx context.Context, logger logging.Logger, jobId, runId string, SLO time.Time) error

	// Dequeue blocks up to timeout for a run id to become available.
	// If timeout is zero, it returns immediately with no runs.
	Dequeue(ctx context.Context, logger logging.Logger, timeout time.Duration) (jobId, runId string, err error)

	// Remove deletes a run id from the queue (e.g., when a run is cancelled).
	Remove(ctx context.Context, logger logging.Logger, runId string) error

	// Close releases any resources held by the implementation.
	Close() error
}

// map to whatever the implementation is
type EventChannel interface {
	// Get gets an event channel based on the jobId and runId
	// CloseFn specified in BatchEventsChan, to close the associated resources.
	Get(ctx context.Context, logger logging.Logger, jobId, runId string) (event BatchEventsChan, err error)
	// Send sends events for the specified jobs.
	// The events are sent and consumed in FIFO order.
	Send(ctx context.Context, logger logging.Logger, events BatchEvent) (sentID string, err error)
}

// RunProgressDB stores temporary per‑job progress information.
// Entries are expected to be removed once the job finishes.
type RunProgressDB interface {
	// Set stores or updates progress data for a given job run.
	Set(ctx context.Context, logger logging.Logger, runId string, data []byte) error

	// Get retrieves the progress data for a given job run.
	// If no data exists, (nil, nil) is returned.
	Get(ctx context.Context, logger logging.Logger, runId string) (data []byte, err error)

	// Delete removes the progress entry for a completed or cancelled job run.
	Delete(ctx context.Context, logger logging.Logger, runId string) error

	// GetContext returns a derived context with a timeout for DB operations.
	GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc)

	// Close releases any resources held by the implementation.
	Close() error
}

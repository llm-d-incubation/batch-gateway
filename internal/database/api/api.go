package api

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

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

type BatchClientAdmin interface {

	// Get a default derived context for a call.
	GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc)

	// Close closes the client.
	Close() error
}

// BatchDBClient enables to manage batch job metadata objects in persistent storage.
type BatchDBClient interface {
	BatchClientAdmin

	// Store stores a batch job.
	// Returns the ID of the job in the database.
	Store(ctx context.Context, logger logging.Logger, job *BatchJob) (jobID string, err error)

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
	Get(ctx context.Context, logger logging.Logger, jobIDs []string, tags []string, tagsLogicalCond TagsLogicalCond,
		includeStatic bool, start, limit int) (
		jobs []*BatchJob, cursor int, err error)

	// Update updates the dynamic parts of a batch job.
	// The function will update in the job's record in the database - all the dynamic fields of the job which are not empty
	// in the given job object.
	// Any dynamic field that is empty in the given job object - will not be updated in the job's record in the database.
	Update(ctx context.Context, logger logging.Logger, job *BatchJob) (err error)

	// Delete deletes batch jobs.
	Delete(ctx context.Context, logger logging.Logger, jobIDs []string) (deletedJobIDs []string, err error)
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

type BatchJobPriority struct {
	ID  string
	SLO time.Time // The SLO value determines the priority of the job id.
}

// BatchPriorityQueueClient enables to perform operation on a priority queue.
type BatchPriorityQueueClient interface {
	BatchClientAdmin

	// Enqueue adds a job priority object to the queue.
	Enqueue(ctx context.Context, logger logging.Logger, jobPriority *BatchJobPriority) error

	// Dequeue returns the job priority objects at the head of the queue,
	// up to the maximum number of objects specified in maxObjs.
	// The function blocks up to the timeout value for a job priority object to be available.
	// If the timeout value is zero, the function returns immediately.
	Dequeue(ctx context.Context, logger logging.Logger, timeout time.Duration, maxObjs int) (
		jobPriority []*BatchJobPriority, err error)

	// Remove deletes a job priority object from the queue.
	Remove(ctx context.Context, logger logging.Logger, jobPriority *BatchJobPriority) error
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

// BatchEventChannelClient enables to create and use an event channel for a batch job being processed.
type BatchEventChannelClient interface {
	BatchClientAdmin

	// GetChannel gets an events channel for the jobID.
	// When processing of job is finished by the caller - the caller MUST call the function
	// CloseFn specified in BatchEventsChan, to close the associated resources.
	GetChannel(ctx context.Context, logger logging.Logger, jobID string) (batchEventsChan *BatchEventsChan, err error)

	// Send sends events for the specified jobs.
	// The events are sent and consumed in FIFO order.
	SendEvent(ctx context.Context, logger logging.Logger, events []BatchEvent) (sentIDs []string, err error)
}

// BatchStatusClient enables to manage temporary job information.
type BatchStatusClient interface {
	BatchClientAdmin

	// Set stores or updates status data for a job.
	Set(ctx context.Context, logger logging.Logger, jobID string, TTL int, data []byte) error

	// Get retrieves the status data for a job.
	// If no data exists (nil, nil) is returned.
	Get(ctx context.Context, logger logging.Logger, jobID string) (data []byte, err error)

	// Delete removes the status data for a job.
	Delete(ctx context.Context, logger logging.Logger, jobID string) error
}

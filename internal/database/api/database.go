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

// This file specifies the interfaces for the batch jobs data structures system.

package api

import (
	"context"
	"fmt"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/store"
)

// -- Batch jobs metadata store --

type BatchItem struct {
	ID     string // [mandatory, immutable, returned by get, parsed by DB, must be unique] User provided unique ID of the item. This ID must be unique.
	Expiry int64  // [optional, immutable, returned by get, parsed by DB] The unix timestamp in seconds when the item is considered expired.
	Tags   Tags   // [optional, updatable, returned by get, parsed by DB] A list of tags that enable to select items based on the tags' contents. The tags must not contain ';;', which is the internal separator used.
	Spec   []byte // [optional, immutable, returned optionally by get, opaque to DB] The static part of the batch item (serialized), including the item's specification.
	Status []byte // [optional, updatable, returned by get, opaque to DB] The dynamic part of the batch item (serialized), including its status.
	//SLO    time.Time // [mandatory, immutable, returned by get, parsed by DB] The time based on which the item should be prioritized relative to other items. TBR
}

func (bj *BatchItem) IsValid() error {
	if len(bj.ID) == 0 {
		return fmt.Errorf("ID is empty")
	}
	// if bj.SLO.IsZero() { TBR
	// 	return fmt.Errorf("SLO is zero for ID %s", bj.ID)
	// }
	// if bj.TTL <= 0 {
	// 	return fmt.Errorf("TTL is invalid for ID %s", bj.ID)
	// }
	return nil
}

// BatchDBClient enables to manage batch item metadata objects in persistent storage.
type BatchDBClient interface {
	store.BatchClientAdmin

	// DBStore stores a batch item metadata object.
	// Returns the ID of the item in the database.
	DBStore(ctx context.Context, item *BatchItem) (ID string, err error)

	// DBGet gets the information (static and dynamic) of batch items.
	// If IDs are specified, this function will get items by the specified IDs.
	// If tags are specified, this function will get items by the specified tags.
	// If expired is set to true, this function will get expired items. This option can be used with tags selection.
	// If no IDs nor tags nor expired are specified, the function will return an empty list of items.
	// tagsLogicalCond specifies the logical condition to use for when searching for the tags per item.
	// includeStatic specifies if to include the static part of a item in the returned output.
	// start and limit specify the pagination details. This is relevant only for search by tags.
	// In the first iteration with pagination specify 0 for 'start', and in any subsequent iteration specify in 'start'
	// the value that was returned by 'cursor' in the previous iteration. The value returned by 'cursor' is an opaque integer.
	// The value specified in 'limit' can be different between iterations, and is a recommendation only.
	// items is a slice of returned items.
	// cursor is an opaque integer that should be given in the next paginated call via the 'start' parameter.
	DBGet(ctx context.Context, query *BatchDBQuery,
		includeStatic bool, start, limit int) (
		items []*BatchItem, cursor int, expectedMore bool, err error)

	// DBUpdate updates the dynamic parts of a batch item.
	// The function will update in the item's record in the database - all the dynamic fields of the item which are not empty
	// in the given item object.
	// Any dynamic field that is empty in the given item object - will not be updated in the item's record in the database.
	DBUpdate(ctx context.Context, item *BatchItem) (err error)

	// DBDelete deletes batch items.
	DBDelete(ctx context.Context, IDs []string) (deletedIDs []string, err error)
}

type Tags map[string]string

type BatchDBQuery struct {
	IDs             []string
	TagSelectors    Tags
	TagsLogicalCond GenLogicalCond
	Expired         bool
}

type GenLogicalCond int

const (
	GenLogicalCondNa GenLogicalCond = iota
	GenLogicalCondAnd
	GenLogicalCondOr
	GenLogicalCondMaxVal // [Internal] Indicates the max value for the enum. Don't use this value.
)

var GenLogicalCondNames = map[GenLogicalCond]string{
	GenLogicalCondAnd: "and",
	GenLogicalCondOr:  "or",
}

// -- Batch jobs priority queue --

type BatchJobPriority struct {
	ID   string    `json:"id,omitempty"`   // [mandatory] ID of the batch job.
	SLO  time.Time `json:"slo,omitempty"`  // [mandatory] The SLO value determines the priority of the job.
	TTL  int       `json:"ttl,omitempty"`  // [optional] TTL in seconds for the record.
	Data []byte    `json:"data,omitempty"` // [optional] User defined data.
}

func (bj *BatchJobPriority) IsValid() error {
	if len(bj.ID) == 0 {
		return fmt.Errorf("ID is empty")
	}
	if bj.SLO.IsZero() {
		return fmt.Errorf("SLO is zero for ID %s", bj.ID)
	}
	// if bj.TTL <= 0 { TBD
	// 	return fmt.Errorf("TTL is invalid for ID %s", bj.ID)
	// }
	return nil
}

// BatchPriorityQueueClient enables to perform operations on a priority queue of jobs.
type BatchPriorityQueueClient interface {
	store.BatchClientAdmin

	// PQEnqueue adds a job priority object to the queue.
	PQEnqueue(ctx context.Context, jobPriority *BatchJobPriority) (err error)

	// PQDequeue returns the job priority objects at the head of the queue,
	// up to the maximum number of objects specified in maxObjs.
	// The function blocks up to the timeout value for a job priority object to be available.
	// If the timeout value is zero, the function returns immediately.
	PQDequeue(ctx context.Context, timeout time.Duration, maxObjs int) (
		jobPriorities []*BatchJobPriority, err error)

	// PQDelete deletes a job priority object from the queue.
	// It returns the number of deleted objects.
	// An error is returned only if the deletion operation failed.
	PQDelete(ctx context.Context, jobPriority *BatchJobPriority) (nDeleted int, err error)
}

// -- Batch jobs events and channels --

type BatchEventType int

const (
	BatchEventCancel BatchEventType = iota // Cancel a job.
	BatchEventPause                        // Pause a job.
	BatchEventResume                       // Resume a job.
	BatchEventMaxVal                       // [Internal] Indicates the max value for the enum. Don't use this value.
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
	CloseFn func()          // Function for closing the channel and the associated resources. Must be called by the consumer when the job's processing is finished.
}

// BatchEventChannelClient enables to create and use event channels for batch jobs being processed.
type BatchEventChannelClient interface {
	store.BatchClientAdmin

	// ECConsumerGetChannel gets an events channel for the job ID, to be used by a consumer to listen for events.
	// When the caller finishes processing a job - the caller must call the function CloseFn specified in BatchEventsChan,
	// to close the associated resources.
	ECConsumerGetChannel(ctx context.Context, ID string) (batchEventsChan *BatchEventsChan, err error)

	// ECProducerSendEvents sends the specified events via associated event channels.
	// The events are sent and consumed in FIFO order.
	ECProducerSendEvents(ctx context.Context, events []BatchEvent) (sentIDs []string, err error)
}

// -- Batch jobs temporary status store --

// BatchStatusClient enables to manage temporary job status.
type BatchStatusClient interface {
	store.BatchClientAdmin

	// STSet stores or updates status data for a job.
	StatusSet(ctx context.Context, ID string, TTL int, data []byte) (err error)

	// STGet retrieves the status data of a job.
	// If no data exists (nil, nil) is returned.
	StatusGet(ctx context.Context, ID string) (data []byte, err error)

	// STDelete deletes the status data for a job.
	StatusDelete(ctx context.Context, ID string) (nDeleted int, err error)
}

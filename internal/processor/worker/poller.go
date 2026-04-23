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

// this file contains the poller logic for the processor
package worker

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

type Poller struct {
	pq db.BatchPriorityQueueClient
	db db.BatchDBClient
}

func NewPoller(pq db.BatchPriorityQueueClient, db db.BatchDBClient) *Poller {
	return &Poller{
		pq: pq,
		db: db,
	}
}

func (p *Poller) validate() error {
	if p.pq == nil {
		return fmt.Errorf("priority queue client is missing")
	}
	if p.db == nil {
		return fmt.Errorf("database client is missing")
	}
	return nil
}

func (p *Poller) dequeueOne(ctx context.Context) (*db.BatchJobPriority, error) {
	tasks, err := p.pq.PQDequeue(ctx, 0, 1) // get only one job without blocking the queue
	if err != nil {
		return nil, err
	}

	logger := logr.FromContextOrDiscard(ctx)

	// there's no backlog
	if len(tasks) == 0 {
		logger.V(logging.TRACE).Info("No jobs to fetch")
		return nil, nil
	}

	logger.V(logging.DEBUG).Info("Successfully fetched a job", "jobID", tasks[0].ID)
	return tasks[0], nil
}

func (p *Poller) enqueueOne(ctx context.Context, task *db.BatchJobPriority) error {
	if task == nil {
		return fmt.Errorf("cannot enqueue nil batch job task")
	}
	return p.pq.PQEnqueue(ctx, task)
}

func (p *Poller) fetchJobItemByID(ctx context.Context, jobID string) (*db.BatchItem, error) {
	jobs, _, _, err := p.db.DBGet(ctx,
		&db.BatchQuery{
			BaseQuery: db.BaseQuery{IDs: []string{jobID}},
		},
		true, 0, 1)
	if err != nil {
		return nil, err
	}

	logger := logr.FromContextOrDiscard(ctx)

	// (nil, nil) signals "not found" — caller decides how to handle.
	if len(jobs) == 0 {
		logger.V(logging.DEBUG).Info("Job item not found in DB", "jobId", jobID)
		return nil, nil
	}

	logger.V(logging.DEBUG).Info("Job DB Data retrieved")
	return jobs[0], nil
}

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

// this file contains the utility functions for the batch object
package batch_utils

import (
	"strings"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/converter"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

// FromDBItemToJobInfoObject: convert db item to Processor's JobInfo object
func FromDBItemToJobInfoObject(job *db.BatchItem) (*batch_types.JobInfo, error) {
	jobInfo := &batch_types.JobInfo{
		JobID:    job.ID,
		BatchJob: &openai.Batch{},
	}

	batchJob, err := converter.DBItemToBatch(job)
	if err != nil {
		return nil, err
	}

	jobInfo.BatchJob = batchJob

	for _, tag := range job.Tags {
		if strings.HasPrefix(tag, "tenant:") {
			jobInfo.TenantID = strings.TrimPrefix(tag, "tenant:")
			break
		}
	}

	return jobInfo, nil
}

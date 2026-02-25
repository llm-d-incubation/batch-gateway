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
	"encoding/json"
	"fmt"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
)

func GetJobPriorityDataFromQueueItem(item *db.BatchJobPriority) (*batch_types.BatchJobPriorityData, error) {
	data := &batch_types.BatchJobPriorityData{}
	if err := json.Unmarshal(item.Data, data); err != nil {
		return nil, fmt.Errorf("failed to unmarshal job priority data: %w", err)
	}
	return data, nil
}

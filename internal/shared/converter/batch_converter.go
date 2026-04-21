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

package converter

import (
	"encoding/json"
	"fmt"

	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

func BatchToDBItem(batch *openai.Batch, tenantID string, tags api.Tags) (*api.BatchItem, error) {
	if batch == nil {
		return nil, fmt.Errorf("batch is nil")
	}

	specData, err := json.Marshal(batch.BatchSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch spec: %w", err)
	}

	statusData, err := json.Marshal(batch.BatchStatusInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch status: %w", err)
	}

	var expiry int64
	if batch.ExpiresAt != nil {
		expiry = *batch.ExpiresAt
	}

	item := &api.BatchItem{}
	item.ID = batch.ID
	item.TenantID = tenantID
	item.Expiry = expiry
	item.Tags = tags
	item.Spec = specData
	item.Status = statusData

	return item, nil
}

func DBItemToBatch(item *api.BatchItem) (*openai.Batch, error) {
	if item == nil {
		return nil, fmt.Errorf("batch item is nil")
	}

	batch := &openai.Batch{
		ID: item.ID,
	}

	if len(item.Spec) > 0 {
		if err := json.Unmarshal(item.Spec, &batch.BatchSpec); err != nil {
			return nil, fmt.Errorf("failed to unmarshal batch spec: %w", err)
		}
	}

	if len(item.Status) > 0 {
		if err := json.Unmarshal(item.Status, &batch.BatchStatusInfo); err != nil {
			return nil, fmt.Errorf("failed to unmarshal batch status: %w", err)
		}
	}

	return batch, nil
}

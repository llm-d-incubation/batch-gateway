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

func FileToDBItem(file *openai.FileObject, tenantID string, tags api.Tags) (*api.FileItem, error) {
	if file == nil {
		return nil, fmt.Errorf("file is nil")
	}

	specData, err := json.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal file: %w", err)
	}

	var expiry int64
	if file.ExpiresAt != nil && *file.ExpiresAt > 0 {
		expiry = *file.ExpiresAt
	}

	item := &api.FileItem{}
	item.ID = file.ID
	item.TenantID = tenantID
	item.Expiry = expiry
	item.Tags = tags
	item.Spec = specData
	item.Status = nil
	item.Purpose = string(file.Purpose)

	return item, nil
}

func DBItemToFile(item *api.FileItem) (*openai.FileObject, error) {
	if item == nil {
		return nil, fmt.Errorf("file item is nil")
	}

	file := &openai.FileObject{}

	if len(item.Spec) > 0 {
		if err := json.Unmarshal(item.Spec, file); err != nil {
			return nil, fmt.Errorf("failed to unmarshal file: %w", err)
		}
	}

	return file, nil
}

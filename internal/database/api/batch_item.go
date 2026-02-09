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

package api

import (
	"fmt"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

// BatchItem is the database item type for openai.Batch objects.
type BatchItem = BaseItem[openai.Batch]

// BatchDBClient is the typed database client for batch objects.
type BatchDBClient = DBClient[openai.Batch]

// IsBatchItemValid validates a BatchItem for required fields.
func IsBatchItemValid(item *BatchItem) error {
	if item == nil {
		return fmt.Errorf("item is nil")
	}
	if len(item.ID) == 0 {
		return fmt.Errorf("ID is empty")
	}
	return nil
}

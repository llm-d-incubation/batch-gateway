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

package regression

import (
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

func TestSchemaCompat_Batch(t *testing.T) {
	t.Run("Batch", func(t *testing.T) {
		assertRoundTrip[openai.Batch](t, "batch_full.golden.json")
	})

	t.Run("ListBatchResponse", func(t *testing.T) {
		assertRoundTrip[openai.ListBatchResponse](t, "batch_list.golden.json")
	})

	t.Run("CreateBatchRequest", func(t *testing.T) {
		assertRoundTrip[openai.CreateBatchRequest](t, "create_batch_request.golden.json")
	})
}

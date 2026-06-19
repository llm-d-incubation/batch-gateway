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
	"encoding/json"
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

func TestRegression_OmitemptyFields(t *testing.T) {
	t.Run("Batch_ModelOmitted", func(t *testing.T) {
		var b openai.Batch
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if _, ok := m["model"]; ok {
			t.Error("zero-value Batch should omit 'model' field (omitempty)")
		}
	})

	t.Run("FileObject_StatusDetailsOmitted", func(t *testing.T) {
		var f openai.FileObject
		data, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if _, ok := m["status_details"]; ok {
			t.Error("zero-value FileObject should omit 'status_details' field (omitempty)")
		}
	})

	t.Run("CreateBatchRequest_MetadataOmitted", func(t *testing.T) {
		req := openai.CreateBatchRequest{
			InputFileID:      "file_abc123",
			Endpoint:         openai.EndpointChatCompletions,
			CompletionWindow: "24h",
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("failed to marshal: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("failed to unmarshal: %v", err)
		}
		if _, ok := m["metadata"]; ok {
			t.Error("CreateBatchRequest without metadata should omit 'metadata' field (omitempty)")
		}
	})
}

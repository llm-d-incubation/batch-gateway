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

package openai

import (
	"strings"
	"testing"
)

func validRequest() CreateBatchRequest {
	return CreateBatchRequest{
		InputFileID:      "file_abc123",
		Endpoint:         EndpointChatCompletions,
		CompletionWindow: "24h",
	}
}

func TestValidate_MetadataTooManyKeys(t *testing.T) {
	req := validRequest()
	req.Metadata = make(map[string]string, 17)
	for i := range 17 {
		req.Metadata[string(rune('a'+i))+"key"] = "val"
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for >16 metadata keys")
	}
	if !strings.Contains(err.Error(), "too many keys") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_MetadataKeyTooLong(t *testing.T) {
	req := validRequest()
	req.Metadata = map[string]string{
		strings.Repeat("k", 65): "val",
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for key >64 chars")
	}
	if !strings.Contains(err.Error(), "key exceeds 64 characters") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_MetadataValueTooLong(t *testing.T) {
	req := validRequest()
	req.Metadata = map[string]string{
		"key": strings.Repeat("v", 513),
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected error for value >512 chars")
	}
	if !strings.Contains(err.Error(), "exceeds 512 characters") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestValidate_MetadataAtLimits(t *testing.T) {
	req := validRequest()
	req.Metadata = make(map[string]string, 16)
	for i := range 16 {
		key := strings.Repeat("k", 64-1) + string(rune('a'+i))
		req.Metadata[key] = strings.Repeat("v", 512)
	}
	if err := req.Validate(); err != nil {
		t.Errorf("expected no error at exact limits, got: %v", err)
	}
}

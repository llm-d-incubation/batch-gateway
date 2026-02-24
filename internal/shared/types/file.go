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

package batch_types

import "github.com/llm-d-incubation/batch-gateway/internal/shared/openai"

type FileSpec struct {
	Bytes      int64                    `json:"bytes"`
	CreatedAt  int64                    `json:"created_at"`
	ExpiresAt  int64                    `json:"expires_at"`
	FolderName string                   `json:"folder_name"`
	Filename   string                   `json:"filename"`
	Purpose    openai.FileObjectPurpose `json:"purpose"`
	LineNumber int64                    `json:"line_number"`
	ModTime    int64                    `json:"mod_time"`
}

type FileStatusInfo struct {
	Status        openai.FileObjectStatus `json:"status,omitempty"`
	StatusDetails string                  `json:"status_details,omitempty"`
}

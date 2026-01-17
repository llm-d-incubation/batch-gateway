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

// The file defines the File API data structures matching the OpenAI specification.
package openai

// https://platform.openai.com/docs/api-reference/files

// File - The `File` object represents a document that has been uploaded to OpenAI.
type File struct {

	// The file identifier, which can be referenced in the API endpoints.
	ID string `json:"id"`

	// The size of the file, in bytes.
	Bytes int32 `json:"bytes"`

	// The Unix timestamp (in seconds) for when the file was created.
	CreatedAt int32 `json:"created_at"`

	// The Unix timestamp (in seconds) for when the file will expire.
	ExpiresAt int32 `json:"expires_at,omitempty"`

	// The name of the file.
	Filename string `json:"filename"`

	// The object type, which is always `file`.
	Object string `json:"object"`

	// The intended purpose of the file. Supported values are `assistants`, `assistants_output`, `batch`, `batch_output`, `fine-tune`, `fine-tune-results` and `vision`.
	Purpose string `json:"purpose"`

	// Deprecated. The current status of the file, which can be either `uploaded`, `processed`, or `error`.
	Status string `json:"status"`

	// Deprecated. For details on why a fine-tuning training file failed validation, see the `error` field on `fine_tuning.job`.
	StatusDetails string `json:"status_details,omitempty"`
}

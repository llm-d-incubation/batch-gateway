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

// The intended purpose of the file. Supported values are `assistants`,
// `assistants_output`, `batch`, `batch_output`, `fine-tune`, `fine-tune-results`,
// `vision`, and `user_data`.
type FileObjectPurpose string

const (
	FileObjectPurposeAssistants       FileObjectPurpose = "assistants"
	FileObjectPurposeAssistantsOutput FileObjectPurpose = "assistants_output"
	FileObjectPurposeBatch            FileObjectPurpose = "batch"
	FileObjectPurposeBatchOutput      FileObjectPurpose = "batch_output"
	FileObjectPurposeFineTune         FileObjectPurpose = "fine-tune"
	FileObjectPurposeFineTuneResults  FileObjectPurpose = "fine-tune-results"
	FileObjectPurposeVision           FileObjectPurpose = "vision"
	FileObjectPurposeUserData         FileObjectPurpose = "user_data"
)

// IsValid checks if the FileObjectPurpose is one of the valid values.
func (p FileObjectPurpose) IsValid() bool {
	switch p {
	case FileObjectPurposeAssistants,
		FileObjectPurposeAssistantsOutput,
		FileObjectPurposeBatch,
		FileObjectPurposeBatchOutput,
		FileObjectPurposeFineTune,
		FileObjectPurposeFineTuneResults,
		FileObjectPurposeVision,
		FileObjectPurposeUserData:
		return true
	default:
		return false
	}
}

// Deprecated. The current status of the file, which can be either `uploaded`,
// `processed`, or `error`.
type FileObjectStatus string

const (
	FileObjectStatusUploaded  FileObjectStatus = "uploaded"
	FileObjectStatusProcessed FileObjectStatus = "processed"
	FileObjectStatusError     FileObjectStatus = "error"
)

// File - The `FileObject` represents a document that has been uploaded to OpenAI.
type FileObject struct {

	// required. The file identifier, which can be referenced in the API endpoints.
	ID string `json:"id"`

	// required. The size of the file, in bytes.
	Bytes int64 `json:"bytes"`

	// required. The Unix timestamp (in seconds) for when the file was created.
	CreatedAt int64 `json:"created_at"`

	// optional, nullable. The Unix timestamp (in seconds) for when the file will expire.
	ExpiresAt *int64 `json:"expires_at"`

	// required. The name of the file.
	Filename string `json:"filename"`

	// required. The object type, which is always `file`.
	Object string `json:"object"`

	// required. The intended purpose of the file. Supported values are `assistants`, `assistants_output`, `batch`, `batch_output`, `fine-tune`, `fine-tune-results` and `vision`.
	Purpose FileObjectPurpose `json:"purpose"`

	// Deprecated. The current status of the file, which can be either `uploaded`, `processed`, or `error`.
	Status FileObjectStatus `json:"status"`

	// Deprecated. For details on why a fine-tuning training file failed validation, see the `error` field on `fine_tuning.job`.
	StatusDetails string `json:"status_details,omitempty"`
}

// ListFilesResponse represents the response for listing files.
type ListFilesResponse struct {
	// required. The type of object returned, must be `list`.
	Object string `json:"object"`

	// required. A list of items used to generate this response.
	Data []FileObject `json:"data"`

	// required. The ID of the first item in the list.
	FirstID string `json:"first_id"`

	// required. The ID of the last item in the list.
	LastID string `json:"last_id"`

	// required. Whether there are more items available.
	HasMore bool `json:"has_more"`
}

// FileDeleteResponse represents the response when deleting a file.
type FileDeleteResponse struct {
	// required. The ID of the deleted file.
	ID string `json:"id"`

	// required. The object type, which is always `file`.
	Object string `json:"object"`

	// required. Whether the file was successfully deleted.
	Deleted bool `json:"deleted"`
}

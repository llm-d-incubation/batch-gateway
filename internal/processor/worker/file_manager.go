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

package worker

import (
	"fmt"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	filesapi "github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
)

// fileManager groups the file-related clients used by the Processor:
// object storage (S3, local FS) for uploading/downloading file content,
// and the file metadata database for storing/retrieving file records.
type fileManager struct {
	storage filesapi.BatchFilesClient
	db      db.FileDBClient
}

func newFileManager(storage filesapi.BatchFilesClient, db db.FileDBClient) *fileManager {
	return &fileManager{storage: storage, db: db}
}

func (fm *fileManager) validate() error {
	if fm.storage == nil {
		return fmt.Errorf("files client is missing")
	}
	if fm.db == nil {
		return fmt.Errorf("file database client is missing")
	}
	return nil
}

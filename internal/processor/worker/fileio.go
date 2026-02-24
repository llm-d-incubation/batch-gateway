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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	filesapi "github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
)

func (p *Processor) jobRootDir(jobID string) string {
	return filepath.Join(p.cfg.WorkDir, "jobs", jobID)
}

func (p *Processor) jobInputFilePath(jobID string) string {
	return filepath.Join(p.jobRootDir(jobID), "input.jsonl")
}

// openInputFileStream opens the input file stream
func (p *Processor) openInputFileStream(ctx context.Context, inputFileID string) (io.ReadCloser, *filesapi.BatchFileMetadata, error) {
	// get file metadata from database
	items, _, _, err := p.clients.database.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{inputFileID}}}, true, 0, 1)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get input file metadata: %w", err)
	}
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("input file %q not found in db", inputFileID)
	}

	// unmarshal file object into openai file object to get inputfile filename and tenant id
	fileItem := items[0]
	fileObj := &openai.FileObject{}
	if err := json.Unmarshal(fileItem.Spec, fileObj); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal file object: %w", err)
	}

	reader, metadata, err := p.clients.files.Retrieve(ctx, fileObj.Filename, fileItem.TenantID)
	if err != nil {
		return nil, metadata, fmt.Errorf("failed to open input file stream: %w", err)
	}
	return reader, metadata, nil
}

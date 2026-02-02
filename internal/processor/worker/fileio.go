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
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch_utils"
)

// openInputFileStream opens the input file stream and validates the file size
func (p *Processor) openInputFileStream(ctx context.Context, fileLocation string) (reader io.ReadCloser, metadata *api.BatchFileMetadata, err error) {
	rawReader, metadata, err := p.clients.files.Retrieve(ctx, fileLocation)
	if err != nil {
		return nil, metadata, fmt.Errorf("failed to open input file stream: %w", err)
	}

	readCloser, ok := rawReader.(io.ReadCloser)
	if !ok {
		readCloser = io.NopCloser(rawReader)
	}

	return readCloser, metadata, nil
}

func (p *Processor) writeResultsToFileLoop(
	ctx context.Context,
	jobID string,
	resChan <-chan *batch_utils.Response,
) (outputFileLocation string, err error) {
	// create the output file
	tempDir := os.TempDir()
	fileLocation := filepath.Join(tempDir, fmt.Sprintf("batch_output_%s.jsonl", jobID))
	f, err := os.Create(fileLocation)
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}

	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			err = closeErr
		}
	}()

	writer := bufio.NewWriter(f)

	for {
		select {
		case <-ctx.Done(): // context cancelled.
			// TODO:: delete the output file
			// re-enqueue the job / status update to in_progress so we can try again?
			return "", ctx.Err()
		case res, ok := <-resChan:
			if !ok { // channel closed. reader finished processing the input file.
				if err := writer.Flush(); err != nil {
					return "", err
				}
				return fileLocation, nil
			}

			// TODO:: create separate response file and error file
			// based on the response.Error field, write to the appropriate file.
			line, _ := json.Marshal(res)
			if _, err := writer.Write(append(line, '\n')); err != nil {
				return "", fmt.Errorf("file write error: %w", err)
			}
		}
	}
}

func (p *Processor) uploadFinalResult(ctx context.Context, localPath string) (string, error) {
	// TODO:: upload the output file to the location
	return "", nil
}

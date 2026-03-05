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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const planEntrySize = 16 // 8 bytes offset + 4 bytes length + 4 bytes prefixHash

// unmarshalPlanEntry decodes a 16-byte little-endian buffer into a planEntry.
func unmarshalPlanEntry(buf [planEntrySize]byte) planEntry {
	return planEntry{
		Offset:     int64(binary.LittleEndian.Uint64(buf[0:8])),
		Length:     binary.LittleEndian.Uint32(buf[8:12]),
		PrefixHash: binary.LittleEndian.Uint32(buf[12:16]),
	}
}

// readPlanEntries reads all plan entries from a finalized plan file.
func readPlanEntries(planFilePath string) ([]planEntry, error) {
	f, err := os.Open(planFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open plan file %s: %w", planFilePath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat plan file: %w", err)
	}

	// check if the plan file size is a multiple of planEntrySize
	if info.Size()%planEntrySize != 0 {
		return nil, fmt.Errorf("plan file size %d is not a multiple of %d", info.Size(), planEntrySize)
	}

	numEntries := int(info.Size() / planEntrySize)
	entries := make([]planEntry, 0, numEntries)

	var buffer [planEntrySize]byte
	for {
		_, err := io.ReadFull(f, buffer[:])
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to read plan entry: %w", err)
		}
		entries = append(entries, unmarshalPlanEntry(buffer))
	}

	return entries, nil
}

// readModelMap reads the model_map.json file from the job root directory.
func readModelMap(jobRootDir string) (*modelMapFile, error) {
	path := filepath.Join(jobRootDir, modelMapFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read model map file: %w", err)
	}

	var mm modelMapFile
	if err := json.Unmarshal(data, &mm); err != nil {
		return nil, fmt.Errorf("failed to unmarshal model map file: %w", err)
	}
	return &mm, nil
}

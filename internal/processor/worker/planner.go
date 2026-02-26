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

// this file contains the plan file reader/writer logic for the job processing plan files
package worker

import (
	"container/list"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
)

const modelMapFileName = "model_map.json"

type planRequestLine struct {
	Body struct {
		Model string `json:"model"`
	} `json:"body"`
}

// planEntry is a single entry in the plan file
// Offset: start byte offset of the request line in input.jsonl
// Length: exact byte length of that line in input.jsonl (including '\n')
type planEntry struct {
	Offset int64
	Length uint32
}

// planWriter is a writer struct for the plan files
// it uses a least recently used (LRU) cache to manage the open files, with the max open files limit.
// this is to avoid opening too many files at once, which can cause performance issues
// jobRootDir is the root directory for the job - caller should ensure the dir is created with jobID so that the plan files are stored in the job directory
type planWriter struct {
	jobRootDir string
	maxOpen    int

	mu    sync.Mutex
	files map[string]*os.File

	openFiles      *list.List               // list of modelIDs in the order of most recently used. front: Most Recently Used, back: Least Recently Used
	openFilesIndex map[string]*list.Element // modelID -> list node in openFiles list
}

// newPlanWriter creates a plan writer
func newPlanWriter(jobRootDir string, maxOpen int) *planWriter {
	return &planWriter{
		jobRootDir:     jobRootDir,
		maxOpen:        maxOpen,
		files:          make(map[string]*os.File),
		openFiles:      list.New(),
		openFilesIndex: make(map[string]*list.Element),
	}
}

// modelMapFile is a map of modelID to the file name of the plan file
type modelMapFile struct {
	ModelToSafe map[string]string `json:"model_to_safe"`
	SafeToModel map[string]string `json:"safe_to_model"`
	LineCount   int64             `json:"line_count"`
}

func writeModelMapFile(jobRootDir string, modelMapFile modelMapFile) error {
	finalPath := filepath.Join(jobRootDir, modelMapFileName)
	tempPath := finalPath + ".tmp"

	if err := os.MkdirAll(jobRootDir, 0o700); err != nil {
		return fmt.Errorf("failed to create job root directory: %w", err)
	}

	data, err := json.MarshalIndent(modelMapFile, "", "  ") // indent for manual inspection
	if err != nil {
		return fmt.Errorf("failed to marshal model map file: %w", err)
	}

	// temp file creation
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write model map file: %w", err)
	}

	// atomic commit
	if err := os.Rename(tempPath, finalPath); err != nil {
		// best effort to rename the file + cleanup
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to rename model map file: %w", err)
	}
	return nil
}

// safeFileNameRegex is a regex to replace invalid file name characters with '_'
var safeFileNameRegex = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// plansDir returns the directory for the plan files
// it is a subdirectory of the job root directory
func (w *planWriter) plansDir() string {
	return filepath.Join(w.jobRootDir, "plans")
}

// tempPlanPath returns the temporary path for the plan file
// it is used to write the plan file while the download is in progress
func (w *planWriter) tempPlanPath(modelID string) string {
	return filepath.Join(w.plansDir(), modelID+".plan.tmp")
}

// finalPath returns the final path for the plan file
// it is used to store the plan file after the download is completed
func (w *planWriter) finalPath(modelID string) string {
	return filepath.Join(w.plansDir(), modelID+".plan")
}

// internModelID interns the modelID to a safe file name
// it is used to ensure the modelID is a valid file name
// it is also used to avoid conflicts with other modelIDs
func internModelID(modelID string, used map[string]int) string {
	base := safeFileNameRegex.ReplaceAllString(modelID, "_")
	if base == "" {
		base = "unknown"
	}
	if used[base] == 0 {
		used[base] = 1
		return base
	}

	used[base]++
	return fmt.Sprintf("%s_%d", base, used[base])
}

// reorderOpenFiles reorders the open files list for the modelID
// it moves the specified modelID to the front of the open files list, making it the most recently used
func (w *planWriter) reorderOpenFiles(modelID string) {
	if element, ok := w.openFilesIndex[modelID]; ok {
		w.openFiles.MoveToFront(element)
		return
	}
	w.openFilesIndex[modelID] = w.openFiles.PushFront(modelID)
}

// evict evicts the least recently used modelID if the max open files limit is reached
func (w *planWriter) evict() error {
	if w.maxOpen <= 0 {
		return nil // unlimited open file. do nothing.
	}
	for len(w.files) > w.maxOpen { // max open files limit is reached.
		target := w.openFiles.Back() // get the least recently used modelID
		if target == nil {
			return nil
		}
		modelID := target.Value.(string)
		w.openFiles.Remove(target)
		delete(w.openFilesIndex, modelID) // remove the pointer to the modelID from the openfiles index map

		file := w.files[modelID]
		delete(w.files, modelID) // remove the file from the files map
		if file != nil {
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}

// openFile must be called with the lock held
func (w *planWriter) openFile(modelID string) (*os.File, error) {
	if file, ok := w.files[modelID]; ok && file != nil {
		w.reorderOpenFiles(modelID)
		return file, nil
	}

	if err := os.MkdirAll(w.plansDir(), 0o700); err != nil { //rwx------
		return nil, err
	}

	// important: append-only, truncate is forbidden
	file, err := os.OpenFile(w.tempPlanPath(modelID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // -rw-------
	if err != nil {
		return nil, err
	}

	w.files[modelID] = file
	w.reorderOpenFiles(modelID)

	if err := w.evict(); err != nil {
		return nil, err
	}
	return file, nil
}

// format (little-endian, fixed 12 bytes):
// [int64 offset][uint32 length]
func (w *planWriter) AppendEntry(modelID string, entry planEntry) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	file, err := w.openFile(modelID)
	if err != nil {
		return err
	}

	var buffer [12]byte // 8 bytes for offset, 4 bytes for length
	binary.LittleEndian.PutUint64(buffer[0:8], uint64(entry.Offset))
	binary.LittleEndian.PutUint32(buffer[8:12], entry.Length)

	_, err = file.Write(buffer[:])
	return err
}

func (w *planWriter) CloseAll() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	var firstErr error
	for modelID, f := range w.files {
		if f == nil {
			continue
		}
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", modelID, err)
		}
	}

	w.files = make(map[string]*os.File)
	w.openFiles.Init()
	w.openFilesIndex = make(map[string]*list.Element)
	return firstErr
}

func (w *planWriter) Finalize(modelIDs []string) error {
	if err := w.CloseAll(); err != nil {
		return err
	}

	if err := os.MkdirAll(w.plansDir(), 0o700); err != nil {
		return err
	}

	for _, modelID := range modelIDs {
		tmp := w.tempPlanPath(modelID)
		final := w.finalPath(modelID)

		if _, err := os.Stat(tmp); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}

		if err := os.Rename(tmp, final); err != nil {
			return err
		}
	}
	return nil
}

func appendPlanEntryForModel(
	planWriter *planWriter,
	modelID string,
	modelToSafe map[string]string,
	used map[string]int,
	offset int64,
	line []byte,
) (int64, string, uint32, error) {
	safeModelID, ok := modelToSafe[modelID]
	if !ok {
		safeModelID = internModelID(modelID, used)
		modelToSafe[modelID] = safeModelID
	}

	length := uint32(len(line))
	if err := planWriter.AppendEntry(safeModelID, planEntry{Offset: offset, Length: length}); err != nil {
		return offset, safeModelID, length, err
	}
	return offset + int64(length), safeModelID, length, nil
}

func finalizePlanFiles(planWriter *planWriter, modelToSafe map[string]string) error {
	modelIDs := make([]string, 0, len(modelToSafe))
	for _, safeID := range modelToSafe {
		modelIDs = append(modelIDs, safeID)
	}
	// predictable order helps reproducibility and debugging.
	sort.Strings(modelIDs)
	return planWriter.Finalize(modelIDs)
}

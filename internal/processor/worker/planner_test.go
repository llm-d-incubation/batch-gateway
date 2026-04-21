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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestInternModelID(t *testing.T) {
	used := map[string]int{}

	got := internModelID("meta/llama-3.1:8b", used)
	if got != "meta_llama-3.1_8b" {
		t.Fatalf("got %q", got)
	}

	// collision
	got2 := internModelID("meta/llama-3.1:8b", used)
	if got2 != "meta_llama-3.1_8b_2" {
		t.Fatalf("got2 %q", got2)
	}
	got3 := internModelID("meta/llama-3.1:8b", used)
	if got3 != "meta_llama-3.1_8b_3" {
		t.Fatalf("got3 %q", got3)
	}

	// base becomes empty -> unknown
	got4 := internModelID("////", used)
	if got4 != "_" {
		t.Fatalf("got4 %q", got4)
	}

	got5 := internModelID("", used)
	if got5 != "unknown" {
		t.Fatalf("got5 %q", got5)
	}
}

func TestPlanAccumulator_AppendAndFinalize_WritesLittleEndian16Bytes(t *testing.T) {
	root := t.TempDir()
	acc := newPlanAccumulator(root)

	model := "m1"
	e1 := planEntry{Offset: 123, Length: 456, PrefixHash: 99}
	e2 := planEntry{Offset: 999999, Length: 1, PrefixHash: 42}

	acc.Append(model, e1)
	acc.Append(model, e2)

	if err := acc.Finalize([]string{model}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	path := filepath.Join(root, "plans", model+".plan")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) != 32 {
		t.Fatalf("len=%d want 32", len(b))
	}

	readEntry := func(off int) planEntry {
		var buf [planEntrySize]byte
		copy(buf[:], b[off:off+planEntrySize])
		return unmarshalPlanEntry(buf)
	}

	// entries should be sorted by PrefixHash: e2 (42) before e1 (99)
	got0 := readEntry(0)
	got1 := readEntry(16)
	if got0 != e2 {
		t.Fatalf("entry0=%+v want %+v (sorted by PrefixHash)", got0, e2)
	}
	if got1 != e1 {
		t.Fatalf("entry1=%+v want %+v (sorted by PrefixHash)", got1, e1)
	}
}

func TestPlanAccumulator_MultipleModels_Finalize(t *testing.T) {
	root := t.TempDir()
	acc := newPlanAccumulator(root)

	acc.Append("a", planEntry{Offset: 0, Length: 10, PrefixHash: 5})
	acc.Append("b", planEntry{Offset: 10, Length: 20, PrefixHash: 3})
	acc.Append("a", planEntry{Offset: 30, Length: 40, PrefixHash: 1})

	if err := acc.Finalize([]string{"a", "b", "missing"}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// tmp files should be gone
	if _, err := os.Stat(filepath.Join(root, "plans", "a.plan.tmp")); err == nil {
		t.Fatalf("a.plan.tmp should not exist after Finalize")
	}
	if _, err := os.Stat(filepath.Join(root, "plans", "b.plan.tmp")); err == nil {
		t.Fatalf("b.plan.tmp should not exist after Finalize")
	}

	// final files should exist
	if _, err := os.Stat(filepath.Join(root, "plans", "a.plan")); err != nil {
		t.Fatalf("a.plan missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "plans", "b.plan")); err != nil {
		t.Fatalf("b.plan missing: %v", err)
	}

	// verify model "a" entries are sorted by PrefixHash
	entries, err := readPlanEntries(filepath.Join(root, "plans", "a.plan"))
	if err != nil {
		t.Fatalf("readPlanEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries=%d want 2", len(entries))
	}
	if entries[0].PrefixHash != 1 || entries[1].PrefixHash != 5 {
		t.Fatalf("entries not sorted by PrefixHash: %+v", entries)
	}
}

func TestWriteModelMapFile_AtomicWrite(t *testing.T) {
	root := t.TempDir()
	m := modelMapFile{
		ModelToSafe: map[string]string{"m": "m_safe"},
		SafeToModel: map[string]string{"m_safe": "m"},
		LineCount:   123,
	}

	if err := writeModelMapFile(root, m); err != nil {
		t.Fatalf("writeModelMapFile: %v", err)
	}

	finalPath := filepath.Join(root, modelMapFileName)
	tmpPath := finalPath + ".tmp"

	if _, err := os.Stat(tmpPath); err == nil {
		t.Fatalf("tmp should not exist")
	}

	b, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var got modelMapFile
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.LineCount != m.LineCount {
		t.Fatalf("LineCount=%d want %d", got.LineCount, m.LineCount)
	}
	if got.ModelToSafe["m"] != "m_safe" {
		t.Fatalf("ModelToSafe mismatch: %+v", got.ModelToSafe)
	}
	if got.SafeToModel["m_safe"] != "m" {
		t.Fatalf("SafeToModel mismatch: %+v", got.SafeToModel)
	}
}

func TestReadPlanEntries_NonexistentFile(t *testing.T) {
	_, err := readPlanEntries(filepath.Join(t.TempDir(), "nonexistent.plan"))
	if err == nil {
		t.Fatalf("expected error for nonexistent file")
	}
}

func TestReadPlanEntries_TruncatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.plan")
	if err := os.WriteFile(path, make([]byte, planEntrySize+3), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := readPlanEntries(path)
	if err == nil {
		t.Fatalf("expected error for file size not multiple of planEntrySize")
	}
}

func TestReadPlanEntries_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.plan")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	entries, err := readPlanEntries(path)
	if err != nil {
		t.Fatalf("unexpected error for empty file: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}
}

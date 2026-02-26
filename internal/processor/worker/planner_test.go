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

func TestPlanWriter_AppendEntry_WritesLittleEndian12Bytes(t *testing.T) {
	root := t.TempDir()
	w := newPlanWriter(root, 10)

	model := "m1"
	e1 := planEntry{Offset: 123, Length: 456}
	e2 := planEntry{Offset: 999999, Length: 1}

	if err := w.AppendEntry(model, e1); err != nil {
		t.Fatalf("AppendEntry e1: %v", err)
	}
	if err := w.AppendEntry(model, e2); err != nil {
		t.Fatalf("AppendEntry e2: %v", err)
	}

	// close to ensure data flush
	if err := w.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}

	path := filepath.Join(root, "plans", model+".plan.tmp")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) != 24 {
		t.Fatalf("len=%d want 24", len(b))
	}

	readEntry := func(off int) planEntry {
		chunk := b[off : off+12]
		o := int64(binary.LittleEndian.Uint64(chunk[0:8]))
		l := binary.LittleEndian.Uint32(chunk[8:12])
		return planEntry{Offset: o, Length: l}
	}

	if got := readEntry(0); got != e1 {
		t.Fatalf("entry1=%+v want %+v", got, e1)
	}
	if got := readEntry(12); got != e2 {
		t.Fatalf("entry2=%+v want %+v", got, e2)
	}
}

func TestPlanWriter_EvictAndFinalize(t *testing.T) {
	root := t.TempDir()
	w := newPlanWriter(root, 1) // force eviction

	// create two model files
	if err := w.AppendEntry("a", planEntry{Offset: 0, Length: 10}); err != nil {
		t.Fatalf("AppendEntry a: %v", err)
	}
	if err := w.AppendEntry("b", planEntry{Offset: 10, Length: 20}); err != nil {
		t.Fatalf("AppendEntry b: %v", err)
	}

	// reopen a and append again (ensures eviction doesn't break reopen)
	if err := w.AppendEntry("a", planEntry{Offset: 30, Length: 40}); err != nil {
		t.Fatalf("AppendEntry a2: %v", err)
	}

	if err := w.Finalize([]string{"a", "b", "missing"}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}

	// tmp files should be gone (best effort; if a model had no tmp, it's fine)
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

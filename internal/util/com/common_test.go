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

package com

import (
	"strings"
	"testing"
)

func TestNewFileID(t *testing.T) {
	id := NewFileID()
	if !strings.HasPrefix(id, "file_") {
		t.Errorf("NewFileID() = %q, want prefix %q", id, "file_")
	}
	if len(strings.TrimPrefix(id, "file_")) == 0 {
		t.Errorf("NewFileID() suffix is empty")
	}
	if id2 := NewFileID(); id == id2 {
		t.Errorf("NewFileID() returned the same ID twice: %q", id)
	}
}

func TestNewBatchID(t *testing.T) {
	id := NewBatchID()
	if !strings.HasPrefix(id, "batch_") {
		t.Errorf("NewBatchID() = %q, want prefix %q", id, "batch_")
	}
	if len(strings.TrimPrefix(id, "batch_")) == 0 {
		t.Errorf("NewBatchID() suffix is empty")
	}
	if id2 := NewBatchID(); id == id2 {
		t.Errorf("NewBatchID() returned the same ID twice: %q", id)
	}
}

func TestRandString(t *testing.T) {
	for _, n := range []int{0, 1, 10, 32} {
		s := RandString(n)
		if len(s) != n {
			t.Errorf("RandString(%d) len = %d, want %d", n, len(s), n)
		}
	}
	if s1, s2 := RandString(16), RandString(16); s1 == s2 {
		t.Errorf("RandString(16) returned the same string twice: %q", s1)
	}
}

func TestGetFolderNameByTenantID(t *testing.T) {
	tests := []struct {
		name       string
		tenantID   string
		wantErr    bool
		wantLen    int
		wantPrefix string
	}{
		{
			name:       "valid tenant ID",
			tenantID:   "tenant-abc",
			wantErr:    false,
			wantLen:    63,
			wantPrefix: "t-",
		},
		{
			name:     "empty tenant ID returns error",
			tenantID: "",
			wantErr:  true,
		},
		{
			name:       "deterministic: same input same output",
			tenantID:   "tenant-xyz",
			wantErr:    false,
			wantLen:    63,
			wantPrefix: "t-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetFolderNameByTenantID(tt.tenantID)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetFolderNameByTenantID(%q) err = %v, wantErr %v", tt.tenantID, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != tt.wantLen {
				t.Errorf("len = %d, want %d", len(got), tt.wantLen)
			}
			if !strings.HasPrefix(got, tt.wantPrefix) {
				t.Errorf("result %q does not have prefix %q", got, tt.wantPrefix)
			}
			// deterministic: same input → same output
			got2, _ := GetFolderNameByTenantID(tt.tenantID)
			if got != got2 {
				t.Errorf("not deterministic: %q != %q", got, got2)
			}
		})
	}

	// different tenant IDs must produce different folder names
	a, _ := GetFolderNameByTenantID("tenant-1")
	b, _ := GetFolderNameByTenantID("tenant-2")
	if a == b {
		t.Errorf("different tenants produced the same folder name: %q", a)
	}
}

func TestSameMembersInStrSlice(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{"both empty", []string{}, []string{}, true},
		{"identical", []string{"x", "y"}, []string{"x", "y"}, true},
		{"different order", []string{"a", "b", "c"}, []string{"c", "a", "b"}, true},
		{"duplicates match", []string{"a", "a"}, []string{"a", "a"}, true},
		{"different lengths", []string{"a"}, []string{"a", "b"}, false},
		{"different elements", []string{"a", "b"}, []string{"a", "c"}, false},
		{"duplicate mismatch", []string{"a", "a"}, []string{"a", "b"}, false},
		{"nil vs empty", nil, []string{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameMembersInStrSlice(tt.a, tt.b); got != tt.want {
				t.Errorf("SameMembersInStrSlice(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

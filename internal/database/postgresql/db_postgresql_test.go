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

package postgresql

import "testing"

func TestShortSpanName(t *testing.T) {
	tests := []struct {
		sql  string
		want string
	}{
		{"SELECT id, tenant_id FROM batch_items WHERE tenant_id = $1 ORDER BY id OFFSET $2 LIMIT $3", "select_batch_items"},
		{"SELECT id, tenant_id, purpose FROM file_items WHERE id = ANY($1) AND tenant_id = $2 ORDER BY id OFFSET $3 LIMIT $4", "select_file_items"},
		{"INSERT INTO batch_items (id, tenant_id, expiry, tags, spec, status) VALUES ($1, $2, $3, $4, $5, $6)", "insert_batch_items"},
		{"INSERT INTO file_items (id, tenant_id) VALUES ($1, $2)", "insert_file_items"},
		{"UPDATE batch_items SET status = $1, tags = $2 WHERE id = $3", "update_batch_items"},
		{"UPDATE file_items SET status = $1 WHERE id = $2", "update_file_items"},
		{"DELETE FROM file_items WHERE id = ANY($1) RETURNING id", "delete_file_items"},
		{"DELETE FROM batch_items WHERE id = ANY($1) RETURNING id", "delete_batch_items"},
		{"", "db_query"},
		{"SELECT", "select"},
		{"COMMIT", "commit"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := shortSpanName(tt.sql); got != tt.want {
				t.Errorf("shortSpanName(%q) = %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

func TestTokenAfter(t *testing.T) {
	tests := []struct {
		name    string
		tokens  []string
		keyword string
		want    string
	}{
		{"found", []string{"SELECT", "id", "FROM", "batch_items", "WHERE"}, "FROM", "batch_items"},
		{"case insensitive", []string{"delete", "from", "file_items"}, "FROM", "file_items"},
		{"not found", []string{"SELECT", "id"}, "FROM", ""},
		{"keyword at end", []string{"SELECT", "FROM"}, "FROM", ""},
		{"empty", []string{}, "FROM", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenAfter(tt.tokens, tt.keyword); got != tt.want {
				t.Errorf("tokenAfter(%v, %q) = %q, want %q", tt.tokens, tt.keyword, got, tt.want)
			}
		})
	}
}

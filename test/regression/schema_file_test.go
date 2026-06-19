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

package regression

import (
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

func TestSchemaCompat_File(t *testing.T) {
	t.Run("FileObject", func(t *testing.T) {
		assertRoundTrip[openai.FileObject](t, "file_object.golden.json")
	})

	t.Run("ListFilesResponse", func(t *testing.T) {
		assertRoundTrip[openai.ListFilesResponse](t, "file_list.golden.json")
	})

	t.Run("FileDeleteResponse", func(t *testing.T) {
		assertRoundTrip[openai.FileDeleteResponse](t, "file_delete.golden.json")
	})
}

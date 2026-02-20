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

package api

// FileItem is the database item type
type FileItem struct {
	BaseIndexes
	// Purpose is the intended purpose of the file (e.g., "batch", "batch_output").
	Purpose string
	BaseContents
}

// FileQuery is the queryItem used to query the DB
type FileQuery struct {
	BaseQuery
	Purpose string
}

// FileDBClient is the typed database client for file objects.
type FileDBClient = DBClient[FileItem, FileQuery]

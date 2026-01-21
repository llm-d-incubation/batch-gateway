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

// The file provides HTTP handlers for file-related API endpoints.
// It implements the OpenAI compatible Files API endpoints for uploading, downloading, listing, and deleting files.
package files

import (
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
)

type FilesApiHandler struct {
}

func NewFilesApiHandler() *FilesApiHandler {
	return &FilesApiHandler{}
}

func (c *FilesApiHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodPost,
			Pattern:     "/v1/files",
			HandlerFunc: c.CreateFile,
		},
		{
			Method:      http.MethodDelete,
			Pattern:     "/v1/files/{file_id}",
			HandlerFunc: c.DeleteFile,
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/files/{file_id}/content",
			HandlerFunc: c.DownloadFile,
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/files",
			HandlerFunc: c.ListFiles,
		},
		{
			Method:      http.MethodGet,
			Pattern:     "/v1/files/{file_id}",
			HandlerFunc: c.RetrieveFile,
		},
	}
}

func (c *FilesApiHandler) CreateFile(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *FilesApiHandler) DeleteFile(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *FilesApiHandler) DownloadFile(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *FilesApiHandler) ListFiles(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

func (c *FilesApiHandler) RetrieveFile(w http.ResponseWriter, r *http.Request) {
	common.WriteNotImplementedError(r.Context(), w)
}

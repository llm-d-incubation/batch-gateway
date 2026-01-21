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

// The file provides HTTP handlers for health check endpoints.
// It implements simple health check endpoints for monitoring server status.
package health

import (
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
)

const (
	HealthPath = "/health"
)

type HealthApiHandler struct {
}

func NewHealthApiHandler() *HealthApiHandler {
	return &HealthApiHandler{}
}

func (c *HealthApiHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodGet,
			Pattern:     HealthPath,
			HandlerFunc: c.HealthHandler,
		},
		{
			Method:      http.MethodHead,
			Pattern:     HealthPath,
			HandlerFunc: c.HealthHandler,
		},
	}
}

func (c *HealthApiHandler) HealthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

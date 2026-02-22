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

// The file provides HTTP handlers for readiness check endpoints.
package readiness

import (
	"net/http"
	"sync/atomic"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
)

const (
	ReadyPath = "/ready"
)

type ReadinessApiHandler struct {
	// serverReady indicates if the HTTP server has started accepting connections
	serverReady *atomic.Bool
}

func NewReadinessApiHandler(serverReady *atomic.Bool) *ReadinessApiHandler {
	return &ReadinessApiHandler{
		serverReady: serverReady,
	}
}

func (c *ReadinessApiHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodGet,
			Pattern:     ReadyPath,
			HandlerFunc: c.ReadyHandler,
		},
		{
			Method:      http.MethodHead,
			Pattern:     ReadyPath,
			HandlerFunc: c.ReadyHandler,
		},
	}
}

func (c *ReadinessApiHandler) ReadyHandler(w http.ResponseWriter, r *http.Request) {
	if !c.serverReady.Load() {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("not ready"))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

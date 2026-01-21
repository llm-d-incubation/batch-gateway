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

// The file provides the HTTP handler for serving Prometheus metrics.
package metrics

import (
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	MetricsPath = "/metrics"
)

type MetricsApiHandler struct {
}

func NewMetricsApiHandler() *MetricsApiHandler {
	return &MetricsApiHandler{}
}

func (c *MetricsApiHandler) GetRoutes() []common.Route {
	return []common.Route{
		{
			Method:      http.MethodGet,
			Pattern:     MetricsPath,
			HandlerFunc: c.MetricsHandler,
		},
	}
}

func (c *MetricsApiHandler) MetricsHandler(w http.ResponseWriter, r *http.Request) {
	promhttp.Handler().ServeHTTP(w, r)
}

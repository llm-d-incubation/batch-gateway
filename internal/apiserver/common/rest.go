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

// The file provides shared utilities for the REST API.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"k8s.io/klog/v2"
)

type Route struct {
	Method      string
	Pattern     string
	HandlerFunc http.HandlerFunc
}

type ApiHandler interface {
	GetRoutes() []Route
}

func RegisterHandler(mux *http.ServeMux, h ApiHandler) {
	routes := h.GetRoutes()
	for _, route := range routes {
		pattern := route.Method + " " + route.Pattern
		mux.HandleFunc(pattern, route.HandlerFunc)
	}
}

func WriteJSONResponse(ctx context.Context, w http.ResponseWriter, status int, obj interface{}) {
	logger := klog.FromContext(ctx)

	w.Header().Set("Content-Type", "application/json")

	data, err := json.Marshal(obj)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"Internal Server Error"}`))
		logger.Error(err, "failed to marshal JSON response", "status", status, "type", fmt.Sprintf("%T", obj))
		return
	}

	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		logger.Error(err, "failed to write response", "status", status, "dataLen", len(data))
		return
	}
}

func WriteOpenAIError(ctx context.Context, w http.ResponseWriter, oaiErr openai.Error) {
	errorResp := openai.ErrorResponse{
		Error: oaiErr,
	}

	WriteJSONResponse(ctx, w, oaiErr.Code, errorResp)
}

func WriteNotImplementedError(ctx context.Context, w http.ResponseWriter) {
	oaiErr := openai.NewError(http.StatusNotImplemented, "", "This is not yet implemented", nil)

	WriteOpenAIError(ctx, w, oaiErr)

}

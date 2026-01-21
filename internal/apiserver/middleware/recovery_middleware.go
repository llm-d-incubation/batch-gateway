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

// The file implements panic recovery middleware that catches and handles panics.
package middleware

import (
	"fmt"
	"net/http"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

// RecoveryMiddleware recovers from panics and returns a JSON error response
func RecoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				var panicErr error
				switch e := err.(type) {
				case string:
					panicErr = fmt.Errorf("%s", e)
				case error:
					panicErr = e
				default:
					panicErr = fmt.Errorf("%v", e)
				}

				logger := logging.GetRequestLogger(r)
				logger.Error(panicErr, "handler panic",
					"method", r.Method,
					"path", r.URL.Path,
					//"stack", string(debug.Stack()),
				)
				requestID := GetRequestIDFromContext(r.Context())
				oaiErr := openai.NewError(http.StatusInternalServerError, "", "The server had an error while processing your request", &requestID)
				common.WriteOpenAIError(r.Context(), w, oaiErr)
			}
		}()

		next.ServeHTTP(w, r)
	})
}

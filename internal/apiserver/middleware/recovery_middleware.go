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
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

// Recovery returns a RouteMiddleware that recovers from panics and returns a JSON error response.
func Recovery(_ common.Route, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

				if !testing.Testing() || testing.Verbose() {
					logger := logging.FromRequest(r)
					logger.Error(panicErr, "handler panic",
						"method", r.Method,
						"path", r.URL.Path,
					)
				}

				requestID := common.GetRequestIDFromContext(r.Context())
				oaiErr := openai.NewAPIError(http.StatusInternalServerError, "", "The server had an error while processing your request", &requestID)
				common.WriteAPIError(w, r, oaiErr)
			}
		}()

		next(w, r)
	}
}

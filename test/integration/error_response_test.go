//go:build integration

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

package integration

import (
	"net/http"
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
)

func TestNotFoundRoute(t *testing.T) {
	ts := newTestServer(t)

	resp := ts.doRequest(t, http.MethodGet, "/v1/nonexistent")
	if resp.StatusCode != http.StatusNotFound {
		body := readBody(t, resp)
		t.Fatalf("expected 404, got %d: %s", resp.StatusCode, body)
	}

	errResp := decodeJSON[openai.ErrorResponse](t, resp)
	if errResp.Error.Type != "not_found_error" {
		t.Errorf("expected error type not_found_error, got %q", errResp.Error.Type)
	}
	if errResp.Error.Message == "" {
		t.Error("expected non-empty error message")
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)

	// Any valid request should return security headers
	resp := ts.doRequest(t, http.MethodGet, "/v1/batches?limit=10")
	defer resp.Body.Close()

	headers := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"X-Xss-Protection":       "1; mode=block",
	}

	for name, want := range headers {
		got := resp.Header.Get(name)
		if got != want {
			t.Errorf("header %s: expected %q, got %q", name, want, got)
		}
	}
}

func TestRequestID(t *testing.T) {
	ts := newTestServer(t)

	t.Run("GeneratedWhenMissing", func(t *testing.T) {
		resp := ts.doRequest(t, http.MethodGet, "/v1/batches?limit=10")
		defer resp.Body.Close()

		requestID := resp.Header.Get("X-Request-Id")
		if requestID == "" {
			t.Error("expected X-Request-Id header to be set")
		}
	})

	t.Run("EchoedWhenProvided", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/batches?limit=10", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(testTenantHeader, testTenantID)
		req.Header.Set("X-Request-Id", "custom-req-id-123")

		resp, err := ts.Client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if got := resp.Header.Get("X-Request-Id"); got != "custom-req-id-123" {
			t.Errorf("expected X-Request-Id=custom-req-id-123, got %q", got)
		}
	})
}

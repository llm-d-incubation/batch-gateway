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

package http

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewHTTPClient_Defaults tests that NewHTTPClient sets proper defaults
func TestNewHTTPClient_Defaults(t *testing.T) {
	config := Config{
		BaseURL: "http://localhost:8000",
	}

	client, err := NewHTTPClient(config)
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	if client == nil {
		t.Fatal("Expected non-nil client")
	}

	// Verify transport settings (defaults should be applied)
	if client.transport == nil {
		t.Fatal("Expected non-nil Transport")
	}
	if client.transport.MaxIdleConns != 100 {
		t.Errorf("Expected Transport.MaxIdleConns=100, got %d", client.transport.MaxIdleConns)
	}
	if client.transport.MaxIdleConnsPerHost != 100 {
		t.Errorf("Expected Transport.MaxIdleConnsPerHost=100, got %d", client.transport.MaxIdleConnsPerHost)
	}
	if client.transport.ResponseHeaderTimeout != 5*time.Minute {
		t.Errorf("Expected Transport.ResponseHeaderTimeout=5m (same as default Timeout), got %v", client.transport.ResponseHeaderTimeout)
	}
}

// TestNewHTTPClient_CustomConfig tests NewHTTPClient with custom configuration
func TestNewHTTPClient_CustomConfig(t *testing.T) {
	config := Config{
		BaseURL:         "http://example.com",
		Timeout:         10 * time.Second,
		MaxIdleConns:    50,
		IdleConnTimeout: 60 * time.Second,
		APIKey:          "test-api-key",
	}

	client, err := NewHTTPClient(config)
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	if client == nil {
		t.Fatal("Expected non-nil client")
	}

	// Client created successfully with custom settings
}

// TestNewHTTPClient_RetryDefaults tests retry configuration defaults
func TestNewHTTPClient_RetryDefaults(t *testing.T) {
	config := Config{
		BaseURL:    "http://localhost:8000",
		MaxRetries: 3,
	}

	client, err := NewHTTPClient(config)
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	if client == nil {
		t.Fatal("Expected non-nil client")
	}

	// Client created successfully with retry enabled (defaults applied internally)
}

// TestNewHTTPClient_RetryCustom tests custom retry configuration
func TestNewHTTPClient_RetryCustom(t *testing.T) {
	config := Config{
		BaseURL:        "http://localhost:8000",
		MaxRetries:     5,
		InitialBackoff: 500 * time.Millisecond,
		MaxBackoff:     30 * time.Second,
	}

	client, err := NewHTTPClient(config)
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	if client == nil {
		t.Fatal("Expected non-nil client")
	}

	// Client created successfully with custom retry settings
}

// TestPost_Success tests successful POST request
func TestPost_Success(t *testing.T) {
	requestBody := map[string]string{"key": "value"}
	expectedResponse := map[string]string{"result": "success"}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		// Verify headers
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type=application/json, got %s", r.Header.Get("Content-Type"))
		}

		// Verify request body
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("Failed to decode request body: %v", err)
		}
		if body["key"] != "value" {
			t.Errorf("Expected body key=value, got %v", body)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(expectedResponse)
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	respBody, statusCode, err := client.Post(context.Background(), "/test", requestBody, nil, "")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}

	var response map[string]string
	if err := json.Unmarshal(respBody, &response); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	if response["result"] != "success" {
		t.Errorf("Expected result=success, got %v", response)
	}
}

// TestPost_WithRequestID tests POST request with request ID header
func TestPost_WithRequestID(t *testing.T) {
	expectedRequestID := "test-request-123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID != expectedRequestID {
			t.Errorf("Expected X-Request-ID=%s, got %s", expectedRequestID, requestID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, expectedRequestID)
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}
}

// TestPost_WithCustomHeaders tests POST request with custom headers
func TestPost_WithCustomHeaders(t *testing.T) {
	customHeaders := map[string]string{
		"X-Custom-Header": "custom-value",
		"X-Another":       "another-value",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range customHeaders {
			if r.Header.Get(k) != v {
				t.Errorf("Expected header %s=%s, got %s", k, v, r.Header.Get(k))
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, customHeaders, "")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}
}

// TestPost_WithAPIKey tests that API key is included as Bearer token
func TestPost_WithAPIKey(t *testing.T) {
	expectedAPIKey := "test-api-key-123"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		expectedAuth := "Bearer " + expectedAPIKey
		if authHeader != expectedAuth {
			t.Errorf("Expected Authorization=%s, got %s", expectedAuth, authHeader)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{
		BaseURL: server.URL,
		APIKey:  expectedAPIKey,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, "")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}
}

// TestPost_ContextCancellation tests POST request with cancelled context
func TestPost_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, _, err = client.Post(ctx, "/test", nil, nil, "")
	if err == nil {
		t.Fatal("Expected error for cancelled context, got nil")
	}
}

// TestPost_ContextTimeout tests POST request with timeout
func TestPost_ContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{BaseURL: server.URL})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err = client.Post(ctx, "/test", nil, nil, "")
	if err == nil {
		t.Fatal("Expected timeout error, got nil")
	}
}

// TestPost_RetryOn500 tests that 500 errors are retried
func TestPost_RetryOn500(t *testing.T) {
	var attemptCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attemptCount.Add(1)
		if count < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error": {"message": "server error"}}`))
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result": "success"}`))
		}
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{
		BaseURL:        server.URL,
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, "test-retry")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}

	if attemptCount.Load() != 3 {
		t.Errorf("Expected 3 attempts, got %d", attemptCount.Load())
	}
}

// TestPost_RetryOn429 tests that 429 (rate limit) errors are retried
func TestPost_RetryOn429(t *testing.T) {
	var attemptCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := attemptCount.Add(1)
		if count < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error": {"message": "rate limit exceeded"}}`))
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"result": "success"}`))
		}
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{
		BaseURL:        server.URL,
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, "test-retry-429")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}

	if attemptCount.Load() != 2 {
		t.Errorf("Expected 2 attempts, got %d", attemptCount.Load())
	}
}

// TestPost_NoRetryOn400 tests that 400 errors are not retried
func TestPost_NoRetryOn400(t *testing.T) {
	var attemptCount atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attemptCount.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error": {"message": "bad request"}}`))
	}))
	defer server.Close()

	client, err := NewHTTPClient(Config{
		BaseURL:        server.URL,
		MaxRetries:     3,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, "test-no-retry")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", statusCode)
	}

	if attemptCount.Load() != 1 {
		t.Errorf("Expected 1 attempt (no retry), got %d", attemptCount.Load())
	}
}

// TestHandleErrorResponse_OpenAIFormat tests parsing OpenAI-style error response
func TestHandleErrorResponse_OpenAIFormat(t *testing.T) {
	body := []byte(`{"error": {"message": "Invalid API key", "type": "invalid_request_error"}}`)

	client, _ := NewHTTPClient(Config{BaseURL: "http://localhost"})
	clientErr := client.HandleErrorResponse(http.StatusUnauthorized, body)

	if clientErr == nil {
		t.Fatal("Expected non-nil error")
	}

	if clientErr.Category != ErrCategoryAuth {
		t.Errorf("Expected category AUTH_ERROR, got %s", clientErr.Category)
	}

	if clientErr.Message != "HTTP 401: Invalid API key" {
		t.Errorf("Expected message 'HTTP 401: Invalid API key', got %s", clientErr.Message)
	}
}

// TestHandleErrorResponse_PlainText tests parsing plain text error response
func TestHandleErrorResponse_PlainText(t *testing.T) {
	body := []byte("Internal Server Error")

	client, _ := NewHTTPClient(Config{BaseURL: "http://localhost"})
	clientErr := client.HandleErrorResponse(http.StatusInternalServerError, body)

	if clientErr == nil {
		t.Fatal("Expected non-nil error")
	}

	if clientErr.Category != ErrCategoryServer {
		t.Errorf("Expected category SERVER_ERROR, got %s", clientErr.Category)
	}

	if clientErr.Message != "HTTP 500: Internal Server Error" {
		t.Errorf("Expected message 'HTTP 500: Internal Server Error', got %s", clientErr.Message)
	}
}

// TestHandleErrorResponse_EmptyBody tests handling empty error response
func TestHandleErrorResponse_EmptyBody(t *testing.T) {
	body := []byte("")

	client, _ := NewHTTPClient(Config{BaseURL: "http://localhost"})
	clientErr := client.HandleErrorResponse(http.StatusBadGateway, body)

	if clientErr == nil {
		t.Fatal("Expected non-nil error")
	}

	if clientErr.Category != ErrCategoryServer {
		t.Errorf("Expected category SERVER_ERROR, got %s", clientErr.Category)
	}

	if clientErr.Message != "HTTP 502: " {
		t.Errorf("Expected message 'HTTP 502: ', got %s", clientErr.Message)
	}
}

// TestMapStatusCodeToCategory tests all status code mappings
func TestMapStatusCodeToCategory(t *testing.T) {
	tests := []struct {
		statusCode int
		expected   ErrorCategory
	}{
		{http.StatusBadRequest, ErrCategoryInvalidReq},          // 400
		{http.StatusUnauthorized, ErrCategoryAuth},              // 401
		{http.StatusForbidden, ErrCategoryAuth},                 // 403
		{http.StatusTooManyRequests, ErrCategoryRateLimit},      // 429
		{http.StatusInternalServerError, ErrCategoryServer},     // 500
		{http.StatusBadGateway, ErrCategoryServer},              // 502
		{http.StatusServiceUnavailable, ErrCategoryServer},      // 503
		{http.StatusGatewayTimeout, ErrCategoryServer},          // 504
		{http.StatusHTTPVersionNotSupported, ErrCategoryServer}, // 505 (other 5xx)
		{http.StatusNotFound, ErrCategoryUnknown},               // 404 (unmapped 4xx)
		{http.StatusTeapot, ErrCategoryUnknown},                 // 418 (unmapped)
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("status_%d", tt.statusCode), func(t *testing.T) {
			category := MapStatusCodeToCategory(tt.statusCode)
			if category != tt.expected {
				t.Errorf("For status %d, expected category %s, got %s", tt.statusCode, tt.expected, category)
			}
		})
	}
}

// TestBuildTLSConfig_Nil tests that BuildTLSConfig returns nil for default config
func TestBuildTLSConfig_Nil(t *testing.T) {
	config := Config{}
	tlsConfig, err := BuildTLSConfig(&config)

	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsConfig != nil {
		t.Error("Expected nil TLS config for default settings")
	}
}

// TestBuildTLSConfig_InsecureSkipVerify tests InsecureSkipVerify option
func TestBuildTLSConfig_InsecureSkipVerify(t *testing.T) {
	config := Config{
		TLSInsecureSkipVerify: true,
	}
	tlsConfig, err := BuildTLSConfig(&config)

	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("Expected non-nil TLS config")
	}

	if !tlsConfig.InsecureSkipVerify {
		t.Error("Expected InsecureSkipVerify=true")
	}
}

// TestBuildTLSConfig_CustomCA tests custom CA certificate
func TestBuildTLSConfig_CustomCA(t *testing.T) {
	// Create a temporary CA cert file
	tmpDir := t.TempDir()
	caCertFile := filepath.Join(tmpDir, "ca.crt")

	// Generate a valid self-signed certificate
	caCertPEM := generateTestCertificate(t)
	if err := os.WriteFile(caCertFile, caCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert file: %v", err)
	}

	config := Config{
		TLSCACertFile: caCertFile,
	}
	tlsConfig, err := BuildTLSConfig(&config)

	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("Expected non-nil TLS config")
	}

	if tlsConfig.RootCAs == nil {
		t.Error("Expected non-nil RootCAs")
	}
}

// TestBuildTLSConfig_CustomCA_FileNotFound tests error when CA cert file doesn't exist
func TestBuildTLSConfig_CustomCA_FileNotFound(t *testing.T) {
	config := Config{
		TLSCACertFile: "/nonexistent/ca.crt",
	}
	_, err := BuildTLSConfig(&config)

	if err == nil {
		t.Fatal("Expected error for missing CA cert file")
	}
}

// TestBuildTLSConfig_CustomCA_InvalidPEM tests error when CA cert is invalid
func TestBuildTLSConfig_CustomCA_InvalidPEM(t *testing.T) {
	tmpDir := t.TempDir()
	caCertFile := filepath.Join(tmpDir, "ca.crt")

	if err := os.WriteFile(caCertFile, []byte("not a valid PEM certificate"), 0644); err != nil {
		t.Fatalf("Failed to write invalid CA cert file: %v", err)
	}

	config := Config{
		TLSCACertFile: caCertFile,
	}
	_, err := BuildTLSConfig(&config)

	if err == nil {
		t.Fatal("Expected error for invalid PEM certificate")
	}
}

// TestBuildTLSConfig_ClientCert_BothRequired tests that both cert and key are required for mTLS
func TestBuildTLSConfig_ClientCert_BothRequired(t *testing.T) {
	tests := []struct {
		name     string
		certFile string
		keyFile  string
	}{
		{"only cert", "/path/to/cert.pem", ""},
		{"only key", "", "/path/to/key.pem"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{
				TLSClientCertFile: tt.certFile,
				TLSClientKeyFile:  tt.keyFile,
			}
			_, err := BuildTLSConfig(&config)

			if err == nil {
				t.Fatal("Expected error when only one of cert/key is specified")
			}
		})
	}
}

// TestBuildTLSConfig_TLSVersions tests TLS version constraints
func TestBuildTLSConfig_TLSVersions(t *testing.T) {
	config := Config{
		TLSMinVersion: tls.VersionTLS12,
		TLSMaxVersion: tls.VersionTLS13,
	}
	tlsConfig, err := BuildTLSConfig(&config)

	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("Expected non-nil TLS config")
	}

	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("Expected MinVersion=TLS1.2, got 0x%04x", tlsConfig.MinVersion)
	}

	if tlsConfig.MaxVersion != tls.VersionTLS13 {
		t.Errorf("Expected MaxVersion=TLS1.3, got 0x%04x", tlsConfig.MaxVersion)
	}
}

// TestBuildTLSConfig_CombinedOptions tests combination of TLS options
func TestBuildTLSConfig_CombinedOptions(t *testing.T) {
	tmpDir := t.TempDir()
	caCertFile := filepath.Join(tmpDir, "ca.crt")

	// Generate a valid self-signed certificate
	caCertPEM := generateTestCertificate(t)
	if err := os.WriteFile(caCertFile, caCertPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert file: %v", err)
	}

	config := Config{
		TLSCACertFile: caCertFile,
		TLSMinVersion: tls.VersionTLS12,
	}
	tlsConfig, err := BuildTLSConfig(&config)

	if err != nil {
		t.Fatalf("BuildTLSConfig failed: %v", err)
	}

	if tlsConfig == nil {
		t.Fatal("Expected non-nil TLS config")
	}

	if tlsConfig.RootCAs == nil {
		t.Error("Expected non-nil RootCAs")
	}

	if tlsConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("Expected MinVersion=TLS1.2, got 0x%04x", tlsConfig.MinVersion)
	}
}

// TestClientError_Error tests ClientError Error() method
func TestClientError_Error(t *testing.T) {
	err := &ClientError{
		Category: ErrCategoryAuth,
		Message:  "authentication failed",
		RawError: fmt.Errorf("invalid token"),
	}

	if err.Error() != "authentication failed" {
		t.Errorf("Expected error message 'authentication failed', got %s", err.Error())
	}
}

// TestClientError_IsRetryable tests ClientError IsRetryable() method
func TestClientError_IsRetryable(t *testing.T) {
	tests := []struct {
		category ErrorCategory
		expected bool
	}{
		{ErrCategoryRateLimit, true},
		{ErrCategoryServer, true},
		{ErrCategoryInvalidReq, false},
		{ErrCategoryAuth, false},
		{ErrCategoryParse, false},
		{ErrCategoryUnknown, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			err := &ClientError{Category: tt.category}
			if err.IsRetryable() != tt.expected {
				t.Errorf("For category %s, expected IsRetryable=%v, got %v", tt.category, tt.expected, err.IsRetryable())
			}
		})
	}
}

// TestNewHTTPClient_TLSInsecureSkipVerify_Integration tests insecure TLS in integration
func TestNewHTTPClient_TLSInsecureSkipVerify_Integration(t *testing.T) {
	// Create HTTPS test server with self-signed cert
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result": "success"}`))
	}))
	defer server.Close()

	// Client with InsecureSkipVerify should work
	client, err := NewHTTPClient(Config{
		BaseURL:               server.URL,
		TLSInsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, "")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}
}

// TestNewHTTPClient_TLSVerifyFails tests that TLS verification fails without InsecureSkipVerify
func TestNewHTTPClient_TLSVerifyFails(t *testing.T) {
	// Create HTTPS test server with self-signed cert
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result": "success"}`))
	}))
	defer server.Close()

	// Client without InsecureSkipVerify should fail on self-signed cert
	client, err := NewHTTPClient(Config{
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, _, err = client.Post(context.Background(), "/test", nil, nil, "")
	if err == nil {
		t.Fatal("Expected TLS verification error, got nil")
	}

	// Verify it's a certificate verification error
	if !isTLSError(err) {
		t.Errorf("Expected TLS error, got: %v", err)
	}
}

// TestNewHTTPClient_WithCustomCA_Integration tests custom CA certificate
func TestNewHTTPClient_WithCustomCA_Integration(t *testing.T) {
	// Create HTTPS test server
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"result": "success"}`))
	}))
	defer server.Close()

	// Extract the server's CA cert
	tmpDir := t.TempDir()
	caCertFile := filepath.Join(tmpDir, "ca.crt")

	serverCert := server.Certificate()
	caCertPool := x509.NewCertPool()
	caCertPool.AddCert(serverCert)

	// Write server cert as CA cert
	certPEM := pemEncodeCert(serverCert.Raw)
	if err := os.WriteFile(caCertFile, certPEM, 0644); err != nil {
		t.Fatalf("Failed to write CA cert: %v", err)
	}

	// Client with custom CA should work
	client, err := NewHTTPClient(Config{
		BaseURL:       server.URL,
		TLSCACertFile: caCertFile,
	})
	if err != nil {
		t.Fatalf("NewHTTPClient failed: %v", err)
	}

	_, statusCode, err := client.Post(context.Background(), "/test", nil, nil, "")
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}

	if statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusCode)
	}
}

// Helper functions

// generateTestCertificate generates a valid self-signed certificate for testing
func generateTestCertificate(t *testing.T) []byte {
	t.Helper()

	// Generate a private key
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	// Create certificate template
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("Failed to generate serial number: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	// Create self-signed certificate
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}

	// Encode to PEM
	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})

	return certPEM
}

func isTLSError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return containsString(errStr, "tls") ||
		containsString(errStr, "certificate") ||
		containsString(errStr, "x509")
}

func containsString(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func pemEncodeCert(derBytes []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})
}

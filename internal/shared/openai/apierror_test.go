package openai

import (
	"net/http"
	"testing"
)

func TestErrorCodeToType(t *testing.T) {
	tests := []struct {
		code     int
		wantType string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "authentication_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusUnprocessableEntity, "invalid_request_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusInternalServerError, "server_error"},
		{http.StatusBadGateway, "server_error"},
		{http.StatusServiceUnavailable, "server_error"},
		{http.StatusGatewayTimeout, "server_error"},
		{http.StatusNotImplemented, "server_error"},
		{http.StatusConflict, "api_error"},
		{http.StatusTeapot, "api_error"},
	}

	for _, tt := range tests {
		got := ErrorCodeToType(tt.code)
		if got != tt.wantType {
			t.Errorf("ErrorCodeToType(%d) = %q, want %q", tt.code, got, tt.wantType)
		}
	}
}

func TestNewAPIError(t *testing.T) {
	t.Run("default type derived from status", func(t *testing.T) {
		err := NewAPIError(http.StatusBadRequest, "", "bad request", nil)
		if err.HTTPStatus != http.StatusBadRequest {
			t.Errorf("HTTPStatus = %d, want %d", err.HTTPStatus, http.StatusBadRequest)
		}
		if err.Type != "invalid_request_error" {
			t.Errorf("Type = %q, want %q", err.Type, "invalid_request_error")
		}
		if err.Code != nil {
			t.Errorf("Code = %v, want nil", err.Code)
		}
	})

	t.Run("explicit errorType overrides default", func(t *testing.T) {
		err := NewAPIError(http.StatusBadRequest, "custom_error", "bad request", nil)
		if err.Type != "custom_error" {
			t.Errorf("Type = %q, want %q", err.Type, "custom_error")
		}
	})

	t.Run("code is set when non-empty", func(t *testing.T) {
		err := NewAPIError(http.StatusNotFound, "", "not found", nil)
		if err.Code != nil {
			t.Errorf("Code = %v, want nil for empty code", err.Code)
		}
	})
}

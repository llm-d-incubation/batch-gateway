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

package batch_test

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch"
)

// ExampleHTTPInferenceClient_chatCompletion demonstrates making a chat completion request
func ExampleHTTPInferenceClient_chatCompletion() {
	// Create client configuration with retry enabled
	config := batch.HTTPInferenceClientConfig{
		BaseURL:         "http://localhost:8000",
		Timeout:         30 * time.Second,
		MaxIdleConns:    100,
		IdleConnTimeout: 90 * time.Second,
		APIKey:          "", // Optional: set if authentication is required

		// Retry configuration (optional)
		MaxRetries:     3,               // Retry up to 3 times
		InitialBackoff: 1 * time.Second, // Start with 1 second backoff
		MaxBackoff:     60 * time.Second, // Max 60 seconds between retries
		BackoffFactor:  2.0,              // Double the backoff each time
		JitterFraction: 0.1,              // Add ±10% jitter
	}

	// Create the client
	client := batch.NewHTTPInferenceClient(config)

	// Prepare the inference request
	req := &batch.InferenceRequest{
		RequestID: "chat-request-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{
					"role":    "system",
					"content": "You are a helpful assistant.",
				},
				{
					"role":    "user",
					"content": "What is the capital of France?",
				},
			},
			"temperature": 0.7,
			"max_tokens":  100,
		},
	}

	// Make the inference request
	ctx := context.Background()
	resp, err := client.Generate(ctx, req)
	if err != nil {
		if err.IsRetryable() {
			fmt.Printf("Retryable error: %s (category: %s)\n", err.Message, err.Category)
			// Implement retry logic here
		} else {
			fmt.Printf("Non-retryable error: %s (category: %s)\n", err.Message, err.Category)
		}
		return
	}

	// Parse the response
	var result map[string]interface{}
	if unmarshalErr := json.Unmarshal(resp.Response, &result); unmarshalErr != nil {
		fmt.Printf("Failed to parse response: %v\n", unmarshalErr)
		return
	}

	fmt.Printf("Response ID: %s\n", resp.RequestID)
	fmt.Printf("Model: %s\n", result["model"])
	if choices, ok := result["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				fmt.Printf("Assistant: %s\n", message["content"])
			}
		}
	}
}

// ExampleHTTPInferenceClient_textCompletion demonstrates making a text completion request
func ExampleHTTPInferenceClient_textCompletion() {
	// Create the client with minimal configuration
	config := batch.HTTPInferenceClientConfig{
		BaseURL: "http://localhost:8000",
	}
	client := batch.NewHTTPInferenceClient(config)

	// Prepare the inference request with prompt
	req := &batch.InferenceRequest{
		RequestID: "completion-request-001",
		Model:     "gpt-3.5-turbo",
		Params: map[string]interface{}{
			"model":       "gpt-3.5-turbo",
			"prompt":      "Once upon a time",
			"max_tokens":  50,
			"temperature": 0.8,
		},
	}

	// Make the inference request
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.Generate(ctx, req)
	if err != nil {
		fmt.Printf("Error: %s\n", err.Message)
		return
	}

	fmt.Printf("Request ID: %s\n", resp.RequestID)
	fmt.Printf("Response received: %d bytes\n", len(resp.Response))
}

// ExampleHTTPInferenceClient_withAuthentication demonstrates using API key authentication
func ExampleHTTPInferenceClient_withAuthentication() {
	// Create client with API key
	config := batch.HTTPInferenceClientConfig{
		BaseURL: "http://api.example.com",
		APIKey:  "sk-your-api-key-here",
		Timeout: 1 * time.Minute,
	}
	client := batch.NewHTTPInferenceClient(config)

	req := &batch.InferenceRequest{
		RequestID: "auth-request-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": "Hello!",
				},
			},
		},
	}

	ctx := context.Background()
	resp, err := client.Generate(ctx, req)
	if err != nil {
		if err.Category == batch.ErrCategoryAuth {
			fmt.Println("Authentication failed - check your API key")
		} else {
			fmt.Printf("Error: %s\n", err.Message)
		}
		return
	}

	fmt.Printf("Success! Response ID: %s\n", resp.RequestID)
}

// ExampleHTTPInferenceClient_toolCalls demonstrates making a request with function/tool calls
func ExampleHTTPInferenceClient_toolCalls() {
	config := batch.HTTPInferenceClientConfig{
		BaseURL: "http://localhost:8000",
	}
	client := batch.NewHTTPInferenceClient(config)

	req := &batch.InferenceRequest{
		RequestID: "tool-call-request-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": "What is the weather like in Boston today?",
				},
			},
			"tools": []map[string]interface{}{
				{
					"type": "function",
					"function": map[string]interface{}{
						"name":        "get_current_weather",
						"description": "Get the current weather in a given location",
						"parameters": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"location": map[string]interface{}{
									"type":        "string",
									"description": "The city and state, e.g. San Francisco, CA",
								},
								"unit": map[string]interface{}{
									"type": "string",
									"enum": []string{"celsius", "fahrenheit"},
								},
							},
							"required": []string{"location"},
						},
					},
				},
			},
			"tool_choice": "auto",
		},
	}

	ctx := context.Background()
	resp, err := client.Generate(ctx, req)
	if err != nil {
		fmt.Printf("Error: %s\n", err.Message)
		return
	}

	var result map[string]interface{}
	json.Unmarshal(resp.Response, &result)

	if choices, ok := result["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
					fmt.Printf("Tool calls requested: %d\n", len(toolCalls))
				}
			}
		}
	}
}

// ExampleHTTPInferenceClient_errorHandling demonstrates error handling patterns
func ExampleHTTPInferenceClient_errorHandling() {
	// Create client without retry to show manual error handling
	// (In production, you'd typically enable retry)
	config := batch.HTTPInferenceClientConfig{
		BaseURL:    "http://localhost:8000",
		Timeout:    10 * time.Second,
		MaxRetries: 0, // Disable automatic retry for this example
	}
	client := batch.NewHTTPInferenceClient(config)

	req := &batch.InferenceRequest{
		RequestID: "error-handling-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": "Test request",
				},
			},
		},
	}

	ctx := context.Background()
	resp, err := client.Generate(ctx, req)
	if err != nil {
		// Check error category and handle accordingly
		switch err.Category {
		case batch.ErrCategoryRateLimit:
			fmt.Println("Rate limit exceeded - implement exponential backoff")
			// Retry with exponential backoff
		case batch.ErrCategoryServer:
			fmt.Println("Server error - retry with backoff")
			// Retry logic
		case batch.ErrCategoryAuth:
			fmt.Println("Authentication error - check credentials")
			// Don't retry, fix authentication
		case batch.ErrCategoryInvalidReq:
			fmt.Println("Invalid request - fix the request parameters")
			// Don't retry, fix the request
		case batch.ErrCategoryUnknown:
			fmt.Println("Unknown error - log and investigate")
			// Log for investigation
		}

		// Check if error is retryable
		if err.IsRetryable() {
			fmt.Println("This error can be retried")
		} else {
			fmt.Println("This error should not be retried")
		}

		return
	}

	fmt.Printf("Success! Response ID: %s\n", resp.RequestID)
}

// ExampleHTTPInferenceClient_withRetry demonstrates automatic retry with exponential backoff
func ExampleHTTPInferenceClient_withRetry() {
	// Create client with retry configuration
	config := batch.HTTPInferenceClientConfig{
		BaseURL: "http://localhost:8000",

		// Retry configuration
		MaxRetries:     3,               // Retry up to 3 times (total 4 attempts)
		InitialBackoff: 1 * time.Second, // Start with 1 second backoff
		MaxBackoff:     60 * time.Second, // Maximum 60 seconds between retries
		BackoffFactor:  2.0,              // Double the backoff each time (1s, 2s, 4s, ...)
		JitterFraction: 0.1,              // Add ±10% random jitter to prevent thundering herd
	}
	client := batch.NewHTTPInferenceClient(config)

	req := &batch.InferenceRequest{
		RequestID: "retry-example-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": "Hello!",
				},
			},
		},
	}

	// The client will automatically retry on:
	// - Rate limit errors (429)
	// - Server errors (500, 502, 503, 504)
	//
	// It will NOT retry on:
	// - Bad request errors (400)
	// - Authentication errors (401, 403)
	// - Other client errors (4xx)
	ctx := context.Background()
	resp, err := client.Generate(ctx, req)
	if err != nil {
		// If we get an error after all retries are exhausted
		fmt.Printf("Request failed after retries: %s (category: %s)\n", err.Message, err.Category)
		return
	}

	fmt.Printf("Success! Response ID: %s\n", resp.RequestID)
}

// ExampleHTTPInferenceClient_customBackoff demonstrates custom retry backoff configuration
func ExampleHTTPInferenceClient_customBackoff() {
	// For rate-limited APIs, you might want aggressive retry with longer backoff
	config := batch.HTTPInferenceClientConfig{
		BaseURL:        "http://api.rate-limited.com",
		MaxRetries:     5,                // More retries for rate limits
		InitialBackoff: 5 * time.Second,  // Start with longer backoff
		MaxBackoff:     5 * time.Minute,  // Allow up to 5 minutes between retries
		BackoffFactor:  3.0,               // More aggressive exponential backoff
		JitterFraction: 0.2,               // More jitter (±20%)
	}
	client := batch.NewHTTPInferenceClient(config)

	req := &batch.InferenceRequest{
		RequestID: "custom-backoff-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{"role": "user", "content": "Test"},
			},
		},
	}

	// With this configuration, backoff will be approximately:
	// Attempt 1: immediate
	// Attempt 2: ~5s (5s * 3^0 ± 20%)
	// Attempt 3: ~15s (5s * 3^1 ± 20%)
	// Attempt 4: ~45s (5s * 3^2 ± 20%)
	// Attempt 5: ~135s (5s * 3^3 ± 20%)
	// Attempt 6: ~300s (capped at MaxBackoff)
	ctx := context.Background()
	resp, err := client.Generate(ctx, req)
	if err != nil {
		fmt.Printf("Error: %s\n", err.Message)
		return
	}

	fmt.Printf("Success! Response ID: %s\n", resp.RequestID)
}

// ExampleHTTPInferenceClient_contextCancellation demonstrates context cancellation
func ExampleHTTPInferenceClient_contextCancellation() {
	config := batch.HTTPInferenceClientConfig{
		BaseURL: "http://localhost:8000",
	}
	client := batch.NewHTTPInferenceClient(config)

	req := &batch.InferenceRequest{
		RequestID: "cancel-test-001",
		Model:     "gpt-4",
		Params: map[string]interface{}{
			"model": "gpt-4",
			"messages": []map[string]interface{}{
				{
					"role":    "user",
					"content": "This is a test",
				},
			},
		},
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Or cancel based on some condition
	go func() {
		time.Sleep(1 * time.Second)
		cancel() // Cancel the request early
	}()

	resp, err := client.Generate(ctx, req)
	if err != nil {
		if ctx.Err() == context.Canceled {
			fmt.Println("Request was cancelled")
		} else if ctx.Err() == context.DeadlineExceeded {
			fmt.Println("Request timed out")
		} else {
			fmt.Printf("Error: %s\n", err.Message)
		}
		return
	}

	fmt.Printf("Success! Response ID: %s\n", resp.RequestID)
}

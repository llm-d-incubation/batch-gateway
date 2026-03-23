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

package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDo_NoRetry(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxRetries: 0}, func() error {
		calls++
		return errors.New("fail")
	}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDo_SucceedsOnFirstTry(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func() error {
		calls++
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestDo_SucceedsAfterRetries(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDo_ExhaustsRetries(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func() error {
		calls++
		return errors.New("persistent")
	}, nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 3 { // 1 initial + 2 retries
		t.Fatalf("expected 3 calls, got %d", calls)
	}
}

func TestDo_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, Config{MaxRetries: 5, InitialBackoff: time.Second, MaxBackoff: time.Second}, func() error {
		calls++
		cancel()
		return errors.New("fail")
	}, nil)
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancel, got %d", calls)
	}
}

func TestDo_OnRetryCallback(t *testing.T) {
	retryAttempts := []int{}
	err := Do(context.Background(), Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func() error {
		if len(retryAttempts) < 2 {
			return errors.New("fail")
		}
		return nil
	}, func(attempt int, _ error) error {
		retryAttempts = append(retryAttempts, attempt)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(retryAttempts) != 2 {
		t.Fatalf("expected 2 retry callbacks, got %d", len(retryAttempts))
	}
	if retryAttempts[0] != 0 || retryAttempts[1] != 1 {
		t.Fatalf("expected attempts [0,1], got %v", retryAttempts)
	}
}

func TestDo_OnRetryAbort(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Config{MaxRetries: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func() error {
		calls++
		return errors.New("fail")
	}, func(_ int, _ error) error {
		return errors.New("abort: non-recoverable")
	})
	if err == nil || err.Error() != "abort: non-recoverable" {
		t.Fatalf("expected abort error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (no retry after abort), got %d", calls)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"zero retries is valid", Config{MaxRetries: 0}, false},
		{"valid config", Config{MaxRetries: 3, InitialBackoff: time.Second, MaxBackoff: 10 * time.Second}, false},
		{"negative retries", Config{MaxRetries: -1}, true},
		{"missing initial_backoff", Config{MaxRetries: 1, MaxBackoff: time.Second}, true},
		{"missing max_backoff", Config{MaxRetries: 1, InitialBackoff: time.Second}, true},
		{"max_backoff < initial_backoff", Config{MaxRetries: 1, InitialBackoff: 10 * time.Second, MaxBackoff: time.Second}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

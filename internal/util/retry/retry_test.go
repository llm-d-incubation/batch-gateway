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
	n, err := Do(context.Background(), &Config{MaxRetries: 0}, func(attempt int) error {
		calls++
		if attempt != 1 {
			t.Fatalf("expected attempt 1, got %d", attempt)
		}
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if n != 1 {
		t.Fatalf("expected attempts=1, got %d", n)
	}
}

func TestDo_SucceedsOnFirstTry(t *testing.T) {
	calls := 0
	n, err := Do(context.Background(), &Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(attempt int) error {
		calls++
		if attempt != 1 {
			t.Fatalf("expected attempt 1 on first try, got %d", attempt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
	if n != 1 {
		t.Fatalf("expected attempts=1, got %d", n)
	}
}

func TestDo_SucceedsAfterRetries(t *testing.T) {
	calls := 0
	n, err := Do(context.Background(), &Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(attempt int) error {
		calls++
		if attempt != calls {
			t.Fatalf("expected attempt %d, got %d", calls, attempt)
		}
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if n != 3 {
		t.Fatalf("expected attempts=3, got %d", n)
	}
}

func TestDo_ExhaustsRetries(t *testing.T) {
	calls := 0
	n, err := Do(context.Background(), &Config{MaxRetries: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(attempt int) error {
		calls++
		if attempt != calls {
			t.Fatalf("expected attempt %d, got %d", calls, attempt)
		}
		return errors.New("persistent")
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls != 3 { // 1 initial + 2 retries
		t.Fatalf("expected 3 calls, got %d", calls)
	}
	if n != 3 {
		t.Fatalf("expected attempts=3, got %d", n)
	}
}

func TestDo_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := Do(ctx, &Config{MaxRetries: 5, InitialBackoff: time.Second, MaxBackoff: time.Second}, func(attempt int) error {
		calls++
		if attempt != calls {
			t.Fatalf("expected attempt %d, got %d", calls, attempt)
		}
		cancel()
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("expected error on context cancel")
	}
	if calls != 1 {
		t.Fatalf("expected 1 call before cancel, got %d", calls)
	}
}

func TestDo_AttemptNumbers(t *testing.T) {
	attempts := []int{}
	n, err := Do(context.Background(), &Config{MaxRetries: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(attempt int) error {
		attempts = append(attempts, attempt)
		if len(attempts) <= 2 {
			return errors.New("fail")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %d", len(attempts))
	}
	if attempts[0] != 1 || attempts[1] != 2 || attempts[2] != 3 {
		t.Fatalf("expected attempts [1,2,3], got %v", attempts)
	}
	if n != 3 {
		t.Fatalf("expected attempts=3, got %d", n)
	}
}

func TestDo_PermanentError(t *testing.T) {
	calls := 0
	permanentErr := errors.New("non-recoverable error")
	n, err := Do(context.Background(), &Config{MaxRetries: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(attempt int) error {
		calls++
		if attempt > 1 {
			return Permanent(permanentErr)
		}
		return errors.New("initial failure")
	})
	if err != permanentErr {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (initial + first retry with permanent error), got %d", calls)
	}
	if n != 2 {
		t.Fatalf("expected attempts=2, got %d", n)
	}
}

func TestDo_PermanentErrorOnFirstAttempt(t *testing.T) {
	calls := 0
	permanentErr := errors.New("non-recoverable on first try")
	n, err := Do(context.Background(), &Config{MaxRetries: 5, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}, func(attempt int) error {
		calls++
		return Permanent(permanentErr)
	})
	if err != permanentErr {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 call (permanent on first attempt should not retry), got %d", calls)
	}
	if n != 1 {
		t.Fatalf("expected attempts=1, got %d", n)
	}
}

func TestPermanent_NilError(t *testing.T) {
	err := Permanent(nil)
	if err != nil {
		t.Fatalf("Permanent(nil) should return nil, got %v", err)
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

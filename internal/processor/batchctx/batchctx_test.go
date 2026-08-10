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

package batchctx

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestCause(t *testing.T) {
	tests := []struct {
		name string
		// build returns the context whose Cause is under test.
		build func() context.Context
		want  error
	}{
		{
			name:  "not cancelled",
			build: func() context.Context { return context.Background() },
			want:  nil,
		},
		{
			name: "user cancel",
			build: func() context.Context {
				ctx, cause := context.WithCancelCause(context.Background())
				cause(ErrCancelled)
				return ctx
			},
			want: ErrCancelled,
		},
		{
			name: "shutdown",
			build: func() context.Context {
				ctx, cause := context.WithCancelCause(context.Background())
				cause(ErrShutdown)
				return ctx
			},
			want: ErrShutdown,
		},
		{
			name: "expired deadline",
			build: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			want: ErrExpired,
		},
		{
			name: "neutral cancel maps to nil",
			build: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cause(tt.build()); !errors.Is(got, tt.want) {
				t.Fatalf("Cause = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "expired", err: ErrExpired, want: true},
		{name: "cancelled", err: ErrCancelled, want: true},
		{name: "shutdown", err: ErrShutdown, want: true},
		{name: "wrapped terminal", err: fmt.Errorf("finalize: %w", ErrCancelled), want: true},
		{name: "neutral canceled", err: context.Canceled, want: false},
		{name: "system error", err: errors.New("boom"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTerminal(tt.err); got != tt.want {
				t.Fatalf("IsTerminal(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestCause_FirstCauseWins verifies the load-bearing property of the nested-layer
// design: when several sources fire on one abort context, Cause reports whichever
// tripped first, regardless of the layer's position — a later cancellation is a
// no-op on an already-cancelled context.
func TestCause_FirstCauseWins(t *testing.T) {
	tests := []struct {
		name string
		// build stacks a shutdown (outer) and user-cancel (inner) layer, then trips
		// them in a specific order; it returns the innermost work context.
		build func() context.Context
		want  error
	}{
		{
			name: "user cancel before shutdown",
			build: func() context.Context {
				base, tripShutdown := context.WithCancelCause(context.Background())
				ctx, cancelUser := context.WithCancelCause(base)
				cancelUser(ErrCancelled)  // first
				tripShutdown(ErrShutdown) // no-op on the already-cancelled inner layer
				return ctx
			},
			want: ErrCancelled,
		},
		{
			name: "shutdown before user cancel",
			build: func() context.Context {
				base, tripShutdown := context.WithCancelCause(context.Background())
				ctx, cancelUser := context.WithCancelCause(base)
				tripShutdown(ErrShutdown) // first: propagates down to the inner layer
				cancelUser(ErrCancelled)  // no-op on the already-cancelled inner layer
				return ctx
			},
			want: ErrShutdown,
		},
		{
			name: "neutral cause wins over later terminal causes",
			build: func() context.Context {
				ctx, cause := context.WithCancelCause(context.Background())
				cause(context.Canceled) // neutral, first
				cause(ErrCancelled)     // no-op
				return ctx
			},
			want: nil, // neutral fired first -> not a terminal routing cause
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Cause(tt.build()); !errors.Is(got, tt.want) {
				t.Fatalf("Cause = %v, want %v", got, tt.want)
			}
		})
	}
}

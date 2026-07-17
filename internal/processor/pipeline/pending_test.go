package pipeline

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPendingRequests(t *testing.T) {
	t.Run("resolve enriches result with request metadata", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1", ModelID: "m1"})

		result := &ResultItem{RequestID: "r1"}
		if !p.Resolve(result) {
			t.Fatal("Resolve returned false")
		}
		if result.CustomID != "c1" {
			t.Fatalf("CustomID = %q, want c1", result.CustomID)
		}
		if result.ModelID != "m1" {
			t.Fatalf("ModelID = %q, want m1", result.ModelID)
		}
	})

	t.Run("resolve returns false for unknown request", func(t *testing.T) {
		p := NewPendingRequests(0)
		result := &ResultItem{RequestID: "unknown"}
		if p.Resolve(result) {
			t.Fatal("Resolve returned true for unknown request")
		}
	})

	t.Run("resolve with CustomID still decrements pending count", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})

		result := &ResultItem{RequestID: "r1", CustomID: "c1"}
		if !p.Resolve(result) {
			t.Fatal("Resolve returned false for result with CustomID")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		p.Wait(ctx)
		if ctx.Err() != nil {
			t.Fatal("Wait blocked after resolving request with pre-set CustomID")
		}
	})

	t.Run("wait returns immediately when no pending", func(t *testing.T) {
		p := NewPendingRequests(0)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Wait(ctx)
		if ctx.Err() != nil {
			t.Fatal("Wait blocked on empty pending set")
		}
	})

	t.Run("wait unblocks when last request resolves", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})
		p.Store(RequestItem{RequestID: "r2", CustomID: "c2"})

		done := make(chan struct{})
		go func() {
			p.Wait(context.Background())
			close(done)
		}()

		p.Resolve(&ResultItem{RequestID: "r1"})
		select {
		case <-done:
			t.Fatal("Wait returned before all resolved")
		case <-time.After(50 * time.Millisecond):
		}

		p.Resolve(&ResultItem{RequestID: "r2"})
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after all resolved")
		}
	})

	t.Run("wait respects context cancellation", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			p.Wait(ctx)
			close(done)
		}()

		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after context cancelled")
		}
	})

	t.Run("wait unblocks under concurrent resolves", func(t *testing.T) {
		p := NewPendingRequests(0)
		const n = 100
		for i := range n {
			id := fmt.Sprintf("r%d", i)
			p.Store(RequestItem{RequestID: id, CustomID: "c"})
		}

		done := make(chan struct{})
		go func() {
			p.Wait(context.Background())
			close(done)
		}()

		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("r%d", i)
				p.Resolve(&ResultItem{RequestID: id})
			}()
		}
		wg.Wait()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after all concurrent resolves")
		}
	})

	t.Run("wait returns when resolve completes before wait starts", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})
		p.Resolve(&ResultItem{RequestID: "r1"})

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Wait(ctx)
		if ctx.Err() != nil {
			t.Fatal("Wait blocked when all items already resolved")
		}
	})

	t.Run("early resolve does not cause Wait to return prematurely", func(t *testing.T) {
		// Simulates non-monotonic interleaving: result for r1 arrives
		// before r2 is stored. Count temporarily hits 0 but Wait must
		// not return until r2 is also resolved.
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})

		// Resolve r1 — count goes to 0 temporarily
		p.Resolve(&ResultItem{RequestID: "r1"})

		// Store r2 — count goes back to 1
		p.Store(RequestItem{RequestID: "r2", CustomID: "c2"})

		done := make(chan struct{})
		go func() {
			p.Wait(context.Background())
			close(done)
		}()

		// Wait should NOT have returned yet
		select {
		case <-done:
			t.Fatal("Wait returned before r2 was resolved (non-monotonic bug)")
		case <-time.After(50 * time.Millisecond):
		}

		// Resolve r2 — now Wait should return
		p.Resolve(&ResultItem{RequestID: "r2"})
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after all resolved")
		}
	})

	t.Run("Error propagates SubmittedAt", func(t *testing.T) {
		submitted := time.Now()
		req := RequestItem{
			RequestID:   "r1",
			CustomID:    "c1",
			ModelID:     "m1",
			SubmittedAt: submitted,
		}
		result := req.Error("test_error", "some error")
		if result.SubmittedAt != submitted {
			t.Fatalf("SubmittedAt not propagated: got %v, want %v", result.SubmittedAt, submitted)
		}
	})

	t.Run("resolve rejects broadcaster error for other job", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})

		// Simulate a broadcaster error result for a request NOT in this job's map
		otherJobResult := &ResultItem{
			RequestID: "other-job-req",
			Error:     &OutputError{Code: "server_error", Message: "bad body"},
		}
		if p.Resolve(otherJobResult) {
			t.Fatal("Resolve accepted error result from another job")
		}
	})

	t.Run("resolve accepts inline error with CustomID", func(t *testing.T) {
		p := NewPendingRequests(0)
		// Inline errors from the dispatcher have CustomID set
		inlineErr := &ResultItem{
			RequestID: "r1",
			CustomID:  "c1",
			Error:     &OutputError{Code: "model_not_found", Message: "not found"},
		}
		if !p.Resolve(inlineErr) {
			t.Fatal("Resolve rejected inline error with CustomID")
		}
	})

	t.Run("drain unresolved unblocks Wait and returns remaining", func(t *testing.T) {
		p := NewPendingRequests(0)
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})
		p.Store(RequestItem{RequestID: "r2", CustomID: "c2"})
		p.Store(RequestItem{RequestID: "r3", CustomID: "c3"})

		// Resolve only r1
		p.Resolve(&ResultItem{RequestID: "r1"})

		done := make(chan struct{})
		go func() {
			p.Wait(context.Background())
			close(done)
		}()

		// Drain the remaining — should also unblock Wait
		var drained []string
		p.DrainUnresolved(func(msg RequestItem) {
			drained = append(drained, msg.RequestID)
		})

		if len(drained) != 2 {
			t.Fatalf("drained %d, want 2", len(drained))
		}

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Wait did not return after DrainUnresolved")
		}
	})
}

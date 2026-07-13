package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPendingRequests(t *testing.T) {
	t.Run("resolve enriches result with request metadata", func(t *testing.T) {
		p := NewPendingRequests()
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
		p := NewPendingRequests()
		result := &ResultItem{RequestID: "unknown"}
		if p.Resolve(result) {
			t.Fatal("Resolve returned true for unknown request")
		}
	})

	t.Run("resolve skips lookup when CustomID already set", func(t *testing.T) {
		p := NewPendingRequests()
		result := &ResultItem{RequestID: "r1", CustomID: "already-set"}
		if !p.Resolve(result) {
			t.Fatal("Resolve returned false for result with CustomID")
		}
	})

	t.Run("wait returns immediately when no pending", func(t *testing.T) {
		p := NewPendingRequests()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Wait(ctx)
		if ctx.Err() != nil {
			t.Fatal("Wait blocked on empty pending set")
		}
	})

	t.Run("wait unblocks when last request resolves", func(t *testing.T) {
		p := NewPendingRequests()
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
		p := NewPendingRequests()
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
		p := NewPendingRequests()
		const n = 100
		for i := range n {
			id := "r" + string(rune('A'+i%26)) + string(rune('0'+i/26))
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
				id := "r" + string(rune('A'+i%26)) + string(rune('0'+i/26))
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
		p := NewPendingRequests()
		p.Store(RequestItem{RequestID: "r1", CustomID: "c1"})
		p.Resolve(&ResultItem{RequestID: "r1"})

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		p.Wait(ctx)
		if ctx.Err() != nil {
			t.Fatal("Wait blocked when all items already resolved")
		}
	})
}

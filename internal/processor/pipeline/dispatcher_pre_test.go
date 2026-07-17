package pipeline

import (
	"context"
	"testing"
	"time"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
)

type recordingDispatcher struct {
	received []RequestItem
}

var _ RequestDispatcher = (*recordingDispatcher)(nil)

func (d *recordingDispatcher) Run(_ context.Context, requestCh <-chan RequestItem, resultCh chan<- ResultItem) error {
	for msg := range requestCh {
		d.received = append(d.received, msg)
		resultCh <- ResultItem{
			RequestID: msg.RequestID,
			CustomID:  msg.CustomID,
			ModelID:   msg.ModelID,
			Response:  &batch_types.ResponseData{StatusCode: 200, RequestID: msg.RequestID},
		}
	}
	close(resultCh)
	return nil
}

func TestPreDispatcher(t *testing.T) {
	t.Run("filters parse errors", func(t *testing.T) {
		inner := &recordingDispatcher{}
		pre := NewPreDispatcher(inner)

		requestCh := make(chan RequestItem, 3)
		resultCh := make(chan ResultItem, 3)

		requestCh <- RequestItem{RequestID: "ok-1", CustomID: "c-1", ModelID: "m1"}
		requestCh <- RequestItem{RequestID: "bad", ParseError: &OutputError{Code: "parse_error", Message: "bad json"}}
		requestCh <- RequestItem{RequestID: "ok-2", CustomID: "c-2", ModelID: "m1"}
		close(requestCh)

		if err := pre.Run(context.Background(), requestCh, resultCh); err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		// Inner should only receive the two valid requests.
		if len(inner.received) != 2 {
			t.Fatalf("inner received %d requests, want 2", len(inner.received))
		}
		if inner.received[0].RequestID != "ok-1" || inner.received[1].RequestID != "ok-2" {
			t.Errorf("unexpected request IDs: %v", inner.received)
		}

		// Collect all results.
		var results []ResultItem
		for r := range resultCh {
			results = append(results, r)
		}
		if len(results) != 3 {
			t.Fatalf("got %d results, want 3", len(results))
		}

		// The parse-error result should have an error.
		var parseErrResult *ResultItem
		for i := range results {
			if results[i].RequestID == "bad" {
				parseErrResult = &results[i]
				break
			}
		}
		if parseErrResult == nil {
			t.Fatal("no result for parse-error request")
		}
		if parseErrResult.Error == nil || parseErrResult.Error.Code != "parse_error" {
			t.Errorf("expected parse_error, got %+v", parseErrResult.Error)
		}
	})

	t.Run("sets SubmittedAt", func(t *testing.T) {
		inner := &recordingDispatcher{}
		pre := NewPreDispatcher(inner)

		requestCh := make(chan RequestItem, 1)
		resultCh := make(chan ResultItem, 1)

		requestCh <- RequestItem{RequestID: "r1", ModelID: "m1"}
		close(requestCh)

		before := time.Now()
		if err := pre.Run(context.Background(), requestCh, resultCh); err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		if len(inner.received) != 1 {
			t.Fatalf("inner received %d, want 1", len(inner.received))
		}
		if inner.received[0].SubmittedAt.Before(before) {
			t.Error("SubmittedAt not set or set too early")
		}
	})

	t.Run("cancellation drains remaining", func(t *testing.T) {
		inner := &recordingDispatcher{}
		pre := NewPreDispatcher(inner)

		requestCh := make(chan RequestItem, 3)
		resultCh := make(chan ResultItem, 6)

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancel immediately

		requestCh <- RequestItem{RequestID: "r1", ModelID: "m1"}
		requestCh <- RequestItem{RequestID: "r2", ModelID: "m1"}
		requestCh <- RequestItem{RequestID: "r3", ModelID: "m1"}
		close(requestCh)

		if err := pre.Run(ctx, requestCh, resultCh); err != nil {
			t.Fatalf("Run() error: %v", err)
		}

		// Inner should receive nothing — all cancelled.
		if len(inner.received) != 0 {
			t.Errorf("inner received %d requests, want 0", len(inner.received))
		}

		// All results should be cancellation errors.
		var results []ResultItem
		for r := range resultCh {
			results = append(results, r)
		}
		if len(results) != 3 {
			t.Fatalf("got %d results, want 3", len(results))
		}
		for _, r := range results {
			if r.Error == nil || r.Error.Code != "batch_cancelled" {
				t.Errorf("expected batch_cancelled for %s, got %+v", r.RequestID, r.Error)
			}
		}
	})
}

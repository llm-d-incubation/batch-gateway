package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/go-logr/logr"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
)

func TestResultCollector_RoutesToCorrectFile(t *testing.T) {
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(3, nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	results := []ResultItem{
		{
			RequestID: "req-1",
			CustomID:  "c-1",
			Response:  &batch_types.ResponseData{StatusCode: 200, RequestID: "req-1", Body: map[string]any{"ok": true}},
		},
		{
			RequestID: "req-2",
			CustomID:  "c-2",
			Response:  &batch_types.ResponseData{StatusCode: 422, RequestID: "req-2", Body: map[string]any{"error": "bad"}},
		},
		{
			RequestID: "req-3",
			CustomID:  "c-3",
			Error:     &OutputError{Code: "SERVER_ERROR", Message: "connection refused"},
		},
	}

	for _, r := range results {
		pending.Store(RequestItem{RequestID: r.RequestID, CustomID: r.CustomID})
	}

	ch := make(chan ResultItem, len(results))
	for _, r := range results {
		ch <- r
	}
	close(ch)

	if err := collector.Drain(context.Background(), ch); err != nil {
		t.Fatalf("Drain error: %v", err)
	}

	outputData := readFile(t, outputFile)
	outputLines := splitLines(outputData)
	if len(outputLines) != 2 {
		t.Fatalf("output lines = %d, want 2 (200 success + 422 HTTP error)", len(outputLines))
	}

	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 1 {
		t.Fatalf("error lines = %d, want 1 (non-HTTP error)", len(errorLines))
	}

	var errEntry outputLine
	if err := json.Unmarshal(errorLines[0], &errEntry); err != nil {
		t.Fatalf("unmarshal error line: %v", err)
	}
	if errEntry.Error == nil || errEntry.Error.Code != "SERVER_ERROR" {
		t.Fatalf("expected SERVER_ERROR, got %+v", errEntry.Error)
	}
}

func TestResultCollector_DrainSkipsUnknownPending(t *testing.T) {
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(1, nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	ch := make(chan ResultItem, 1)
	ch <- ResultItem{
		RequestID: "unknown-req",
	}
	close(ch)

	if err := collector.Drain(context.Background(), ch); err != nil {
		t.Fatalf("Drain error: %v", err)
	}

	outputData := readFile(t, outputFile)
	errorData := readFile(t, errorFile)
	if len(trimBytes(outputData)) != 0 || len(trimBytes(errorData)) != 0 {
		t.Fatal("expected no output for unknown pending request")
	}
}

func TestResultCollector_DrainProcessesAllResultsAfterCancel(t *testing.T) {
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(3, nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	results := []ResultItem{
		{RequestID: "req-1", CustomID: "c-1", Response: &batch_types.ResponseData{StatusCode: 200, RequestID: "req-1", Body: map[string]any{"ok": true}}},
		{RequestID: "req-2", CustomID: "c-2", Error: &OutputError{Code: "batch_cancelled", Message: "cancelled"}},
		{RequestID: "req-3", CustomID: "c-3", Error: &OutputError{Code: "batch_cancelled", Message: "cancelled"}},
	}
	for _, r := range results {
		pending.Store(RequestItem{RequestID: r.RequestID, CustomID: r.CustomID})
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan ResultItem, len(results))

	// Send first result, then cancel, then send the rest.
	ch <- results[0]
	cancel()
	ch <- results[1]
	ch <- results[2]
	close(ch)

	err := collector.Drain(ctx, ch)
	if err != context.Canceled {
		t.Fatalf("Drain error = %v, want context.Canceled", err)
	}

	outputData := readFile(t, outputFile)
	outputLines := splitLines(outputData)
	if len(outputLines) != 1 {
		t.Fatalf("output lines = %d, want 1", len(outputLines))
	}

	errorData := readFile(t, errorFile)
	errorLines := splitLines(errorData)
	if len(errorLines) != 2 {
		t.Fatalf("error lines = %d, want 2 (cancelled requests must still be written)", len(errorLines))
	}
}

type failAfterNWriter struct {
	remaining int
}

func (w *failAfterNWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, fmt.Errorf("simulated write failure")
	}
	w.remaining--
	return len(p), nil
}

func TestResultCollector_DrainDecrementsMetricsAfterWriteFailure(t *testing.T) {
	outputFile := tempFile(t)
	errorFile := tempFile(t)
	pending := NewPendingRequests(0)
	tracker := NewProgressTracker(3, nil, "test-job", 0, logr.Discard())
	collector := NewResultCollector(outputFile, errorFile, pending, tracker, logr.Discard())

	collector.output = bufio.NewWriterSize(&failAfterNWriter{remaining: 1}, 1)

	now := time.Now()
	results := []ResultItem{
		{RequestID: "req-1", CustomID: "c-1", SubmittedAt: now, Response: &batch_types.ResponseData{StatusCode: 200, RequestID: "req-1", Body: map[string]any{"ok": true}}},
		{RequestID: "req-2", CustomID: "c-2", SubmittedAt: now, Response: &batch_types.ResponseData{StatusCode: 200, RequestID: "req-2", Body: map[string]any{"ok": true}}},
		{RequestID: "req-3", CustomID: "c-3", SubmittedAt: now, Response: &batch_types.ResponseData{StatusCode: 200, RequestID: "req-3", Body: map[string]any{"ok": true}}},
	}
	for _, r := range results {
		pending.Store(RequestItem{RequestID: r.RequestID, CustomID: r.CustomID, SubmittedAt: r.SubmittedAt})
	}

	ch := make(chan ResultItem, len(results))
	for _, r := range results {
		ch <- r
	}
	close(ch)

	err := collector.Drain(context.Background(), ch)
	if err == nil {
		t.Fatal("expected write failure error from Drain")
	}

	var remaining int
	pending.DrainUnresolved(func(_ RequestItem) {
		remaining++
	})
	if remaining != 0 {
		t.Fatalf("pending requests remaining = %d, want 0 (all should be resolved despite write failure)", remaining)
	}
}

func TestProgressTracker_AddFailed(t *testing.T) {
	tracker := NewProgressTracker(10, nil, "test-job", 0, logr.Discard())
	tracker.AddFailed(5)
	counts := tracker.Counts()
	if counts.Failed != 5 {
		t.Fatalf("Failed = %d, want 5", counts.Failed)
	}
	if counts.Total != 10 {
		t.Fatalf("Total = %d, want 10", counts.Total)
	}
}

func TestProgressTracker_RecordSuccessAndFailure(t *testing.T) {
	tracker := NewProgressTracker(3, nil, "test-job", 0, logr.Discard())
	tracker.RecordSuccess(ResultItem{RequestID: "r1"})
	tracker.RecordSuccess(ResultItem{RequestID: "r2"})
	tracker.RecordFailure(nil)

	counts := tracker.Counts()
	if counts.Completed != 2 {
		t.Fatalf("Completed = %d, want 2", counts.Completed)
	}
	if counts.Failed != 1 {
		t.Fatalf("Failed = %d, want 1", counts.Failed)
	}
}

package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/go-logr/logr"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
)

// outputLine is the JSONL format for one result, matching the OpenAI batch output schema.
type outputLine struct {
	ID       string                    `json:"id"`
	CustomID string                    `json:"custom_id"`
	Response *batch_types.ResponseData `json:"response"`
	Error    *OutputError              `json:"error"`
}

const fileBufferSize = 1024 * 1024

func (o *outputLine) isSuccess() bool {
	return o.Error == nil && o.Response != nil && o.Response.StatusCode == 200
}

// ResultCollector writes ResultItem values to JSONL and records progress.
// Terminal actor — no out channel.
type ResultCollector struct {
	output  *bufio.Writer
	errors  *bufio.Writer
	pending *PendingRequests
	tracker *ProgressTracker
	logger  logr.Logger
}

func NewResultCollector(outputFile, errorFile *os.File, pending *PendingRequests, tracker *ProgressTracker, logger logr.Logger) *ResultCollector {
	output := bufio.NewWriterSize(outputFile, fileBufferSize)
	errors := bufio.NewWriterSize(errorFile, fileBufferSize)

	return &ResultCollector{output: output, errors: errors, pending: pending, tracker: tracker, logger: logger}
}

// Drain reads results until resultCh is closed, then flushes.
// Both dispatchers close the channel when done, so this always terminates.
// Returns ctx.Err() if the context was cancelled during draining.
func (c *ResultCollector) Drain(ctx context.Context, resultCh <-chan ResultItem) error {
	var firstErr error
	for msg := range resultCh {
		if !c.pending.Resolve(&msg) {
			continue
		}
		if err := c.Receive(msg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if flushErr := c.flushFiles(); flushErr != nil {
		return flushErr
	}
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (c *ResultCollector) Receive(msg ResultItem) error {
	line := &outputLine{
		ID:       msg.RequestID,
		CustomID: msg.CustomID,
		Response: msg.Response,
		Error:    msg.Error,
	}

	lineBytes, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("marshal output for %s: %w", msg.RequestID, err)
	}
	lineBytes = append(lineBytes, '\n')

	w := c.output
	if line.Error != nil {
		w = c.errors
	}
	if _, err := w.Write(lineBytes); err != nil {
		return fmt.Errorf("write output for %s: %w", msg.RequestID, err)
	}

	if line.isSuccess() {
		c.tracker.RecordSuccess(msg)
	} else {
		code := "unknown"
		if line.Error != nil {
			code = line.Error.Code
		} else if line.Response != nil {
			code = fmt.Sprintf("http_%d", line.Response.StatusCode)
		}
		c.tracker.RecordFailure(fmt.Errorf("%s: %s", msg.RequestID, code))
	}

	return nil
}

func (c *ResultCollector) flushFiles() (err error) {
	if err = c.output.Flush(); err != nil {
		c.logger.Error(err, "Failed to flush output file")
	}
	if errErr := c.errors.Flush(); errErr != nil {
		c.logger.Error(errErr, "Failed to flush error file")
		if err == nil {
			err = errErr
		}
	}
	return
}

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

package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"sync"

	"github.com/go-logr/logr"

	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
)

// ResultItem is the outcome of a single inference request.
// Produced by processModel goroutines, consumed by resultCollector.
type ResultItem struct {
	RequestID        string
	CustomID         string
	Response         *batch_types.ResponseData
	Error            *OutputError
	HadCapacityRetry bool
	ModelID          string
}

// OutputError is the error structure written to JSONL output/error files.
type OutputError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *ResultItem) isSuccess() bool {
	return r.Error == nil && r.Response != nil && r.Response.StatusCode == 200
}

func resultToOutputLine(r *ResultItem) *outputLine {
	var outErr *outputError
	if r.Error != nil {
		outErr = &outputError{Code: r.Error.Code, Message: r.Error.Message}
	}
	return &outputLine{
		ID:       r.RequestID,
		CustomID: r.CustomID,
		Response: r.Response,
		Error:    outErr,
	}
}

// resultCollector reads ResultItems from an internal channel, marshals each to
// JSONL, writes to the appropriate file (output or error), and records progress.
// It runs as a single goroutine.
//
// On write/marshal errors the collector calls abortFn to stop dispatch, logs
// the error, and continues draining so senders don't deadlock. The channel
// buffer (1024) absorbs bursts, but senders will block if it fills.
//
// Usage:
//
//	c := newResultCollector(outputBuf, errorBuf, progress, logger, abortFn)
//	c.start()
//	// ... call c.collect(result) from any goroutine ...
//	c.flush() // closes channel, waits for goroutine to finish
type resultCollector struct {
	outputWriter *bufio.Writer
	errorWriter  *bufio.Writer
	progress     *executionProgress
	logger       logr.Logger
	abortFn      context.CancelFunc
	abortOnce    sync.Once

	ch       chan *ResultItem
	done     chan error
	writeErr error
}

func newResultCollector(outputWriter, errorWriter *bufio.Writer, progress *executionProgress, logger logr.Logger, abortFn context.CancelFunc) *resultCollector {
	if abortFn == nil {
		panic("resultCollector: abortFn cannot be nil")
	}
	return &resultCollector{
		outputWriter: outputWriter,
		errorWriter:  errorWriter,
		progress:     progress,
		logger:       logger,
		abortFn:      abortFn,
		ch:           make(chan *ResultItem, 1024),
		done:         make(chan error, 1),
	}
}

// start launches the collector goroutine. The context is used for progress
// updates to the status store.
func (c *resultCollector) start(ctx context.Context) {
	go func() {
		c.done <- c.run(ctx)
	}()
}

// collect sends a result to the collector goroutine.
func (c *resultCollector) collect(result *ResultItem) {
	c.ch <- result
}

// flush closes the result channel, waits for the collector goroutine to
// finish writing all buffered results, and returns the first write error
// encountered (or nil).
func (c *resultCollector) flush() error {
	close(c.ch)
	return <-c.done
}

func (c *resultCollector) abort(err error) {
	if c.writeErr == nil {
		c.writeErr = err
	}
	c.abortOnce.Do(c.abortFn)
}

func (c *resultCollector) run(ctx context.Context) error {
	for result := range c.ch {
		line := resultToOutputLine(result)

		lineBytes, err := json.Marshal(line)
		if err != nil {
			c.logger.Error(err, "Failed to marshal output line", "customId", result.CustomID)
			c.abort(err)
			continue
		}
		lineBytes = append(lineBytes, '\n')

		isError := line.Error != nil
		writer := c.outputWriter
		if isError {
			writer = c.errorWriter
		}
		if _, err := writer.Write(lineBytes); err != nil {
			c.logger.Error(err, "Failed to write output line", "customId", result.CustomID)
			c.abort(err)
			continue
		}

		c.progress.record(ctx, result.isSuccess())
	}

	if err := c.outputWriter.Flush(); err != nil {
		c.logger.Error(err, "Failed to flush output file (partial results may be truncated)")
		c.abort(err)
	}
	if err := c.errorWriter.Flush(); err != nil {
		c.logger.Error(err, "Failed to flush error file (partial results may be truncated)")
		c.abort(err)
	}
	return c.writeErr
}

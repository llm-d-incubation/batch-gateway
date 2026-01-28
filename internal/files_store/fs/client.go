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

// Package fs provides a filesystem-based implementation of the BatchFilesClient interface.
package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
)

// DefaultTimeout is the default timeout for filesystem operations.
const DefaultTimeout = 30 * time.Second

// ErrFileTooLarge is returned when the file size exceeds the limit.
var ErrFileTooLarge = errors.New("file size exceeds limit")

// ErrFileExists is returned when attempting to store a file that already exists.
var ErrFileExists = errors.New("file already exists")

// Client implements api.BatchFilesClient using local filesystem storage.
type Client struct {
	basePath       string
	defaultTimeout time.Duration
}

// Compile-time check that Client implements api.BatchFilesClient.
var _ api.BatchFilesClient = (*Client)(nil)

// New creates a new filesystem-based BatchFilesClient.
func New(basePath string) (*Client, error) {
	absPath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	if err := os.MkdirAll(absPath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create base directory: %w", err)
	}

	return &Client{
		basePath:       filepath.Clean(absPath),
		defaultTimeout: DefaultTimeout,
	}, nil
}

// SetDefaultTimeout sets the default timeout for operations.
func (c *Client) SetDefaultTimeout(timeout time.Duration) {
	c.defaultTimeout = timeout
}

// resolvePath sanitizes and resolves a location to a full path, preventing path traversal.
func (c *Client) resolvePath(location string) (string, error) {
	fullPath := filepath.Join(c.basePath, filepath.Clean(location))
	if !strings.HasPrefix(fullPath, c.basePath+string(os.PathSeparator)) &&
		fullPath != filepath.Clean(c.basePath) {
		return "", fmt.Errorf("invalid path: %w", os.ErrInvalid)
	}
	return fullPath, nil
}

// Store stores a file in the filesystem.
func (c *Client) Store(ctx context.Context, location string, fileSizeLimit int64, reader io.Reader) (
	*api.BatchFileMetadata, error,
) {
	fullPath, err := c.resolvePath(location)
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(fullPath); err == nil {
		return nil, ErrFileExists
	}

	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		// Clean up on error.
		_ = os.Remove(tmpPath)
	}()

	limitedReader := io.LimitReader(reader, fileSizeLimit+1)

	written, err := io.Copy(tmpFile, limitedReader)
	if err != nil {
		_ = tmpFile.Close()
		return nil, fmt.Errorf("failed to write file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("failed to close temp file: %w", err)
	}

	if written > fileSizeLimit {
		return nil, ErrFileTooLarge
	}

	// Rename temp file to final destination.
	if err := os.Rename(tmpPath, fullPath); err != nil {
		return nil, fmt.Errorf("failed to rename file: %w", err)
	}

	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	return &api.BatchFileMetadata{
		Location: fullPath,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
	}, nil
}

// Retrieve retrieves a file from the filesystem.
func (c *Client) Retrieve(ctx context.Context, location string) (io.Reader, *api.BatchFileMetadata, error) {
	fullPath, err := c.resolvePath(location)
	if err != nil {
		return nil, nil, err
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return nil, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("failed to stat file: %w", err)
	}

	return file, &api.BatchFileMetadata{
		Location: fullPath,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
	}, nil
}

// List lists files matching the pattern.
func (c *Client) List(ctx context.Context, location string) ([]api.BatchFileMetadata, error) {
	fullPattern, err := c.resolvePath(location)
	if err != nil {
		return nil, err
	}

	matches, err := filepath.Glob(fullPattern)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern: %w", err)
	}

	var files []api.BatchFileMetadata
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue // Skip files that can't be stat'd.
		}
		if info.IsDir() {
			continue // Skip directories.
		}
		files = append(files, api.BatchFileMetadata{
			Location: match,
			Size:     info.Size(),
			ModTime:  info.ModTime(),
		})
	}

	return files, nil
}

// Delete deletes a file from the filesystem.
func (c *Client) Delete(ctx context.Context, location string) error {
	fullPath, err := c.resolvePath(location)
	if err != nil {
		return err
	}

	return os.Remove(fullPath)
}

// GetContext returns a derived context with a timeout.
func (c *Client) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	if timeLimit == 0 {
		timeLimit = c.defaultTimeout
	}
	return context.WithTimeout(parentCtx, timeLimit)
}

// Close closes the client.
func (c *Client) Close() error {
	return nil
}

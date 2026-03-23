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

// Package retryclient wraps a BatchFilesClient with retry logic.
package retryclient

import (
	"context"
	"io"

	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"github.com/llm-d-incubation/batch-gateway/internal/util/retry"
)

// Client wraps a BatchFilesClient and retries transient errors.
type Client struct {
	inner api.BatchFilesClient
	cfg   retry.Config
}

var _ api.BatchFilesClient = (*Client)(nil)

// New creates a retry-wrapping Client. If cfg.MaxRetries is 0, operations
// are forwarded to inner without any retry overhead.
func New(inner api.BatchFilesClient, cfg retry.Config) *Client {
	return &Client{inner: inner, cfg: cfg}
}

func (c *Client) Store(ctx context.Context, fileName, folderName string, fileSizeLimit, lineNumLimit int64, reader io.Reader) (
	*api.BatchFileMetadata, error,
) {
	rs, seekable := reader.(io.ReadSeeker)

	var meta *api.BatchFileMetadata
	err := retry.Do(ctx, c.cfg, func() error {
		var storeErr error
		meta, storeErr = c.inner.Store(ctx, fileName, folderName, fileSizeLimit, lineNumLimit, reader)
		return storeErr
	}, func(attempt int, retryErr error) {
		klog.FromContext(ctx).V(logging.WARNING).Info("Retrying file store",
			"file", fileName, "attempt", attempt+1, "error", retryErr)
		if seekable {
			if _, seekErr := rs.Seek(0, io.SeekStart); seekErr != nil {
				klog.FromContext(ctx).Error(seekErr, "failed to seek reader for retry")
			}
		}
	})
	return meta, err
}

func (c *Client) Retrieve(ctx context.Context, fileName, folderName string) (io.ReadCloser, *api.BatchFileMetadata, error) {
	var (
		rc   io.ReadCloser
		meta *api.BatchFileMetadata
	)
	err := retry.Do(ctx, c.cfg, func() error {
		var retrieveErr error
		rc, meta, retrieveErr = c.inner.Retrieve(ctx, fileName, folderName)
		return retrieveErr
	}, func(attempt int, retryErr error) {
		klog.FromContext(ctx).V(logging.WARNING).Info("Retrying file retrieve",
			"file", fileName, "attempt", attempt+1, "error", retryErr)
	})
	return rc, meta, err
}

func (c *Client) Delete(ctx context.Context, fileName, folderName string) error {
	return retry.Do(ctx, c.cfg, func() error {
		return c.inner.Delete(ctx, fileName, folderName)
	}, func(attempt int, retryErr error) {
		klog.FromContext(ctx).V(logging.WARNING).Info("Retrying file delete",
			"file", fileName, "attempt", attempt+1, "error", retryErr)
	})
}

func (c *Client) Close() error {
	return c.inner.Close()
}

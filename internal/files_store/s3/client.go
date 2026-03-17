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

// Package s3 provides an S3-based implementation of the BatchFilesClient interface.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
	fsio "github.com/llm-d-incubation/batch-gateway/internal/files_store/io"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
)

const (
	defaultTimeout = 30 * time.Second
)

type s3API interface {
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type uploaderAPI interface {
	Upload(ctx context.Context, input *s3.PutObjectInput, opts ...func(*manager.Uploader)) (*manager.UploadOutput, error) //nolint:staticcheck // TODO: migrate to feature/s3/transfermanager
}

// Client implements api.BatchFilesClient using S3 storage.
// The folderName parameter in Store/Retrieve/Delete is used as the S3 bucket name.
type Client struct {
	s3Client s3API
	uploader uploaderAPI
	prefix   string
}

var _ api.BatchFilesClient = (*Client)(nil)

// Config holds configuration for the S3 client.
type Config struct {
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"-"`
	Prefix          string `yaml:"prefix"`
	UsePathStyle    bool   `yaml:"use_path_style"`
}

// Validate checks that all required fields are set.
func (c *Config) Validate() error {
	if c.Region == "" {
		return fmt.Errorf("s3.region cannot be empty")
	}
	return nil
}

// New creates a new S3-based BatchFilesClient.
func New(ctx context.Context, cfg Config) (*Client, error) {
	var opts []func(*config.LoadOptions) error
	opts = append(opts, config.WithRegion(cfg.Region))

	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	if cfg.UsePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	s3Client := s3.NewFromConfig(awsCfg, s3Opts...)

	klog.InfoS("BatchFiles S3 client initialized", "endpoint", cfg.Endpoint, "region", cfg.Region, "prefix", cfg.Prefix)

	return &Client{
		s3Client: s3Client,
		uploader: manager.NewUploader(s3Client), //nolint:staticcheck // TODO: migrate to feature/s3/transfermanager
		prefix:   cfg.Prefix,
	}, nil
}

// buildKey constructs the full S3 key from the file name.
func (c *Client) buildKey(fileName string) string {
	if c.prefix == "" {
		return fileName
	}
	return c.prefix + "/" + fileName
}

// Store stores a file in S3.
// The bucket parameter specifies which S3 bucket to use.
func (c *Client) Store(ctx context.Context, fileName, bucket string, fileSizeLimit, lineNumLimit int64, reader io.Reader) (
	*api.BatchFileMetadata, error,
) {
	key := c.buildKey(fileName)

	_, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil, fmt.Errorf("%w: %s", api.ErrFileExists, key)
	}

	var notFound *types.NotFound
	if !errors.As(err, &notFound) {
		return nil, err
	}

	countingReader := &fsio.LimitedCountingReader{
		Reader:    reader,
		SizeLimit: fileSizeLimit,
		LineLimit: lineNumLimit,
	}

	_, err = c.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   countingReader,
	})
	if err != nil {
		switch {
		case errors.Is(err, api.ErrFileTooLarge):
			return nil, api.ErrFileTooLarge
		case errors.Is(err, api.ErrTooManyLines):
			return nil, api.ErrTooManyLines
		default:
			return nil, err
		}
	}

	metadata := &api.BatchFileMetadata{
		Location:    key,
		Size:        countingReader.BytesRead,
		LinesNumber: countingReader.LineCount,
		ModTime:     time.Now(),
	}

	klog.FromContext(ctx).V(logging.INFO).Info("File stored successfully",
		"bucket", bucket, "key", key, "size", metadata.Size, "lines", metadata.LinesNumber)

	return metadata, nil
}

// Retrieve retrieves a file from S3.
// The bucket parameter specifies which S3 bucket to use.
func (c *Client) Retrieve(ctx context.Context, fileName, bucket string) (io.ReadCloser, *api.BatchFileMetadata, error) {
	key := c.buildKey(fileName)

	out, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, nil, os.ErrNotExist
		}
		return nil, nil, err
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}

	modTime := time.Now()
	if out.LastModified != nil {
		modTime = *out.LastModified
	}

	metadata := &api.BatchFileMetadata{
		Location: key,
		Size:     size,
		ModTime:  modTime,
	}

	klog.FromContext(ctx).V(logging.INFO).Info("File retrieved successfully",
		"bucket", bucket, "key", key, "size", metadata.Size)

	return out.Body, metadata, nil
}

// Delete deletes a file from S3.
// The bucket parameter specifies which S3 bucket to use.
func (c *Client) Delete(ctx context.Context, fileName, bucket string) error {
	key := c.buildKey(fileName)

	_, err := c.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return err
	}

	klog.FromContext(ctx).V(logging.INFO).Info("File deleted successfully",
		"bucket", bucket, "key", key)

	return nil
}

// GetContext returns a derived context with a timeout.
func (c *Client) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	if timeLimit == 0 {
		timeLimit = defaultTimeout
	}
	return context.WithTimeout(parentCtx, timeLimit)
}

// Close closes the client.
func (c *Client) Close() error {
	return nil
}

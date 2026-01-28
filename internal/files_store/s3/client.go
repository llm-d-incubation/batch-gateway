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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/llm-d-incubation/batch-gateway/internal/files_store/api"
)

const DefaultTimeout = 30 * time.Second

var ErrFileTooLarge = errors.New("file size exceeds limit")
var ErrFileExists = errors.New("file already exists")

type s3API interface {
	PutObject(ctx context.Context, params *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, params *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, params *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, params *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

type Client struct {
	s3Client       s3API
	bucket         string
	prefix         string
	defaultTimeout time.Duration
}

var _ api.BatchFilesClient = (*Client)(nil)

type Config struct {
	Bucket          string
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Prefix          string
	UsePathStyle    bool
}

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

	return &Client{
		s3Client:       s3.NewFromConfig(awsCfg, s3Opts...),
		bucket:         cfg.Bucket,
		prefix:         cfg.Prefix,
		defaultTimeout: DefaultTimeout,
	}, nil
}

func (c *Client) SetDefaultTimeout(timeout time.Duration) {
	c.defaultTimeout = timeout
}

func (c *Client) resolveKey(location string) string {
	if c.prefix == "" {
		return location
	}
	return c.prefix + "/" + location
}

func (c *Client) Store(ctx context.Context, location string, fileSizeLimit int64, reader io.Reader) (
	*api.BatchFileMetadata, error,
) {
	key := c.resolveKey(location)

	_, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return nil, ErrFileExists
	}
	var notFound *types.NotFound
	if !errors.As(err, &notFound) {
		return nil, fmt.Errorf("failed to check if object exists: %w", err)
	}

	limitedReader := io.LimitReader(reader, fileSizeLimit+1)
	content, err := io.ReadAll(limitedReader)
	if err != nil {
		return nil, fmt.Errorf("failed to read content: %w", err)
	}

	if int64(len(content)) > fileSizeLimit {
		return nil, ErrFileTooLarge
	}

	_, err = c.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(content),
		ContentLength: aws.Int64(int64(len(content))),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to upload object: %w", err)
	}

	headOut, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get object metadata: %w", err)
	}

	modTime := time.Now()
	if headOut.LastModified != nil {
		modTime = *headOut.LastModified
	}

	return &api.BatchFileMetadata{
		Location: key,
		Size:     int64(len(content)),
		ModTime:  modTime,
	}, nil
}

func (c *Client) Retrieve(ctx context.Context, location string) (io.Reader, *api.BatchFileMetadata, error) {
	key := c.resolveKey(location)

	out, err := c.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		if errors.As(err, &noSuchKey) {
			return nil, nil, os.ErrNotExist
		}
		return nil, nil, fmt.Errorf("failed to get object: %w", err)
	}

	var size int64
	if out.ContentLength != nil {
		size = *out.ContentLength
	}

	modTime := time.Now()
	if out.LastModified != nil {
		modTime = *out.LastModified
	}

	return out.Body, &api.BatchFileMetadata{
		Location: key,
		Size:     size,
		ModTime:  modTime,
	}, nil
}

func (c *Client) List(ctx context.Context, location string) ([]api.BatchFileMetadata, error) {
	prefix := c.resolveKey(location)

	var files []api.BatchFileMetadata
	var continuationToken *string

	for {
		out, err := c.s3Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(c.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", err)
		}

		for _, obj := range out.Contents {
			var modTime time.Time
			if obj.LastModified != nil {
				modTime = *obj.LastModified
			}
			files = append(files, api.BatchFileMetadata{
				Location: aws.ToString(obj.Key),
				Size:     aws.ToInt64(obj.Size),
				ModTime:  modTime,
			})
		}

		if !aws.ToBool(out.IsTruncated) {
			break
		}
		continuationToken = out.NextContinuationToken
	}

	return files, nil
}

func (c *Client) Delete(ctx context.Context, location string) error {
	key := c.resolveKey(location)

	_, err := c.s3Client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var notFound *types.NotFound
		if errors.As(err, &notFound) {
			return os.ErrNotExist
		}
		return fmt.Errorf("failed to check if object exists: %w", err)
	}

	_, err = c.s3Client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object: %w", err)
	}

	return nil
}

func (c *Client) GetContext(parentCtx context.Context, timeLimit time.Duration) (context.Context, context.CancelFunc) {
	if timeLimit == 0 {
		timeLimit = c.defaultTimeout
	}
	return context.WithTimeout(parentCtx, timeLimit)
}

func (c *Client) Close() error {
	return nil
}

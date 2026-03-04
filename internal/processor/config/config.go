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

// The processor's configuration definitions.

package config

import (
	"fmt"
	"os"
	"time"

	fsclient "github.com/llm-d-incubation/batch-gateway/internal/files_store/fs"
	s3client "github.com/llm-d-incubation/batch-gateway/internal/files_store/s3"
	"gopkg.in/yaml.v3"
)

type ProcessorConfig struct {
	// TaskWaitTime is the timeout parameter used when dequeueing from the priority queue
	// This should be shorter than PollInterval
	TaskWaitTime time.Duration `yaml:"task_wait_time"`

	// NumWorkers is the fixed number of worker goroutines spawned to process jobs
	NumWorkers int `yaml:"num_workers"`

	// MaxJobConcurrency defines how many lines within a single job are processed concurrently
	MaxJobConcurrency int `yaml:"max_job_concurrency"`

	// PollInterval defines how frequently the processor checks the database for new jobs
	PollInterval time.Duration `yaml:"poll_interval"`

	// QueueTimeBucket defines exponential bucket configs for queue wait time metric
	QueueTimeBucket BucketConfig `yaml:"queue_time_bucket"`

	// ProcessTimeBucket defines exponential bucket configs for process time metric
	ProcessTimeBucket BucketConfig `yaml:"process_time_bucket"`

	// MaxOpenFiles is the maximum number of open files for the plan writer
	MaxOpenFiles int `yaml:"max_open_files"`

	// DatabaseType specifies the database backend: "mock", "redis", or "postgresql" (not yet implemented).
	DatabaseType string `yaml:"database_type"`

	Addr        string `yaml:"addr"`
	SSLCertFile string `yaml:"ssl_cert_file"`
	SSLKeyFile  string `yaml:"ssl_key_file"`
	// TerminateOnObservabilityFailure controls whether observability server failures should terminate the processor.
	// false: best-effort (default), true: fatal.
	TerminateOnObservabilityFailure bool `yaml:"terminate_on_observability_failure"`

	// ShutdownTimeout is the timeout for shutting down the processor
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`

	// WorkDir is the work directory for processor
	WorkDir string `yaml:"work_dir"`

	// Inference Config
	InferenceConfig InferenceConfig `yaml:"inference_config"`

	// UploadRetry controls retry behaviour when uploading output files to shared storage.
	UploadRetry RetryConfig `yaml:"upload_retry"`

	// DefaultOutputExpirationSeconds is the default TTL for batch output/error files in seconds.
	// Used as fallback when the user does not provide output_expires_after in POST /v1/batches.
	// 0 means no expiration (keep until explicitly deleted).
	DefaultOutputExpirationSeconds int64 `yaml:"default_output_expiration_seconds"`

	// ProgressTTLSeconds is the TTL for temporary progress updates in the status store (Redis).
	ProgressTTLSeconds int `yaml:"progress_ttl_seconds"`

	// OTel holds OpenTelemetry-related settings.
	OTel struct {
		RedisTracing bool `yaml:"redis_tracing"`
	} `yaml:"otel"`

	// FileClient holds configuration for the shared file storage client (fs or s3).
	FileClientCfg struct {
		Type     string          `yaml:"type"`
		FSConfig fsclient.Config `yaml:"fs"`
		S3Config s3client.Config `yaml:"s3"`
	} `yaml:"file_client"`
}

type RetryConfig struct {
	MaxRetries     int           `yaml:"max_retries"`
	InitialBackoff time.Duration `yaml:"initial_backoff"`
	MaxBackoff     time.Duration `yaml:"max_backoff"`
}

type InferenceConfig struct {
	// GatewayURL is the base URL of the inference gateway (llm-d or GAIE)
	GatewayURL string `yaml:"gateway_url"`

	// RequestTimeout is the timeout for individual inference requests
	RequestTimeout time.Duration `yaml:"request_timeout"`

	// MaxRetries is the maximum number of retry attempts for failed requests
	MaxRetries int `yaml:"max_retries"`

	// InitialBackoff is the initial backoff duration for retries
	InitialBackoff time.Duration `yaml:"initial_backoff"`

	// MaxBackoff is the maximum backoff duration for retries
	MaxBackoff time.Duration `yaml:"max_backoff"`

	// TLSInsecureSkipVerify skips TLS certificate verification (INSECURE, only for testing)
	TLSInsecureSkipVerify bool `yaml:"tls_insecure_skip_verify"`

	// TLSCACertFile is the path to custom CA certificate file (for private CAs)
	TLSCACertFile string `yaml:"tls_ca_cert_file"`

	// TLSClientCertFile is the path to client certificate file (for mTLS)
	TLSClientCertFile string `yaml:"tls_client_cert_file"`

	// TLSClientKeyFile is the path to client private key file (for mTLS)
	TLSClientKeyFile string `yaml:"tls_client_key_file"`
}

type BucketConfig struct {
	BucketStart  float64 `yaml:"bucket_start"`
	BucketFactor float64 `yaml:"bucket_factor"`
	BucketCount  int     `yaml:"bucket_count"`
}

func (pc *ProcessorConfig) SSLEnabled() bool {
	return pc.SSLCertFile != "" && pc.SSLKeyFile != ""
}

// LoadFromYaml loads the configuration from a YAML file.
func (pc *ProcessorConfig) LoadFromYAML(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(pc); err != nil {
		return err
	}
	return nil
}

// NewConfig returns a new ProcessorConfig with default values.
// TaskWaitTime has to be shorter than poll interval
func NewConfig() *ProcessorConfig {
	return &ProcessorConfig{
		PollInterval: 5 * time.Second,
		TaskWaitTime: 1 * time.Second,
		ProcessTimeBucket: BucketConfig{
			BucketStart:  0.1,
			BucketFactor: 2,
			BucketCount:  15,
		},
		QueueTimeBucket: BucketConfig{
			BucketStart:  0.1,
			BucketFactor: 2,
			BucketCount:  10,
		},

		MaxJobConcurrency: 10,
		NumWorkers:        1,
		Addr:              ":9090",
		// Keep observability as best-effort by default.
		TerminateOnObservabilityFailure: false,
		ShutdownTimeout:                 30 * time.Second,
		WorkDir:                         "/var/lib/batch-gateway/processor",
		MaxOpenFiles:                    50, // default to 50 open files
		DatabaseType:                    "redis",
		FileClientCfg: struct {
			Type     string          `yaml:"type"`
			FSConfig fsclient.Config `yaml:"fs"`
			S3Config s3client.Config `yaml:"s3"`
		}{
			Type: "mock",
		},
		InferenceConfig: InferenceConfig{
			GatewayURL:            "http://localhost:8000",
			RequestTimeout:        5 * time.Minute,
			MaxRetries:            3,
			InitialBackoff:        1 * time.Second,
			MaxBackoff:            60 * time.Second,
			TLSInsecureSkipVerify: false,
		},
		UploadRetry: RetryConfig{
			MaxRetries:     3,
			InitialBackoff: 1 * time.Second,
			MaxBackoff:     10 * time.Second,
		},
		DefaultOutputExpirationSeconds: 90 * 24 * 60 * 60, // 90 days
		ProgressTTLSeconds:             24 * 60 * 60,      // 24 hours
	}
}

func (c *ProcessorConfig) Validate() error {
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll_interval must be > 0")
	}
	if c.TaskWaitTime <= 0 {
		return fmt.Errorf("task_wait_time must be > 0")
	}
	if c.TaskWaitTime >= c.PollInterval {
		return fmt.Errorf("task_wait_time must be shorter than poll_interval")
	}
	if c.NumWorkers <= 0 {
		return fmt.Errorf("num_workers must be > 0")
	}
	if c.MaxJobConcurrency <= 0 {
		return fmt.Errorf("max_job_concurrency must be > 0")
	}
	if c.ShutdownTimeout <= 0 {
		return fmt.Errorf("shutdown_timeout must be > 0")
	}
	if c.Addr == "" {
		return fmt.Errorf("addr cannot be empty")
	}
	if c.WorkDir == "" {
		return fmt.Errorf("work_dir cannot be empty")
	}

	if c.QueueTimeBucket.BucketStart <= 0 || c.QueueTimeBucket.BucketFactor <= 1 || c.QueueTimeBucket.BucketCount <= 0 {
		return fmt.Errorf("queue_time_bucket must satisfy: bucket_start > 0, bucket_factor > 1, bucket_count > 0")
	}
	if c.ProcessTimeBucket.BucketStart <= 0 || c.ProcessTimeBucket.BucketFactor <= 1 || c.ProcessTimeBucket.BucketCount <= 0 {
		return fmt.Errorf("process_time_bucket must satisfy: bucket_start > 0, bucket_factor > 1, bucket_count > 0")
	}

	if c.InferenceConfig.GatewayURL == "" {
		return fmt.Errorf("inference_config.gateway_url cannot be empty")
	}
	if c.InferenceConfig.RequestTimeout <= 0 {
		return fmt.Errorf("inference_config.request_timeout must be > 0")
	}
	if c.InferenceConfig.MaxRetries < 0 {
		return fmt.Errorf("inference_config.max_retries must be >= 0")
	}
	if c.InferenceConfig.InitialBackoff <= 0 {
		return fmt.Errorf("inference_config.initial_backoff must be > 0")
	}
	if c.InferenceConfig.MaxBackoff <= 0 {
		return fmt.Errorf("inference_config.max_backoff must be > 0")
	}
	if c.InferenceConfig.MaxBackoff < c.InferenceConfig.InitialBackoff {
		return fmt.Errorf("inference_config.max_backoff must be >= inference_config.initial_backoff")
	}

	if (c.SSLCertFile == "") != (c.SSLKeyFile == "") {
		return fmt.Errorf("ssl_cert_file and ssl_key_file must both be set or both be empty")
	}
	if c.SSLEnabled() {
		if _, err := os.Stat(c.SSLCertFile); err != nil {
			return err
		}
		if _, err := os.Stat(c.SSLKeyFile); err != nil {
			return err
		}
	}

	if (c.InferenceConfig.TLSClientCertFile == "") != (c.InferenceConfig.TLSClientKeyFile == "") {
		return fmt.Errorf("inference_config.tls_client_cert_file and inference_config.tls_client_key_file must both be set or both be empty")
	}
	if c.InferenceConfig.TLSCACertFile != "" {
		if _, err := os.Stat(c.InferenceConfig.TLSCACertFile); err != nil {
			return err
		}
	}
	if c.InferenceConfig.TLSClientCertFile != "" {
		if _, err := os.Stat(c.InferenceConfig.TLSClientCertFile); err != nil {
			return err
		}
		if _, err := os.Stat(c.InferenceConfig.TLSClientKeyFile); err != nil {
			return err
		}
	}

	if c.UploadRetry.MaxRetries < 0 {
		return fmt.Errorf("upload_retry.max_retries must be >= 0")
	}
	if c.UploadRetry.InitialBackoff <= 0 {
		return fmt.Errorf("upload_retry.initial_backoff must be > 0")
	}
	if c.UploadRetry.MaxBackoff <= 0 {
		return fmt.Errorf("upload_retry.max_backoff must be > 0")
	}
	if c.UploadRetry.MaxBackoff < c.UploadRetry.InitialBackoff {
		return fmt.Errorf("upload_retry.max_backoff must be >= upload_retry.initial_backoff")
	}

	// <= 0 means unlimited.
	if c.MaxOpenFiles <= 0 {
		c.MaxOpenFiles = 0
	}

	if c.ProgressTTLSeconds <= 0 {
		return fmt.Errorf("progress_ttl_seconds must be > 0")
	}

	return nil
}

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
	"os"
	"time"

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

	Addr        string `yaml:"addr"`
	SSLCertFile string `yaml:"ssl_cert_file"`
	SSLKeyFile  string `yaml:"ssl_key_file"`

	// Inference Config
	InferenceConfig InferenceConfig `yaml:"inference_config"`
}

type InferenceConfig struct {
	// GatewayURL is the base URL of the inference gateway (llm-d or GAIE)
	GatewayURL string `yaml:"gateway_url"`

	// RequestTimeout is the timeout for individual inference requests
	RequestTimeout time.Duration `yaml:"request_timeout"`

	// APIKey is the optional API key for authenticating with the inference gateway
	APIKey string `yaml:"api_key"`

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

		InferenceConfig: InferenceConfig{
			GatewayURL:            "http://localhost:8000",
			RequestTimeout:        5 * time.Minute,
			APIKey:                "",
			MaxRetries:            3,
			InitialBackoff:        1 * time.Second,
			MaxBackoff:            60 * time.Second,
			TLSInsecureSkipVerify: false,
		},
	}
}

func (c *ProcessorConfig) Validate() error {
	if c.SSLEnabled() {
		if _, err := os.Stat(c.SSLCertFile); err != nil {
			return err
		}
		if _, err := os.Stat(c.SSLKeyFile); err != nil {
			return err
		}
	}
	return nil
}

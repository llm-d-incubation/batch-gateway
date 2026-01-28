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

	// InferenceGatewayURL is the base URL of the inference gateway (llm-d or GAIE)
	InferenceGatewayURL string `yaml:"inference_gateway_url"`

	// InferenceRequestTimeout is the timeout for individual inference requests
	InferenceRequestTimeout time.Duration `yaml:"inference_request_timeout"`

	// InferenceAPIKey is the optional API key for authenticating with the inference gateway
	InferenceAPIKey string `yaml:"inference_api_key"`

	// InferenceMaxRetries is the maximum number of retry attempts for failed requests
	InferenceMaxRetries int `yaml:"inference_max_retries"`

	// InferenceInitialBackoff is the initial backoff duration for retries
	InferenceInitialBackoff time.Duration `yaml:"inference_initial_backoff"`

	// InferenceMaxBackoff is the maximum backoff duration for retries
	InferenceMaxBackoff time.Duration `yaml:"inference_max_backoff"`

	// InferenceTLSInsecureSkipVerify skips TLS certificate verification (INSECURE, only for testing)
	InferenceTLSInsecureSkipVerify bool `yaml:"inference_tls_insecure_skip_verify"`

	// InferenceTLSCACertFile is the path to custom CA certificate file (for private CAs)
	InferenceTLSCACertFile string `yaml:"inference_tls_ca_cert_file"`

	// InferenceTLSClientCertFile is the path to client certificate file (for mTLS)
	InferenceTLSClientCertFile string `yaml:"inference_tls_client_cert_file"`

	// InferenceTLSClientKeyFile is the path to client private key file (for mTLS)
	InferenceTLSClientKeyFile string `yaml:"inference_tls_client_key_file"`
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

		InferenceGatewayURL:     "http://localhost:8000",
		InferenceRequestTimeout: 5 * time.Minute,
		InferenceAPIKey:         "",
		InferenceMaxRetries:     3,
		InferenceInitialBackoff: 1 * time.Second,
		InferenceMaxBackoff:     60 * time.Second,
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

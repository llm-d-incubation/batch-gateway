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
	"strings"
	"time"

	sharedcfg "github.com/llm-d-incubation/batch-gateway/internal/shared/config"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	"github.com/llm-d-incubation/batch-gateway/internal/util/ptr"
	"github.com/llm-d-incubation/batch-gateway/internal/util/retry"
	inference "github.com/llm-d-incubation/batch-gateway/pkg/clients/inference"
	"gopkg.in/yaml.v3"
)

type ProcessorConfig struct {
	// TaskWaitTime is the timeout parameter used when dequeueing from the priority queue
	// This should be shorter than PollInterval
	TaskWaitTime time.Duration `yaml:"task_wait_time"`

	// NumWorkers is the fixed number of worker goroutines spawned to process jobs
	NumWorkers int `yaml:"num_workers"`

	// GlobalConcurrency limits total in-flight inference requests across all workers in a processor.
	// Protects system resources (goroutines, sockets, memory) from unbounded growth.
	GlobalConcurrency int `yaml:"global_concurrency"`

	// PerModelMaxConcurrency limits concurrent inference requests per individual model.
	// Protects downstream inference gateway from being overwhelmed by a single model's requests.
	PerModelMaxConcurrency int `yaml:"per_model_max_concurrency"`

	// PollInterval defines how frequently the processor checks the database for new jobs
	PollInterval time.Duration `yaml:"poll_interval"`

	// QueueTimeBucket defines exponential bucket configs for queue wait time metric
	QueueTimeBucket BucketConfig `yaml:"queue_time_bucket"`

	// ProcessTimeBucket defines exponential bucket configs for process time metric
	ProcessTimeBucket BucketConfig `yaml:"process_time_bucket"`

	// E2ELatencyBucket defines exponential bucket configs for end-to-end job latency metric.
	// Covers the full lifecycle from submission to terminal state, which can span hours for large jobs.
	E2ELatencyBucket BucketConfig `yaml:"e2e_latency_bucket"`

	// DB client configuration
	DBClientCfg sharedcfg.DBClientConfig `yaml:"db_client"`

	Addr string `yaml:"addr"`
	// TerminateOnObservabilityFailure controls whether observability server failures should terminate the processor.
	// false: best-effort (default), true: fatal.
	TerminateOnObservabilityFailure bool `yaml:"terminate_on_observability_failure"`

	// ShutdownTimeout is the timeout for shutting down the processor
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`

	// WorkDir is the work directory for processor
	WorkDir string `yaml:"work_dir"`

	// GlobalInferenceGateway, when set, routes all inference requests to a
	// single endpoint regardless of model name. Per-model entries in
	// ModelGateways are ignored for routing when this is set.
	// Use this for MaaS / multi-model platforms or LoRA adapter deployments
	// where many model names share one inference endpoint.
	GlobalInferenceGateway *ModelGatewayConfig `yaml:"global_inference_gateway,omitempty"`

	// ModelGateways maps model names to gateway/inference settings.
	// Only models listed here are routed; requests for unlisted models
	// receive a request-level error.
	// Ignored when GlobalInferenceGateway is set.
	ModelGateways map[string]ModelGatewayConfig `yaml:"model_gateways"`

	// DefaultOutputExpirationSeconds is the default TTL for batch output/error files in seconds.
	// Used as fallback when the user does not provide output_expires_after in POST /v1/batches.
	// 0 means no expiration (keep until explicitly deleted).
	DefaultOutputExpirationSeconds int64 `yaml:"default_output_expiration_seconds"`

	// ProgressTTLSeconds is the TTL for temporary progress updates in the status store (Redis).
	ProgressTTLSeconds int `yaml:"progress_ttl_seconds"`

	// RecoveryMaxConcurrency limits concurrent job recoveries during startup.
	// Each recovery can involve DB lookups, S3 uploads, and status updates.
	RecoveryMaxConcurrency int `yaml:"recovery_max_concurrency"`

	// InferenceObjective is the name of a GIE InferenceObjective CRD to reference
	// in the x-gateway-inference-objective header on inference requests.
	// Used by GIE's flow control to assign batch requests to a priority band.
	// Empty (default) means the header is not sent.
	InferenceObjective string `yaml:"inference_objective"`

	// EnablePprof enables pprof profiling endpoints on the observability server.
	EnablePprof bool `yaml:"enable_pprof"`

	// OTelCfg holds OpenTelemetry-related settings.
	OTelCfg sharedcfg.OTelConfig `yaml:"otel"`

	// FileClient holds configuration for the shared file storage client (fs or s3).
	FileClientCfg sharedcfg.FileClientConfig `yaml:"file_client"`
}

// ModelGatewayConfig describes the full gateway and HTTP/TLS settings for one
// model or the global inference gateway. Each per-model entry must be fully
// specified — there is no inheritance between entries.
//
// HTTP fields use pointers so that nil (unset) is distinguishable from explicit
// zero values (e.g. MaxRetries=0 means "no retries", RequestTimeout=0 means
// "no timeout").
//
// API key resolution (mutually exclusive, first match wins):
//   - api_key_file: read the token/key from an arbitrary file path
//     (e.g. /var/run/secrets/kubernetes.io/serviceaccount/token).
//   - api_key_name: key name under /etc/.secrets/ (mounted Kubernetes secret).
//   - (neither set): no API key is sent. For global gateway, the mounted
//     inference-api-key secret is tried as a best-effort fallback.
type ModelGatewayConfig struct {
	URL        string `yaml:"url"`
	APIKeyName string `yaml:"api_key_name"`
	APIKeyFile string `yaml:"api_key_file"`

	RequestTimeout *time.Duration `yaml:"request_timeout"`
	MaxRetries     *int           `yaml:"max_retries"`
	InitialBackoff *time.Duration `yaml:"initial_backoff"`
	MaxBackoff     *time.Duration `yaml:"max_backoff"`

	TLSInsecureSkipVerify bool   `yaml:"tls_insecure_skip_verify"`
	TLSCACertFile         string `yaml:"tls_ca_cert_file,omitempty"`
	TLSClientCertFile     string `yaml:"tls_client_cert_file,omitempty"`
	TLSClientKeyFile      string `yaml:"tls_client_key_file,omitempty"`
}

type BucketConfig struct {
	BucketStart  float64 `yaml:"start"`
	BucketFactor float64 `yaml:"factor"`
	BucketCount  int     `yaml:"count"`
}

// LoadFromYaml loads the configuration from a YAML file.
func (pc *ProcessorConfig) LoadFromYAML(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	return decoder.Decode(pc)
}

// NewConfig returns a new ProcessorConfig with default values.
// Gateway fields (GlobalInferenceGateway, ModelGateways) are intentionally
// left nil — the user must configure exactly one via YAML or env.
// TaskWaitTime has to be shorter than poll interval.
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
		E2ELatencyBucket: BucketConfig{
			BucketStart:  1,
			BucketFactor: 3,
			BucketCount:  12,
		},

		GlobalConcurrency:      100,
		PerModelMaxConcurrency: 10,
		NumWorkers:             1,
		Addr:                   ":9090",
		// Keep observability as best-effort by default.
		TerminateOnObservabilityFailure: false,
		ShutdownTimeout:                 30 * time.Second,
		WorkDir:                         "/var/lib/batch-gateway/processor",
		DBClientCfg: sharedcfg.DBClientConfig{
			Type: sharedcfg.DBTypeRedis,
		},
		FileClientCfg: sharedcfg.FileClientConfig{
			Type: sharedcfg.FileTypeMock,
			Retry: retry.Config{
				MaxRetries:     3,
				InitialBackoff: 1 * time.Second,
				MaxBackoff:     10 * time.Second,
			},
		},
		DefaultOutputExpirationSeconds: 90 * 24 * 60 * 60, // 90 days
		ProgressTTLSeconds:             24 * 60 * 60,      // 24 hours
		RecoveryMaxConcurrency:         5,
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
	if c.GlobalConcurrency <= 0 {
		return fmt.Errorf("global_concurrency must be > 0")
	}
	if c.PerModelMaxConcurrency <= 0 {
		return fmt.Errorf("per_model_max_concurrency must be > 0")
	}
	if c.PerModelMaxConcurrency > c.GlobalConcurrency {
		return fmt.Errorf("per_model_max_concurrency (%d) must be <= global_concurrency (%d)", c.PerModelMaxConcurrency, c.GlobalConcurrency)
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
		return fmt.Errorf("queue_time_bucket must satisfy: start > 0, factor > 1, count > 0")
	}
	if c.ProcessTimeBucket.BucketStart <= 0 || c.ProcessTimeBucket.BucketFactor <= 1 || c.ProcessTimeBucket.BucketCount <= 0 {
		return fmt.Errorf("process_time_bucket must satisfy: start > 0, factor > 1, count > 0")
	}
	if c.E2ELatencyBucket.BucketStart <= 0 || c.E2ELatencyBucket.BucketFactor <= 1 || c.E2ELatencyBucket.BucketCount <= 0 {
		return fmt.Errorf("e2e_latency_bucket must satisfy: start > 0, factor > 1, count > 0")
	}

	if c.GlobalInferenceGateway == nil && len(c.ModelGateways) == 0 {
		return fmt.Errorf("either global_inference_gateway or model_gateways must be configured")
	}
	if c.GlobalInferenceGateway != nil && len(c.ModelGateways) > 0 {
		return fmt.Errorf("global_inference_gateway and model_gateways are mutually exclusive")
	}

	if c.GlobalInferenceGateway != nil {
		if err := validateGatewayConfig("global_inference_gateway", *c.GlobalInferenceGateway); err != nil {
			return err
		}
	}
	for model, gw := range c.ModelGateways {
		if err := validateGatewayConfig(fmt.Sprintf("model_gateways[%s]", model), gw); err != nil {
			return err
		}
	}

	if err := c.FileClientCfg.Retry.Validate(); err != nil {
		return fmt.Errorf("file_client.retry: %w", err)
	}

	if c.ProgressTTLSeconds <= 0 {
		return fmt.Errorf("progress_ttl_seconds must be > 0")
	}
	if c.RecoveryMaxConcurrency <= 0 {
		return fmt.Errorf("recovery_max_concurrency must be > 0")
	}

	return nil
}

func validateGatewayConfig(prefix string, gw ModelGatewayConfig) error {
	if gw.URL == "" {
		return fmt.Errorf("%s.url cannot be empty", prefix)
	}
	if gw.RequestTimeout == nil {
		return fmt.Errorf("%s.request_timeout must be set", prefix)
	}
	if *gw.RequestTimeout < 0 {
		return fmt.Errorf("%s.request_timeout must be >= 0", prefix)
	}
	if gw.MaxRetries == nil {
		return fmt.Errorf("%s.max_retries must be set", prefix)
	}
	if *gw.MaxRetries < 0 {
		return fmt.Errorf("%s.max_retries must be >= 0", prefix)
	}
	if gw.InitialBackoff == nil {
		return fmt.Errorf("%s.initial_backoff must be set", prefix)
	}
	if *gw.InitialBackoff < 0 {
		return fmt.Errorf("%s.initial_backoff must be >= 0", prefix)
	}
	if gw.MaxBackoff == nil {
		return fmt.Errorf("%s.max_backoff must be set", prefix)
	}
	if *gw.MaxBackoff < 0 {
		return fmt.Errorf("%s.max_backoff must be >= 0", prefix)
	}
	if *gw.MaxBackoff < *gw.InitialBackoff {
		return fmt.Errorf("%s.max_backoff must be >= initial_backoff", prefix)
	}
	if gw.APIKeyName != "" && gw.APIKeyFile != "" {
		return fmt.Errorf("%s: api_key_name and api_key_file are mutually exclusive", prefix)
	}
	if gw.APIKeyFile != "" {
		if _, err := os.Stat(gw.APIKeyFile); err != nil {
			return fmt.Errorf("%s.api_key_file: %w", prefix, err)
		}
	}
	if (gw.TLSClientCertFile == "") != (gw.TLSClientKeyFile == "") {
		return fmt.Errorf("%s: tls_client_cert_file and tls_client_key_file must both be set or both be empty", prefix)
	}
	if gw.TLSCACertFile != "" {
		if _, err := os.Stat(gw.TLSCACertFile); err != nil {
			return fmt.Errorf("%s.tls_ca_cert_file: %w", prefix, err)
		}
	}
	if gw.TLSClientCertFile != "" {
		if _, err := os.Stat(gw.TLSClientCertFile); err != nil {
			return fmt.Errorf("%s.tls_client_cert_file: %w", prefix, err)
		}
		if _, err := os.Stat(gw.TLSClientKeyFile); err != nil {
			return fmt.Errorf("%s.tls_client_key_file: %w", prefix, err)
		}
	}
	return nil
}

// resolveGatewayAPIKey resolves the API key for a single gateway config entry.
func resolveGatewayAPIKey(name string, gw ModelGatewayConfig) (string, error) {
	switch {
	case gw.APIKeyFile != "":
		data, err := os.ReadFile(gw.APIKeyFile)
		if err != nil {
			return "", fmt.Errorf("read API key file for %q: %w", name, err)
		}
		return strings.TrimSpace(string(data)), nil
	case gw.APIKeyName != "":
		key, err := ucom.ReadSecretFile(gw.APIKeyName)
		if err != nil {
			return "", fmt.Errorf("read API key for %q: %w", name, err)
		}
		return key, nil
	default:
		return "", nil
	}
}

func toGatewayClientConfig(gw ModelGatewayConfig, apiKey string) inference.GatewayClientConfig {
	return inference.GatewayClientConfig{
		URL:                   gw.URL,
		APIKey:                apiKey,
		Timeout:               ptr.Deref(gw.RequestTimeout),
		MaxRetries:            ptr.Deref(gw.MaxRetries),
		InitialBackoff:        ptr.Deref(gw.InitialBackoff),
		MaxBackoff:            ptr.Deref(gw.MaxBackoff),
		TLSInsecureSkipVerify: gw.TLSInsecureSkipVerify,
		TLSCACertFile:         gw.TLSCACertFile,
		TLSClientCertFile:     gw.TLSClientCertFile,
		TLSClientKeyFile:      gw.TLSClientKeyFile,
	}
}

// ResolvedGateways holds the fully-resolved gateway configs ready for client construction.
type ResolvedGateways struct {
	Global   *inference.GatewayClientConfig
	PerModel map[string]inference.GatewayClientConfig
}

// ResolveModelGateways resolves API keys for all configured gateways and returns
// a ResolvedGateways ready to pass to the inference client resolver.
// Validate() ensures exactly one of GlobalInferenceGateway or ModelGateways is set.
func ResolveModelGateways(cfg *ProcessorConfig) (*ResolvedGateways, error) {
	result := &ResolvedGateways{}

	if cfg.GlobalInferenceGateway != nil {
		apiKey, err := resolveGatewayAPIKey("global_inference_gateway", *cfg.GlobalInferenceGateway)
		if err != nil {
			return nil, err
		}
		if apiKey == "" {
			if key, err := ucom.ReadSecretFile(ucom.SecretKeyInferenceAPI); err == nil {
				apiKey = key
			}
		}
		gc := toGatewayClientConfig(*cfg.GlobalInferenceGateway, apiKey)
		result.Global = &gc
	}

	if len(cfg.ModelGateways) > 0 {
		resolved := make(map[string]inference.GatewayClientConfig, len(cfg.ModelGateways))
		for model, gw := range cfg.ModelGateways {
			apiKey, err := resolveGatewayAPIKey(model, gw)
			if err != nil {
				return nil, err
			}
			resolved[model] = toGatewayClientConfig(gw, apiKey)
		}
		result.PerModel = resolved
	}

	return result, nil
}

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
	DatabaseURL    string        `json:"database_url" yaml:"database_url" mapstructure:"database_url"`
	PollInterval   time.Duration `json:"worker_poll_interval" yaml:"worker_poll_interval" mapstructure:"worker_poll_interval"`
	MaxWorkers     int           `json:"max_workers" yaml:"max_workers" mapstructure:"max_workers"`
	MetricsAddress string        `json:"metrics_address" yaml:"metrics_address" mapstructure:"metrics_address"`
}

// LoadFromYaml loads the configuration from a YAML file.
func (c *ProcessorConfig) LoadFromYAML(filePath string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	if err := decoder.Decode(c); err != nil {
		return err
	}
	return nil
}

// NewConfig returns a new ProcessorConfig with default values.
func NewConfig() *ProcessorConfig {
	return &ProcessorConfig{
		DatabaseURL:    "",
		PollInterval:   5 * time.Second,
		MaxWorkers:     10,
		MetricsAddress: ":9090",
	}
}

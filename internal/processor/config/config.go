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
	TaskWaitTime      time.Duration `yaml:"task_wait_time"`
	MaxWorkers        int           `yaml:"max_workers"`
	MaxJobConcurrency int           `yaml:"max_job_concurrency"`
	PollInterval      time.Duration `yaml:"poll_interval"`
	Port              string        `yaml:"port"`
	SSLCertFile       string        `yaml:"ssl_cert_file"`
	SSLKeyFile        string        `yaml:"ssl_key_file"`
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

		MaxJobConcurrency: 10,
		MaxWorkers:        10,
		Port:              ":9090",
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

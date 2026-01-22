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

// The file implements server configuration management and validation for API server.
package common

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/klog/v2"
)

// Use snake_case for YAML, and use camelCase for JSON
type ServerConfig struct {
	Host        string `json:"host" yaml:"host"`
	Port        string `json:"port" yaml:"port"`
	SSLCertFile string `json:"sslCertFile" yaml:"ssl_cert_file"`
	SSLKeyFile  string `json:"sslKeyFile" yaml:"ssl_key_file"`
}

func NewConfig() *ServerConfig {
	return &ServerConfig{}
}

func (c *ServerConfig) Load() error {
	// Initialize flags (including klog flags)
	fs := flag.NewFlagSet("batch-gateway-apiserver", flag.ContinueOnError)
	klog.InitFlags(fs)

	var configFile string
	fs.StringVar(&configFile, "config", "", "path to config file")
	fs.StringVar(&c.Host, "host", "", "server host")
	fs.StringVar(&c.Port, "port", "8000", "server port")
	fs.StringVar(&c.SSLCertFile, "ssl-cert-file", "", "SSL certificate file")
	fs.StringVar(&c.SSLKeyFile, "ssl-key-file", "", "SSL key file")

	// Parse all flags (klog flags and application flags)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}

	// If config file is provided, load from file (config file values replace CLI flag values)
	if configFile != "" {
		if err := c.loadFromFile(configFile); err != nil {
			return err
		}
	}

	return c.Validate()
}

func (c *ServerConfig) Validate() error {
	if c.Port == "" {
		return fmt.Errorf("port cannot be empty")
	}

	// If one SSL file is provided, both must be provided
	if (c.SSLCertFile != "" && c.SSLKeyFile == "") || (c.SSLCertFile == "" && c.SSLKeyFile != "") {
		return fmt.Errorf("both ssl-cert-file and ssl-private-key-file must be provided together")
	}

	// Verify SSL files exist if provided
	if c.SSLCertFile != "" {
		if _, err := os.Stat(c.SSLCertFile); err != nil {
			return fmt.Errorf("ssl cert file not found: %w", err)
		}
		if _, err := os.Stat(c.SSLKeyFile); err != nil {
			return fmt.Errorf("ssl key file not found: %w", err)
		}
	}

	return nil
}

// LoadFromFile loads configuration from a JSON or YAML file
// File format is determined by extension (.json, .yaml, .yml)
// When config file is specified, it overrides all CLI flags
func (c *ServerConfig) loadFromFile(path string) error {
	if path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	// Determine file format by extension
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".json":
		if err := json.Unmarshal(data, c); err != nil {
			return fmt.Errorf("failed to parse JSON config file: %w", err)
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, c); err != nil {
			return fmt.Errorf("failed to parse YAML config file: %w", err)
		}
	default:
		return fmt.Errorf("unsupported config file format: %s (supported: .json, .yaml, .yml)", ext)
	}

	return nil
}

func (c *ServerConfig) SSLEnabled() bool {
	return (c.SSLCertFile != "" && c.SSLKeyFile != "")
}

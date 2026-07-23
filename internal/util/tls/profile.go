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

package tls

import (
	"crypto/tls"
	"fmt"
	"strings"
)

// ParseMinVersion converts a TLS version name to its crypto/tls constant.
// An empty string defaults to TLS 1.2 for backward compatibility.
func ParseMinVersion(v string) (uint16, error) {
	switch v {
	case "", "VersionTLS12":
		return tls.VersionTLS12, nil
	case "VersionTLS13":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unsupported TLS version %q: must be VersionTLS12 or VersionTLS13", v)
	}
}

// ParseCipherSuites converts a comma-separated list of cipher suite names
// to their uint16 IDs. An empty string returns nil (Go default cipher list).
// Only cipher suites from tls.CipherSuites() (secure) are accepted.
func ParseCipherSuites(s string) ([]uint16, error) {
	if s == "" {
		return nil, nil
	}

	secure := make(map[string]uint16)
	for _, cs := range tls.CipherSuites() {
		secure[cs.Name] = cs.ID
	}

	insecure := make(map[string]bool)
	for _, cs := range tls.InsecureCipherSuites() {
		insecure[cs.Name] = true
	}

	parts := strings.Split(s, ",")
	ids := make([]uint16, 0, len(parts))
	for _, name := range parts {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if id, ok := secure[name]; ok {
			ids = append(ids, id)
			continue
		}
		if insecure[name] {
			return nil, fmt.Errorf("cipher suite %q is insecure and not supported", name)
		}
		return nil, fmt.Errorf("unknown cipher suite %q", name)
	}

	if len(ids) == 0 {
		return nil, nil
	}
	return ids, nil
}

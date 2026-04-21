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

// This file provides tls utilities.

package tls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

type Certificates struct {
	Dir        string
	CertFile   string
	KeyFile    string
	CaCertFile string
}

func (c Certificates) IsEmpty() bool {
	return reflect.ValueOf(c).IsZero()
}

type LoadType int

const (
	LOAD_TYPE_CLIENT LoadType = iota
	LOAD_TYPE_SERVER
)

func GetTlsConfig(loadType LoadType, insecure bool, certFile string, keyFile string, caCertFile string) (*tls.Config, error) {
	var tlsConf tls.Config
	if certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("GetTlsConfig: LoadX509KeyPair failed: %v", err) // pragma: allowlist secret
		}
		tlsConf.Certificates = []tls.Certificate{certificate}
	}

	if insecure {
		tlsConf.InsecureSkipVerify = true
	} else if caCertFile != "" {
		ca, err := os.ReadFile(caCertFile)
		if err != nil {
			return nil, fmt.Errorf("GetTlsConfig: Could not read CA certificate file: %v", err) // pragma: allowlist secret
		}
		certPool := x509.NewCertPool()
		if ok := certPool.AppendCertsFromPEM(ca); !ok {
			return nil, fmt.Errorf("GetTlsConfig: AppendCertsFromPEM failed") // pragma: allowlist secret
		}
		if loadType == LOAD_TYPE_CLIENT {
			tlsConf.RootCAs = certPool
		} else {
			tlsConf.ClientCAs = certPool
			tlsConf.ClientAuth = tls.RequireAndVerifyClientCert // pragma: allowlist secret // To enable self signed client cert use tls.RequireAnyClientCert
		}
	}
	return &tlsConf, nil
}

// Return the cert path only when file is not empty.
func JoinCertPath(dir, file string) string {
	if len(file) > 0 {
		return filepath.Join(dir, file)
	}
	return ""
}

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

// The file provides the HTTP server implementation for the batch gateway API.
package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/batch"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/files"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/health"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/middleware"
	"k8s.io/klog/v2"
)

type Server struct {
	logger klog.Logger
	config *common.ServerConfig
}

func New(config *common.ServerConfig) (*Server, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	logger := klog.Background().WithName("api server")
	return &Server{config: config, logger: logger}, nil
}

// Start the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	logger := s.logger

	ln, err := net.Listen("tcp", s.config.Host+":"+s.config.Port)
	if err != nil {
		logger.Error(err, "failed to start")
		return err
	}

	handler := s.buildHandler()

	httpserver := &http.Server{
		Handler: handler,
	}

	// Enable TLS if cert and key are provided
	if s.config.SSLEnabled() {
		cert, err := tls.LoadX509KeyPair(s.config.SSLCertFile, s.config.SSLKeyFile)
		if err != nil {
			return err
		}
		httpserver.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
				tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			},
		}
		s.logger.Info("server TLS configured")
	} else if s.config.SSLCertFile != "" || s.config.SSLKeyFile != "" {
		err := fmt.Errorf("both tls-cert-file and tls-private-key-file must be provided to enable TLS")
		return err
	}

	// graceful termination
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")

		shutdownCtx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		if err := httpserver.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "failed to gracefully shutdown")
		} else {
			logger.Info("shutdown complete")
		}
	}()

	logger.Info("starting", "addr", ln.Addr().String())
	if s.config.SSLEnabled() {
		if err := httpserver.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "failed to start")
			return err
		}
	} else {
		if err := httpserver.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error(err, "failed to start")
			return err
		}
	}

	return nil
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// register handlers
	healthHandler := health.NewHealthApiHandler()
	metricsHandler := metrics.NewMetricsApiHandler()
	filesHandler := files.NewFilesApiHandler()
	batchHandler := batch.NewBatchApiHandler()

	handlers := []common.ApiHandler{
		healthHandler,
		metricsHandler,
		filesHandler,
		batchHandler,
	}
	for _, c := range handlers {
		common.RegisterHandler(mux, c)
	}

	// register middlewares
	var h http.Handler
	h = middleware.RecoveryMiddleware(mux) // Innermost, catches panics from business logic
	//h = middleware.BodySizeLimitMiddleware(h) //  Limit request body size
	//h = middleware.AuthorizationMiddleware(h) //  Check permissions
	//h = middleware.AuthenticationMiddleware(h) // Verify API key/JWT
	h = middleware.RequestMiddleware(h) // Request ID, logging, metrics
	//h = middleware.RateLimitMiddleware(h)      // Early Rejection
	h = middleware.SecurityHeadersMiddleware(h) // Outermost, affects all responses

	return h
}

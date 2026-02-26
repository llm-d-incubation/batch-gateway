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
	"sync/atomic"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/batch"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/file"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/health"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/middleware"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/readiness"
	"github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"k8s.io/klog/v2"
)

type Server struct {
	logger      klog.Logger
	config      *common.ServerConfig
	serverReady *atomic.Bool
}

func New(config *common.ServerConfig) (*Server, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	logger := klog.Background().WithName("api_server")
	serverReady := &atomic.Bool{}
	serverReady.Store(false)
	return &Server{
		config:      config,
		logger:      logger,
		serverReady: serverReady,
	}, nil
}

// Start the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	logger := s.logger

	ln, err := net.Listen("tcp", s.config.Host+":"+s.config.Port)
	if err != nil {
		logger.Error(err, "failed to start")
		return err
	}
	defer ln.Close()

	handler := s.buildHandler()

	httpserver := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: time.Duration(s.config.GetReadHeaderTimeoutSeconds()) * time.Second,
		ReadTimeout:       time.Duration(s.config.GetReadTimeoutSeconds()) * time.Second,
		WriteTimeout:      time.Duration(s.config.GetWriteTimeoutSeconds()) * time.Second,
		IdleTimeout:       time.Duration(s.config.GetIdleTimeoutSeconds()) * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
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
			// CipherSuites omitted - use Go's secure defaults
			// This allows TLS 1.3 to use its own cipher suites
		}
		s.logger.Info("server TLS configured", "minVersion", "TLS 1.2")
	} else if s.config.SSLCertFile != "" || s.config.SSLKeyFile != "" {
		err := fmt.Errorf("both tls-cert-file and tls-private-key-file must be provided to enable TLS")
		return err
	}

	logger.Info("starting", "addr", ln.Addr().String())

	// Start serving in a goroutine
	serveDone := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error(nil, "server goroutine panicked", "panic", r)
				serveDone <- fmt.Errorf("server panicked: %v", r)
			}
		}()
		var err error
		if s.config.SSLEnabled() {
			err = httpserver.ServeTLS(ln, "", "")
		} else {
			err = httpserver.Serve(ln)
		}
		serveDone <- err
	}()

	// Wait for immediate startup failure or mark ready after 100ms
	select {
	case <-time.After(100 * time.Millisecond):
		logger.Info("server is ready")
		s.serverReady.Store(true)
	case err := <-serveDone:
		logger.Error(err, "server failed to start")
		return err
	case <-ctx.Done():
		logger.Info("shutdown requested before server ready", "reason", ctx.Err())
		return ctx.Err()
	}

	// Continue waiting for shutdown or failure after marking ready
	select {
	case <-ctx.Done():
		// Normal shutdown path
		s.serverReady.Store(false)
		logger.Info("shutting down", "reason", ctx.Err())

		// Gracefully shutdown the server
		shutdownCtx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		if err := httpserver.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "failed to gracefully shutdown")
		}

		// Wait for server goroutine to finish with timeout
		select {
		case err = <-serveDone:
			if err != nil && err != http.ErrServerClosed {
				logger.Error(err, "server exited with error after shutdown")
				return err
			}
		case <-time.After(5 * time.Second):
			logger.Error(nil, "timeout waiting for server goroutine to exit")
			return fmt.Errorf("server goroutine did not exit after shutdown")
		}

		logger.Info("shutdown complete")

	case err := <-serveDone:
		// Server failed after becoming ready
		s.serverReady.Store(false)
		if err != nil && err != http.ErrServerClosed {
			logger.Error(err, "server exited unexpectedly")
			return err
		}
	}

	return nil
}

func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	// TODO: change to actual implementation
	batchDBClient := mockdb.NewMockDBClient[api.BatchItem, api.BatchQuery](
		func(b *api.BatchItem) string { return b.ID },
		func(q *api.BatchQuery) *api.BaseQuery { return &q.BaseQuery },
	)
	fileDBClient := mockdb.NewMockDBClient[api.FileItem, api.FileQuery](
		func(f *api.FileItem) string { return f.ID },
		func(q *api.FileQuery) *api.BaseQuery { return &q.BaseQuery },
	)
	eventClient := mockdb.NewMockBatchEventChannelClient()
	queueClient := mockdb.NewMockBatchPriorityQueueClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	// register handlers
	healthHandler := health.NewHealthApiHandler()
	readinessHandler := readiness.NewReadinessApiHandler(s.serverReady)
	metricsHandler := metrics.NewMetricsApiHandler()
	fileHandler := file.NewFileAPIHandler(s.config, fileDBClient, filesClient)
	batchHandler := batch.NewBatchAPIHandler(s.config, batchDBClient, fileDBClient, queueClient, eventClient, statusClient)

	handlers := []common.ApiHandler{
		healthHandler,
		readinessHandler,
		metricsHandler,
		fileHandler,
		batchHandler,
	}
	for _, c := range handlers {
		common.RegisterHandler(mux, c)
	}

	// register middlewares
	var h http.Handler = mux
	//h = middleware.BodySizeLimitMiddleware(h) //  Limit request body size
	//h = middleware.AuthorizationMiddleware(h) //  Check permissions
	//h = middleware.AuthenticationMiddleware(h) // Verify API key/JWT
	//h = middleware.RateLimitMiddleware(h)      // Early Rejection
	h = middleware.SecurityHeadersMiddleware(h)   // Add security headers
	h = middleware.RequestMiddleware(s.config)(h) // 2nd Outermost, request monitoring with tenant support
	h = middleware.RecoveryMiddleware(h)          // Outermost - catches ALL panics

	return h
}

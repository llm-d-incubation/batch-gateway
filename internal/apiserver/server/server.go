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
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
	"k8s.io/klog/v2"
)

type Server struct {
	logger      klog.Logger
	config      *common.ServerConfig
	serverReady *atomic.Bool
	handler     http.Handler
	clients     *clientset.Clientset
}

func buildClients(ctx context.Context, config *common.ServerConfig) (*clientset.Clientset, error) {
	logger := klog.FromContext(ctx)

	redisCfg := &uredis.RedisClientConfig{
		ServiceName:   "batch-apiserver",
		EnableTracing: config.OTel.RedisTracing,
	}

	clients, err := clientset.NewClientset(
		ctx,
		config.DatabaseType,
		nil,
		redisCfg,
		config.FileClientCfg.Type,
		&config.FileClientCfg.FSConfig,
		&config.FileClientCfg.S3Config,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create clients: %w", err)
	}
	logger.Info("clients initialized")

	return clients, nil
}

func New(ctx context.Context, config *common.ServerConfig) (*Server, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	logger := klog.Background().WithName("api_server")
	serverReady := &atomic.Bool{}
	serverReady.Store(false)

	// build clients
	clients, err := buildClients(ctx, config)
	if err != nil {
		return nil, err
	}

	// build HTTP handlers
	mux := http.NewServeMux()
	healthHandler := health.NewHealthApiHandler()
	readinessHandler := readiness.NewReadinessApiHandler(serverReady)
	metricsHandler := metrics.NewMetricsApiHandler()
	fileHandler := file.NewFileAPIHandler(config, clients)
	batchHandler := batch.NewBatchAPIHandler(config, clients)

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
	var handler http.Handler = mux
	//handler = middleware.BodySizeLimitMiddleware(handler) //  Limit request body size
	//handler = middleware.AuthorizationMiddleware(handler) //  Check permissions
	//handler = middleware.AuthenticationMiddleware(handler) // Verify API key/JWT
	//handler = middleware.RateLimitMiddleware(handler)      // Early Rejection
	handler = middleware.SecurityHeadersMiddleware(handler) // Add security headers
	handler = middleware.RequestMiddleware(config)(handler) // 2nd Outermost, request monitoring with tenant support
	handler = middleware.RecoveryMiddleware(handler)        // Outermost - catches ALL panics

	return &Server{
		config:      config,
		logger:      logger,
		serverReady: serverReady,
		handler:     handler,
		clients:     clients,
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

	httpserver := &http.Server{
		Handler:           s.handler,
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

		if err := s.clients.Close(); err != nil {
			logger.Error(err, "failed to close clients")
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

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

// The entry point for the worker process.

package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"time"

	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/worker"
	"github.com/llm-d-incubation/batch-gateway/internal/util/interrupt"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	"github.com/llm-d-incubation/batch-gateway/internal/util/tls"
)

func main() {
	// initialize klog
	klog.InitFlags(nil)
	defer klog.Flush()

	// load configuration & logging setup
	rootLogger := klog.Background()
	ctx := klog.NewContext(context.Background(), rootLogger)

	hostname, _ := os.Hostname()
	rootLogger = rootLogger.WithValues("hostname", hostname, "service", "batch-processor")
	ctx = klog.NewContext(ctx, rootLogger)

	logger := klog.FromContext(ctx)

	cfg := config.NewConfig()
	fs := flag.NewFlagSet("batch-gateway-processor", flag.ExitOnError)

	cfgFilePath := fs.String("config", "cmd/batch-processor/config.yaml", "Path to configuration file")
	klog.InitFlags(fs)
	fs.Parse(os.Args[1:])

	if err := cfg.LoadFromYAML(*cfgFilePath); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to load config file. Processor cannot start", "path", *cfgFilePath, "err", err)
		os.Exit(1)
	}

	// metrics setup
	if err := metrics.InitMetrics(*cfg); err != nil {
		logger.V(logging.ERROR).Error(err, "Failed to initialize metrics")
		os.Exit(1)
	}
	logger.V(logging.INFO).Info("Metrics initialized", "numWorkers", cfg.NumWorkers)

	// setup context with graceful shutdown
	ctx, cancel := interrupt.ContextWithSignal(ctx)
	defer cancel()

	go func() {
		m := http.NewServeMux()
		m.Handle("/metrics", metrics.NewMetricsHandler())
		m.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})

		server := &http.Server{
			Addr:    cfg.Addr,
			Handler: m,
		}

		// tls setup
		if cfg.SSLEnabled() {
			tlsConfig, err := tls.GetTlsConfig(tls.LOAD_TYPE_SERVER, false, cfg.SSLCertFile, cfg.SSLKeyFile, "")
			if err != nil {
				logger.V(logging.ERROR).Error(err, "Failed to configure TLS for observability server")
				return
			}
			server.TLSConfig = tlsConfig
			logger.V(logging.INFO).Info("Observability server TLS configured")
		}

		// http server shutdown when context cancels
		go func() {
			<-ctx.Done()
			logger.V(logging.INFO).Info("Shutting down observability server")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				logger.V(logging.ERROR).Error(err, "Observability server shutdown failed")
			}
		}()

		logger.V(logging.INFO).Info("Start observability server", "port", cfg.Addr, "tls", cfg.SSLEnabled())

		var err error
		if cfg.SSLEnabled() {
			err = server.ListenAndServeTLS("", "")
		} else {
			err = server.ListenAndServe()
		}

		if err != nil && err != http.ErrServerClosed {
			logger.V(logging.ERROR).Error(err, "Observability server failed")
		}

	}()

	// Todo:: db/llmd client setup
	var dbClient db.BatchDBClient
	var pqClient db.BatchPriorityQueueClient
	var statusClient db.BatchStatusClient
	var eventClient db.BatchEventChannelClient

	// Initialize inference client with configuration
	inferenceClient := inference.NewHTTPClient(inference.HTTPClientConfig{
		BaseURL:                   cfg.InferenceGatewayURL,
		Timeout:                   cfg.InferenceRequestTimeout,
		APIKey:                    cfg.InferenceAPIKey,
		MaxRetries:                cfg.InferenceMaxRetries,
		InitialBackoff:            cfg.InferenceInitialBackoff,
		MaxBackoff:                cfg.InferenceMaxBackoff,
		TLSInsecureSkipVerify:     cfg.InferenceTLSInsecureSkipVerify,
		TLSCACertFile:             cfg.InferenceTLSCACertFile,
		TLSClientCertFile:         cfg.InferenceTLSClientCertFile,
		TLSClientKeyFile:          cfg.InferenceTLSClientKeyFile,
	})
	logger.V(logging.INFO).Info("Initialized inference client",
		"baseURL", cfg.InferenceGatewayURL,
		"timeout", cfg.InferenceRequestTimeout,
		"maxRetries", cfg.InferenceMaxRetries)

	processorClients := worker.NewProcessorClients(
		dbClient, pqClient, statusClient, eventClient, inferenceClient,
	)

	// initialize processor (worker pool manager)
	// get max worker from cfg then decide the worker pool size
	logger.V(logging.INFO).Info("Initializing worker processor", "maxWorkers", cfg.NumWorkers)
	proc := worker.NewProcessor(cfg, &processorClients)

	// start the main polling loop
	// this polls for new tasks, check for empty worker slots, and assign tasks to workers
	logger.V(logging.INFO).Info("Processor polling loop started", "pollInterval", cfg.PollInterval.String())
	if err := proc.RunPollingLoop(ctx); err != nil {
		logger.V(logging.ERROR).Error(err, "Processor polling loop exited with error")
		os.Exit(1)
	}

	// cleanup and shutdown
	logger.V(logging.INFO).Info("Processor exited, shutting down")
	proc.Stop(ctx) // wait for all workers to finish
	logger.V(logging.INFO).Info("Processor exited gracefully")
}

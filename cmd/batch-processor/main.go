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
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/worker"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	"github.com/llm-d-incubation/batch-gateway/internal/util/interrupt"
	"github.com/llm-d-incubation/batch-gateway/internal/util/logging"
	uotel "github.com/llm-d-incubation/batch-gateway/internal/util/otel"
	uredis "github.com/llm-d-incubation/batch-gateway/internal/util/redis"
)

func main() {
	defer klog.Flush()

	if err := run(); err != nil {
		klog.ErrorS(err, "Processor failed to start")
		klog.Flush() // Must flush manually before os.Exit
		os.Exit(1)
	}
}

func run() error {
	// load configuration & logging setup
	hostname, _ := os.Hostname()
	logger := klog.Background().WithValues("hostname", hostname, "service", "batch-processor")
	ctx := klog.NewContext(context.Background(), logger)

	cfg := config.NewConfig()
	fs := flag.NewFlagSet("batch-gateway-processor", flag.ExitOnError)

	cfgFilePath := fs.String("config", "cmd/batch-processor/config.yaml", "Path to configuration file")
	klog.InitFlags(fs)
	fs.Parse(os.Args[1:])

	if err := cfg.LoadFromYAML(*cfgFilePath); err != nil {
		logger.Error(err, "Failed to load config file. Processor cannot start", "path", *cfgFilePath, "err", err)
		return err
	}

	if err := cfg.Validate(); err != nil {
		logger.Error(err, "Invalid config. Processor cannot start", "err", err)
		return err
	}

	// initialize OpenTelemetry tracing
	shutdownTracer, err := uotel.InitTracer(ctx)
	if err != nil {
		logger.Error(err, "Failed to initialize tracer")
		return err
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		shutdownTracer(shutdownCtx)
	}()

	// metrics setup
	if err := metrics.InitMetrics(*cfg); err != nil {
		logger.Error(err, "Failed to initialize metrics")
		return err
	}
	logger.V(logging.INFO).Info("Metrics initialized", "numWorkers", cfg.NumWorkers)

	// setup context with graceful shutdown
	ctx, cancel := interrupt.ContextWithSignal(ctx)
	defer cancel()

	// readiness starts as false and flips right before entering polling loop execution.
	var ready atomic.Bool
	// read only channel for observability server's fatal error
	obsFatalCh := startObservabilityServer(
		ctx,
		logger,
		cfg,
		&ready,
		cancel,
		cfg.TerminateOnObservabilityFailure,
	)

	procClients, err := buildProcessorClients(ctx, cfg)
	if err != nil {
		logger.Error(err, "Failed to build processor clients")
		return err
	}
	defer procClients.Close()

	if err := worker.ValidateClientset(procClients); err != nil {
		logger.Error(err, "Processor client validation failed")
		return err
	}

	// init processor
	logger.V(logging.INFO).Info("Initializing worker processor", "maxWorkers", cfg.NumWorkers)
	proc := worker.NewProcessor(cfg, procClients)
	defer func() {
		// stop with a fresh timeout ctx (avoid already-cancelled ctx)
		// timeout should be less than k8s terminationGracePeriodSeconds
		stopCtx, stopCtxCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer stopCtxCancel()
		logger.V(logging.INFO).Info("Processor exited, shutting down")
		proc.Stop(stopCtx) // wait for all workers to finish
		logger.V(logging.INFO).Info("Processor exited gracefully")
	}()

	// start the main polling loop
	// ready indicates the processor can actively run the polling loop.
	ready.Store(true)
	go func() {
		<-ctx.Done()
		ready.Store(false)
	}()
	logger.V(logging.INFO).Info("Processor polling loop started", "pollInterval", cfg.PollInterval.String())
	err = proc.Run(ctx)
	if cfg.TerminateOnObservabilityFailure {
		// Give the observability goroutine a brief chance to publish the fatal cause,
		// so we can prefer it over a derived context-cancel error from the polling loop.
		if obsErr := waitObservabilityFatalError(ctx, obsFatalCh, 100*time.Millisecond); obsErr != nil {
			logger.Error(obsErr, "Processor stopped due to observability server failure")
			return obsErr
		}
	}
	if err != nil {
		logger.Error(err, "Processor polling loop exited with error")
		return err
	}
	return nil
}

func startObservabilityServer(
	ctx context.Context,
	logger klog.Logger,
	cfg *config.ProcessorConfig,
	ready *atomic.Bool,
	cancel context.CancelFunc,
	terminateOnObservabilityFailure bool,
) <-chan error {
	errCh := make(chan error, 1)

	go func() {
		// event channel - no need to close (1 buffer, max 1 event sent)
		reportFatal := func(err error) {
			if err == nil {
				return
			}
			if !terminateOnObservabilityFailure {
				logger.Error(err, "Observability server failed in best-effort mode; processor will continue")
				return
			}

			// Keep observability failure as primary shutdown cause.
			select {
			case errCh <- err:
			default:
			}
			cancel()
		}

		m := http.NewServeMux()
		m.Handle("/metrics", metrics.NewMetricsHandler())
		m.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})
		// ready endpoint - indicates the processor is ready to process requests
		m.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
			if !ready.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte("not ready"))
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok"))
		})

		server := &http.Server{
			Addr:              cfg.Addr,
			Handler:           m,
			ReadHeaderTimeout: 10 * time.Second,
		}

		go func() {
			<-ctx.Done()
			logger.V(logging.INFO).Info("Shutting down observability server")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := server.Shutdown(shutdownCtx); err != nil {
				logger.Error(err, "Observability server shutdown failed")
			}
		}()

		logger.V(logging.INFO).Info("Start observability server", "addr", cfg.Addr)

		err := server.ListenAndServe()

		if err != nil && err != http.ErrServerClosed {
			logger.Error(err, "Observability server failed")
			reportFatal(err)
		}
	}()

	return errCh
}

func waitObservabilityFatalError(ctx context.Context, obsFatalCh <-chan error, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case err, ok := <-obsFatalCh:
		if ok {
			return err
		}
		return nil
	case <-ctx.Done():
		// graceful shutdown can race with publishing observability fatal causes.
		// wait a short additional window and prefer the explicit fatal error when present.
		fallback := time.NewTimer(wait)
		defer fallback.Stop()
		select {
		case err, ok := <-obsFatalCh:
			if ok {
				return err
			}
			return nil
		case <-fallback.C:
			return nil
		}
	case <-timer.C:
		return nil
	}
}

// buildProcessorClients constructs all processor clients using the same backend as the apiserver
func buildProcessorClients(ctx context.Context, cfg *config.ProcessorConfig) (*clientset.Clientset, error) {
	logger := klog.FromContext(ctx)

	redisCfg := &uredis.RedisClientConfig{
		ServiceName:   "batch-processor",
		EnableTracing: cfg.OTel.RedisTracing,
	}

	modelGatewaysConfigs, err := config.ResolveModelGateways(cfg.ModelGateways)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve model gateways: %w", err)
	}
	cfg.PostgreSQLCfg.EnableTracing = cfg.OTel.PostgresqlTracing

	clients, err := clientset.NewClientset(
		ctx,
		cfg.DatabaseType,
		&cfg.PostgreSQLCfg,
		redisCfg,
		cfg.FileClientCfg.Type,
		&cfg.FileClientCfg.FSConfig,
		&cfg.FileClientCfg.S3Config,
		modelGatewaysConfigs,
	)
	if err != nil {
		logger.Error(err, "Failed to create clients")
		return nil, err
	}

	logger.V(logging.INFO).Info("Processor clients initialized",
		"defaultInferenceURL", cfg.ModelGateways[config.DefaultModelGatewayKey].URL,
		"numModelOverrides", len(cfg.ModelGateways)-1,
		"fileClientType", cfg.FileClientCfg.Type)

	return clients, nil
}

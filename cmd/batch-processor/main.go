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

	"k8s.io/klog/v2"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/worker"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/batch"
	"github.com/llm-d-incubation/batch-gateway/internal/util/interrupt"
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
		logger.Info("Failed to load config file. Processor cannot start", "path", *cfgFilePath, "err", err)
		os.Exit(1)
	}

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
		logger.Info("Starting observability server")
		if err := http.ListenAndServe(cfg.MetricsAddress, m); err != nil {
			logger.Error(err, "Observability server failed")
		}
	}()

	// Todo:: db/llmd client setup
	var dbClient db.BatchDBClient
	var pqClient db.BatchPriorityQueueClient
	var statusClient db.BatchStatusClient
	var eventClient db.BatchEventChannelClient
	var inferenceClient batch.InferenceClient
	processorClients := worker.ProcessorClients{
		Database:      dbClient,
		PriorityQueue: pqClient,
		Status:        statusClient,
		Event:         eventClient,
		Inference:     inferenceClient,
	}

	// initialize processor (worker pool manager)
	// get max worker from cfg then decide the worker pool size
	logger.Info("Initializing worker processor", "maxWorkers", cfg.MaxWorkers)
	proc := worker.NewProcessor(cfg, processorClients)

	// start the main polling loop
	// this polls for new tasks, check for empty worker slots, and assign tasks to workers
	logger.Info("Processor polling loop started", "pollInterval", cfg.PollInterval.String())
	if err := proc.RunPollingLoop(ctx); err != nil {
		logger.Error(err, "Processor polling loop exited with error")
		klog.Flush()
		os.Exit(1)
	}

	// cleanup and shutdown
	logger.Info("Processor exited, shutting down")
	proc.Stop(ctx) // wait for all workers to finish
	logger.Info("Processor exited gracefully")
}

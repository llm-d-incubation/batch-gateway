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

// The entry point for the batch garbage collector.
// This command runs as a long-lived process that periodically scans for
// expired batch jobs and files and removes them from the database.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"

	"github.com/llm-d-incubation/batch-gateway/internal/gc/collector"
	gcconfig "github.com/llm-d-incubation/batch-gateway/internal/gc/config"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
	"github.com/llm-d-incubation/batch-gateway/internal/util/interrupt"
)

func main() {
	defer klog.Flush()

	if err := run(); err != nil {
		klog.Fatalf("Garbage collector failed: %v", err)
	}
}

func run() error {
	hostname, _ := os.Hostname()
	logger := klog.NewKlogr().WithValues("hostname", hostname, "service", "batch-gc")
	ctx := logr.NewContext(context.Background(), logger)

	flagSet := flag.NewFlagSet("batch-gc", flag.ExitOnError)
	configFile := flagSet.String("config", "./config.yaml", "path to YAML config file")
	klog.InitFlags(flagSet)
	_ = flagSet.Parse(os.Args[1:]) // ExitOnError mode handles errors

	cfg, err := gcconfig.Load(*configFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	logger.Info("Starting batch garbage collector", "dryRun", cfg.DryRun, "interval", cfg.Interval)

	ctx, cancel := interrupt.ContextWithSignal(ctx)
	defer cancel()

	cfg.DBClientCfg.RedisCfg.ServiceName = "batch-gc"

	clients, err := clientset.NewClientset(ctx, ucom.ComponentGC,
		clientset.WithDB(cfg.DBClientCfg),
		clientset.WithFile(cfg.FileClientCfg),
	)
	if err != nil {
		return fmt.Errorf("failed to create clients: %w", err)
	}
	defer func() { _ = clients.Close() }()

	gc := collector.NewGarbageCollector(clients.BatchDB, clients.FileDB, clients.File, cfg.DryRun, cfg.Interval, cfg.MaxConcurrency, nil)

	if err := gc.RunLoop(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("garbage collector failed: %w", err)
	}

	logger.Info("Garbage collector shut down gracefully")
	return nil
}

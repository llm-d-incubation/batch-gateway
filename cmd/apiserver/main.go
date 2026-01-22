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

// The entry point for the batch gateway API server.
// It handles server initialization, configuration, and graceful shutdown.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/common"
	"github.com/llm-d-incubation/batch-gateway/internal/apiserver/server"
	"k8s.io/klog/v2"
)

func main() {
	config := common.NewConfig()

	if err := config.Load(); err != nil {
		klog.Fatalf("failed to load config: %v", err)
	}

	// make sure to flush logs before exiting
	defer klog.Flush()

	// graceful shutdown
	parentCtx := context.Background()
	c := make(chan os.Signal, 2)
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	signal.Notify(c, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-c
		cancel()
		<-c
		os.Exit(1)
	}()

	// start server
	logger := klog.FromContext(ctx)

	logger.Info("starting api server")

	server, err := server.New(config)
	if err != nil {
		logger.Error(err, "failed to create api server")
		return
	}
	if err := server.Start(ctx); err != nil {
		logger.Error(err, "failed to start api server")
		return
	}
	logger.Info("api server is terminated")
}

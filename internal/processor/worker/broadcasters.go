package worker

import (
	"context"
	"sync"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/pipeline"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// broadcasterRegistry manages per-model ResultBroadcasters.
// Created lazily, shared across jobs, lives on Processor.
type broadcasterRegistry struct {
	mu           sync.Mutex
	broadcasters map[string]*pipeline.ResultBroadcaster
	resolver     *inference.AsyncGatewayResolver
	ctx          context.Context
	cancel       context.CancelFunc
	logger       logr.Logger
}

func newBroadcasterRegistry(ctx context.Context, resolver *inference.AsyncGatewayResolver, logger logr.Logger) *broadcasterRegistry {
	ctx, cancel := context.WithCancel(ctx)
	return &broadcasterRegistry{
		broadcasters: make(map[string]*pipeline.ResultBroadcaster),
		resolver:     resolver,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
	}
}

func (r *broadcasterRegistry) stop() {
	r.cancel()
}

func (r *broadcasterRegistry) forModels(modelMap *modelMapFile) map[string]*pipeline.ResultBroadcaster {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := make(map[string]*pipeline.ResultBroadcaster)
	for _, modelID := range modelMap.SafeToModel {
		if b, ok := r.broadcasters[modelID]; ok {
			result[modelID] = b
			continue
		}
		client := r.resolver.SharedClientFor(modelID)
		if client == nil {
			continue
		}
		b := pipeline.NewResultBroadcaster(client, r.logger.WithValues("model", modelID))
		go b.Run(r.ctx)
		r.broadcasters[modelID] = b
		result[modelID] = b
	}
	return result
}

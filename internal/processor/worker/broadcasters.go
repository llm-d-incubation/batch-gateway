package worker

import (
	"context"
	"sync"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/pipeline"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// broadcasterRegistry manages per-model ResultBroadcasters.
// Shared across jobs, lives on Processor.
type broadcasterRegistry struct {
	broadcasters map[string]*pipeline.ResultBroadcaster
	wg           sync.WaitGroup
}

func newBroadcasterRegistry(resolver *inference.AsyncGatewayResolver, logger logr.Logger) *broadcasterRegistry {
	models := resolver.Models()
	broadcasters := make(map[string]*pipeline.ResultBroadcaster, len(models))
	for _, modelID := range models {
		client := resolver.SharedClientFor(modelID)
		if client == nil {
			continue
		}
		broadcasters[modelID] = pipeline.NewResultBroadcaster(client, logger.WithValues("model", modelID))
	}
	return &broadcasterRegistry{broadcasters: broadcasters}
}

func (r *broadcasterRegistry) Run(ctx context.Context) {
	for _, b := range r.broadcasters {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			b.Run(ctx)
		}()
	}
}

func (r *broadcasterRegistry) Wait() {
	r.wg.Wait()
}

func (r *broadcasterRegistry) forModels(modelMap *modelMapFile) *pipeline.BroadcasterGroup {
	var result []*pipeline.ResultBroadcaster
	for _, modelID := range modelMap.SafeToModel {
		if b, ok := r.broadcasters[modelID]; ok {
			result = append(result, b)
		}
	}
	return pipeline.NewBroadcasterGroup(result)
}

//go:build bench

package worker

import (
	"context"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/clientset"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/semaphore"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// NewProcessorForTest creates a Processor with semaphores initialised,
// suitable for calling PreProcessJobForTest / ExecuteJobForTest from
// external benchmark packages.
func NewProcessorForTest(
	cfg *config.ProcessorConfig,
	clients *clientset.Clientset,
	logger logr.Logger,
) (*Processor, error) {
	p, err := NewProcessor(cfg, clients, "bench-processor", logger)
	if err != nil {
		return nil, err
	}
	p.tokens, err = semaphore.New(cfg.NumWorkers, nil)
	if err != nil {
		return nil, err
	}
	p.globalSem, err = semaphore.New(cfg.Concurrency.Global, nil)
	if err != nil {
		return nil, err
	}

	if p.inference != nil {
		cc := &cfg.Concurrency
		inferClients := p.inference.Clients()
		p.endpointLimits = make(map[inference.InferenceClient]*endpointLimit, len(inferClients))
		for _, client := range inferClients {
			epLabel := p.inference.ClientLabel(client)
			epSem, semErr := semaphore.NewAdaptive(cc.PerEndpoint, nil)
			if semErr != nil {
				return nil, semErr
			}
			var epAIMD *semaphore.AIMDController
			if cc.AIMD.Enabled {
				epAIMD = semaphore.NewAIMDController(
					semaphore.AIMDConfig{
						MinLimit:         cc.AIMD.Min,
						MaxLimit:         cc.PerEndpoint,
						BackoffFactor:    cc.AIMD.BackoffFactor,
						AdditiveIncrease: cc.AIMD.AdditiveIncrease,
					},
					cc.PerEndpoint,
					func(limit int) { epSem.SetLimit(limit) },
					logr.Discard(),
				)
			}
			p.endpointLimits[client] = &endpointLimit{sem: epSem, aimd: epAIMD, label: epLabel}
		}
	} else {
		p.endpointLimits = make(map[inference.InferenceClient]*endpointLimit)
	}

	return p, nil
}

// PreProcessJobForTest exports preProcessJob for benchmarking.
func (p *Processor) PreProcessJobForTest(ctx, sloCtx, userCancelCtx context.Context, jobInfo *batch_types.JobInfo) error {
	return p.preProcessJob(ctx, sloCtx, userCancelCtx, jobInfo)
}

// ExecuteJobForTest exports the execution pipeline for benchmarking.
func (p *Processor) ExecuteJobForTest(ctx context.Context, jobInfo *batch_types.JobInfo) (*openai.BatchRequestCounts, error) {
	params := &jobExecutionParams{
		updater: p.updater,
		jobInfo: jobInfo,
	}
	return p.executeJobAsync(ctx, ctx, ctx, ctx, params)
}

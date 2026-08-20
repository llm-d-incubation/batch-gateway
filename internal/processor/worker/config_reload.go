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

// Hot reload of the model_gateways / global_inference_gateway section of
// the processor config file.
//
// A ticker polls the config file and compares SHA-256 content hashes. On a
// change, the file is re-parsed and re-validated through the exact same
// code path as startup (LoadFromYAML + Validate + ResolveModelGateways).
// A new immutable routingSnapshot (resolver + endpoint limits + objectives)
// is built and atomically swapped in; ANY error keeps the old routing and
// records a failure metric. Only the sync-dispatch routing plane is
// replaced — DB/queue/file clients, the polling loop and in-flight jobs
// are untouched.
//
// Removed models need no dedicated teardown: queued jobs for an
// unconfigured model already fail with model_not_found written to the job
// error file via the existing preprocessor / dispatcher paths (their
// ClientFor lookup simply starts returning nil after the swap).

package worker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// defaultModelGatewayCloseGracePeriod is how long replaced gateway clients
// outlive a routing swap before their idle connections are closed, giving
// in-flight jobs time to finish against the old endpoints.
const defaultModelGatewayCloseGracePeriod = 30 * time.Second

// startModelGatewayWatcher starts the config-file watcher when reload is
// enabled. No-op for a zero interval or async dispatch mode.
func (p *Processor) startModelGatewayWatcher(ctx context.Context) {
	logger := logr.FromContextOrDiscard(ctx)
	if p.gwReloadPath == "" || p.gwReloadInterval <= 0 {
		return
	}
	if p.asyncInference != nil {
		logger.V(logging.INFO).Info("Model gateway config reload is not supported for async dispatch mode; watcher not started")
		return
	}
	logger.V(logging.INFO).Info("Starting model gateway config watcher",
		"path", p.gwReloadPath,
		"interval", p.gwReloadInterval,
		"closeGracePeriod", defaultModelGatewayCloseGracePeriod,
	)
	go p.modelGatewayWatchLoop(ctx)
}

// modelGatewayWatchLoop polls the config file and applies changes.
func (p *Processor) modelGatewayWatchLoop(ctx context.Context) {
	logger := logr.FromContextOrDiscard(ctx)
	ticker := time.NewTicker(p.gwReloadInterval)
	defer ticker.Stop()

	var lastHash [sha256.Size]byte
	hashValid := false
	if data, err := os.ReadFile(p.gwReloadPath); err == nil {
		lastHash, hashValid = sha256.Sum256(data), true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(p.gwReloadPath)
			if err != nil {
				logger.Error(err, "Model gateway config reload failed: cannot read config file", "path", p.gwReloadPath)
				metrics.RecordModelGatewayReload(metrics.ResultFailed)
				hashValid = false // force a full retry once the file is readable again
				continue
			}
			hash := sha256.Sum256(data)
			if hashValid && hash == lastHash {
				continue
			}
			lastHash, hashValid = hash, true
			logger.V(logging.INFO).Info("Config file change detected, reloading model gateways", "path", p.gwReloadPath)

			newCfg, err := loadProcessorConfig(p.gwReloadPath)
			if err != nil {
				logger.Error(err, "Model gateway config reload failed: invalid config", "path", p.gwReloadPath)
				metrics.RecordModelGatewayReload(metrics.ResultFailed)
				continue
			}
			if err := p.applyModelGatewayConfig(newCfg, logger); err != nil {
				logger.Error(err, "Model gateway config reload failed; keeping current routing", "path", p.gwReloadPath)
				metrics.RecordModelGatewayReload(metrics.ResultFailed)
				continue
			}
			metrics.RecordModelGatewayReload(metrics.ResultSuccess)
			metrics.SetModelGatewayReloadLastSuccess(time.Now())
		}
	}
}

// loadProcessorConfig parses and validates the config file through the same
// path as startup (defaults + LoadFromYAML + Validate).
func loadProcessorConfig(path string) (*config.ProcessorConfig, error) {
	cfg := config.NewConfig()
	if err := cfg.LoadFromYAML(path); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyModelGatewayConfig builds a new routing snapshot from the reloaded
// config and swaps it in. Any error leaves the current routing untouched.
func (p *Processor) applyModelGatewayConfig(newCfg *config.ProcessorConfig, logger logr.Logger) error {
	resolved, err := config.ResolveModelGateways(newCfg)
	if err != nil {
		return fmt.Errorf("resolve model gateways: %w", err)
	}
	if resolved.Async != nil {
		return fmt.Errorf("switching to dispatch_mode %q at runtime is not supported", config.DispatchModeAsync)
	}

	oldSnapshot := p.routing.Load()
	globalObjective, modelObjectives := objectivesFromConfig(newCfg)
	if oldSnapshot != nil {
		if resolvedGatewaysEqual(oldSnapshot.gateways, resolved) &&
			oldSnapshot.globalObjective == globalObjective &&
			maps.Equal(oldSnapshot.modelObjectives, modelObjectives) {
			logger.V(logging.INFO).Info("Gateway routing unchanged after reload; keeping current routing")
			return nil
		}
	}

	var resolver *inference.GatewayResolver
	switch {
	case resolved.Global != nil:
		resolver, err = inference.NewGlobalResolver(*resolved.Global, logger)
	default:
		resolver, err = inference.NewPerModelResolver(resolved.PerModel, logger)
	}
	if err != nil {
		return fmt.Errorf("create inference clients: %w", err)
	}

	var prevLimits map[inference.InferenceClient]*endpointLimit
	if oldSnapshot != nil {
		prevLimits = oldSnapshot.endpointLimits
	}
	limits, err := p.buildEndpointLimits(resolver, prevLimits, logger)
	if err != nil {
		return fmt.Errorf("build endpoint limits: %w", err)
	}

	added, removed, updated := routingDiff(nilOrGateways(oldSnapshot), resolved)
	logger.V(logging.INFO).Info("Model gateway routing reloaded",
		"numModelGateways", len(resolved.PerModel),
		"globalGateway", resolved.Global != nil,
		"addedModels", added,
		"removedModels", removed,
		"updatedModels", updated,
	)

	p.swapRouting(&routingSnapshot{
		resolver:        resolver,
		endpointLimits:  limits,
		globalObjective: globalObjective,
		modelObjectives: modelObjectives,
		gateways:        resolved,
	}, defaultModelGatewayCloseGracePeriod, logger)
	return nil
}

func nilOrGateways(s *routingSnapshot) *config.ResolvedGateways {
	if s == nil {
		return nil
	}
	return s.gateways
}

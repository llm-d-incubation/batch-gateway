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
// A ticker polls the config file and compares SHA-256 fingerprints covering
// the config content plus every referenced external file (API key and TLS
// material), so Secret rotations alone trigger a reload. On a change, the
// file is re-parsed and re-validated through the exact same code path as
// startup (LoadFromYAML + Validate + ResolveModelGateways).
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
	"slices"
	"time"

	"github.com/go-logr/logr"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
	ucom "github.com/llm-d/llm-d-batch-gateway/internal/util/com"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// defaultModelGatewayCloseGracePeriod is how long replaced gateway clients
// outlive a routing swap before their idle connections are closed, giving
// in-flight jobs time to finish against the old endpoints.
const defaultModelGatewayCloseGracePeriod = 30 * time.Second

// startModelGatewayWatcher starts the config-file watcher when reload is
// enabled. No-op for a zero interval or async dispatch mode.
// The given ctx should be the polling loop's context: when the processor
// stops accepting work (guard shutdown / SIGTERM), the routing plane must
// stop mutating. The goroutine is tracked by p.watcherWG so Run can wait
// for it to exit before returning.
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
	p.watcherWG.Add(1)
	go func() {
		defer p.watcherWG.Done()
		p.modelGatewayWatchLoop(ctx)
	}()
}

// modelGatewayWatchLoop polls the config file and applies changes.
//
// Change detection fingerprints the main config file PLUS the contents of
// every external file the gateway section references (api_key_file, the
// file behind api_key_name, and TLS cert/key files): a Secret rotation that
// only rewrites the key file must still reload routing.
//
// Retry semantics: the remembered fingerprint is updated only after a
// successful reload. Any failure (unreadable file, invalid config, resolver
// error, rejected change) leaves the fingerprint untouched, so the next
// poll retries the same failing content instead of silently sticking with
// the last good routing until the config text happens to change again.
func (p *Processor) modelGatewayWatchLoop(ctx context.Context) {
	logger := logr.FromContextOrDiscard(ctx)
	ticker := time.NewTicker(p.gwReloadInterval)
	defer ticker.Stop()

	var lastFP [sha256.Size]byte
	fpValid := false
	if fp, _, ok := p.reloadFingerprint(logger); ok {
		lastFP, fpValid = fp, ok
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fp, newCfg, ok := p.reloadFingerprint(logger)
			if !ok {
				// File unreadable or a referenced file vanished between
				// validation and fingerprinting — transient; keep retrying.
				fpValid = false
				continue
			}
			if fpValid && fp == lastFP {
				continue
			}
			logger.V(logging.INFO).Info("Config change detected, reloading model gateways", "path", p.gwReloadPath)

			if err := p.applyModelGatewayConfig(newCfg, logger); err != nil {
				logger.Error(err, "Model gateway config reload failed; keeping current routing", "path", p.gwReloadPath)
				metrics.RecordModelGatewayReload(metrics.ResultFailed)
				continue // retry the same content on the next tick
			}
			lastFP, fpValid = fp, true
			metrics.RecordModelGatewayReload(metrics.ResultSuccess)
			metrics.SetModelGatewayReloadLastSuccess(time.Now())
		}
	}
}

// reloadFingerprint reads the config file, parses and validates it, and
// returns the parsed config together with a combined content hash of the
// config and every referenced external file. ok=false means reloading
// cannot be attempted right now (unreadable or invalid input); the failure
// metric is recorded here.
func (p *Processor) reloadFingerprint(logger logr.Logger) ([sha256.Size]byte, *config.ProcessorConfig, bool) {
	var fp [sha256.Size]byte
	data, err := os.ReadFile(p.gwReloadPath)
	if err != nil {
		logger.Error(err, "Model gateway config reload failed: cannot read config file", "path", p.gwReloadPath)
		metrics.RecordModelGatewayReload(metrics.ResultFailed)
		return fp, nil, false
	}
	newCfg, err := loadProcessorConfig(p.gwReloadPath)
	if err != nil {
		logger.Error(err, "Model gateway config reload failed: invalid config", "path", p.gwReloadPath)
		metrics.RecordModelGatewayReload(metrics.ResultFailed)
		return fp, nil, false
	}
	fp, err = gatewayFingerprint(data, newCfg)
	if err != nil {
		logger.Error(err, "Model gateway config reload failed: cannot read referenced files", "path", p.gwReloadPath)
		metrics.RecordModelGatewayReload(metrics.ResultFailed)
		return fp, nil, false
	}
	return fp, newCfg, true
}

// gatewayFingerprint hashes the config file content together with the
// contents of every external file the gateway section references, so key or
// certificate rotations alone trigger a reload. Referenced files are
// re-read from disk here (they are read again during resolve; worst case a
// rotation racing this read just causes one extra reload next poll).
// A referenced file that does not exist hashes as absent rather than
// failing — resolveGatewayAPIKey treats a missing named secret the same way.
func gatewayFingerprint(configData []byte, cfg *config.ProcessorConfig) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	h := sha256.New()
	h.Write(configData)
	for _, f := range referencedGatewayFiles(cfg) {
		h.Write([]byte{0})
		h.Write([]byte(f))
		h.Write([]byte{0})
		data, err := os.ReadFile(f)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return zero, fmt.Errorf("read referenced file %q: %w", f, err)
		}
		h.Write(data)
	}
	var fp [sha256.Size]byte
	copy(fp[:], h.Sum(nil))
	return fp, nil
}

// referencedGatewayFiles lists the external files whose contents feed into
// gateway client construction: explicit api_key_file paths, the mounted
// secret behind api_key_name (and the global gateway's fallback
// inference-api-key when neither key field is set), and TLS cert/key files.
func referencedGatewayFiles(cfg *config.ProcessorConfig) []string {
	var files []string
	add := func(gw *config.ModelGatewayConfig) {
		if gw == nil {
			return
		}
		if gw.APIKeyFile != "" {
			files = append(files, gw.APIKeyFile)
		}
		if gw.APIKeyName != "" {
			files = append(files, ucom.SecretFilePath(gw.APIKeyName))
		}
		for _, f := range []string{gw.TLSCACertFile, gw.TLSClientCertFile, gw.TLSClientKeyFile} {
			if f != "" {
				files = append(files, f)
			}
		}
	}
	add(cfg.GlobalInferenceGateway)
	if g := cfg.GlobalInferenceGateway; g != nil && g.APIKeyFile == "" && g.APIKeyName == "" {
		// ResolveModelGateways falls back to the mounted inference-api-key
		// for the global gateway; its rotation must reload routing too.
		files = append(files, ucom.SecretFilePath(ucom.SecretKeyInferenceAPI))
	}
	for _, model := range slices.Sorted(maps.Keys(cfg.ModelGateways)) {
		gw := cfg.ModelGateways[model]
		add(&gw)
	}
	slices.Sort(files)
	return slices.Compact(files)
}

// newSyncResolver builds the sync routing resolver for a freshly resolved
// gateway set — the same construction as startup. Declared as a var so
// tests can substitute a resolver over closeable fake clients when asserting
// failure-path resource cleanup.
var newSyncResolver = func(resolved *config.ResolvedGateways, logger logr.Logger) (*inference.GatewayResolver, error) {
	if resolved.Global != nil {
		return inference.NewGlobalResolver(*resolved.Global, logger)
	}
	return inference.NewPerModelResolver(resolved.PerModel, logger)
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
//
// route_key_method is explicitly NOT reloadable: the resolver is keyed on
// it and request construction (preprocessor, plan source, dispatcher) uses
// it too. Reloading it would silently split the two sides of the lookup, so
// a change is rejected and surfaced as a reload failure — restart required.
func (p *Processor) applyModelGatewayConfig(newCfg *config.ProcessorConfig, logger logr.Logger) (retErr error) {
	if newCfg.RouteKeyMethod != p.cfg.RouteKeyMethod {
		return fmt.Errorf("route_key_method changed from %q to %q: not hot-reloadable, restart the processor to apply",
			p.cfg.RouteKeyMethod, newCfg.RouteKeyMethod)
	}

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

	resolver, err := newSyncResolver(resolved, logger)
	if err != nil {
		return fmt.Errorf("create inference clients: %w", err)
	}
	// A resolver that never gets swapped in must not leak its transports.
	defer func() {
		if retErr != nil {
			_ = resolver.Close()
		}
	}()

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

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

// Hot-reloadable routing plane for model gateway resolution.
//
// The sync-dispatch routing plane (resolver + per-endpoint concurrency
// limits + inference-objective lookup) is captured in an immutable
// routingSnapshot held behind an atomic pointer on the Processor. Every
// job takes the snapshot at job start, so an in-flight job never observes
// a mid-flight routing change; jobs started after a swap resolve against
// the new config. See config_reload.go for the file watcher that swaps the
// snapshot.

package worker

import (
	"crypto/sha256"
	"maps"
	"slices"
	"time"

	"github.com/go-logr/logr"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/logging"
	"github.com/llm-d/llm-d-batch-gateway/internal/util/semaphore"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// routingSnapshot is the immutable, atomically swapped routing plane.
// All fields are read-only after construction — safe for concurrent reads.
type routingSnapshot struct {
	resolver       *inference.GatewayResolver
	endpointLimits map[inference.InferenceClient]*endpointLimit

	// inference-objective lookup mirrors ProcessorConfig.InferenceObjectiveFor
	// for the gateway set captured by this snapshot.
	globalObjective string
	modelObjectives map[string]string

	// gateways is the resolved gateway baseline this snapshot was built
	// from. Used by the reload path to diff and to skip no-op swaps.
	// Nil for bootstrap snapshots synthesized before Run() initializes
	// routing (e.g. unit tests that call job steps without Run).
	gateways *config.ResolvedGateways

	// referencedFilesHash digests the path set and contents of every
	// external file the gateway config references (api key and TLS
	// material). GatewayClientConfig carries TLS cert/key paths only, not
	// contents, so gateway-set equality cannot see a certificate rotated in
	// place — without this digest a content-only rotation would be
	// misjudged as a no-op and the applied fingerprint would pin the stale
	// TLS material forever. Nil when it could not be computed (the startup
	// baseline is best-effort); nil is "unknown", never "unchanged" — the
	// next reload forces one resolver rebuild and anchors the computed
	// digest, so an unreadable-at-startup file cannot permanently stick
	// change detection in the no-op state.
	referencedFilesHash *[sha256.Size]byte
}

// objectiveFor returns the inference objective for the given lookup key
// (the route key used for gateway resolution), or "" when none is
// configured — mirroring ProcessorConfig.InferenceObjectiveFor.
func (s *routingSnapshot) objectiveFor(modelID string) string {
	if s == nil || s.resolver == nil {
		return ""
	}
	if s.resolver.IsGlobal() {
		return s.globalObjective
	}
	return s.modelObjectives[modelID]
}

// objectivesFromConfig mirrors ProcessorConfig.InferenceObjectiveFor as a
// snapshot-friendly (global, per-model) pair.
func objectivesFromConfig(cfg *config.ProcessorConfig) (string, map[string]string) {
	if cfg.GlobalInferenceGateway != nil {
		return cfg.GlobalInferenceGateway.InferenceObjective, nil
	}
	modelObjectives := make(map[string]string, len(cfg.ModelGateways))
	for model, gw := range cfg.ModelGateways {
		modelObjectives[model] = gw.InferenceObjective
	}
	return "", modelObjectives
}

// routingState returns the current routing snapshot. Before Run() stores
// one (unit tests, recovery pre-Run steps), it falls back to a bootstrap
// snapshot synthesized from the startup resolver and static config.
func (p *Processor) routingState() *routingSnapshot {
	if s := p.routing.Load(); s != nil {
		return s
	}
	globalObjective, modelObjectives := objectivesFromConfig(p.cfg)
	return &routingSnapshot{
		resolver:        p.inference,
		endpointLimits:  p.endpointLimits,
		globalObjective: globalObjective,
		modelObjectives: modelObjectives,
	}
}

// buildEndpointLimits creates a per-endpoint concurrency limiter for every
// unique client in the resolver. Unchanged endpoints (matched by URL label)
// reuse their previous limiter so learned AIMD state survives a reload.
func (p *Processor) buildEndpointLimits(
	resolver *inference.GatewayResolver,
	prev map[inference.InferenceClient]*endpointLimit,
	logger logr.Logger,
) (map[inference.InferenceClient]*endpointLimit, error) {
	cc := &p.cfg.Concurrency

	prevByLabel := make(map[string]*endpointLimit, len(prev))
	for _, ep := range prev {
		prevByLabel[ep.label] = ep
	}

	clients := resolver.Clients()
	limits := make(map[inference.InferenceClient]*endpointLimit, len(clients))
	for _, client := range clients {
		epLabel := resolver.ClientLabel(client)
		if ep, ok := prevByLabel[epLabel]; ok {
			limits[client] = ep
			continue
		}
		epSem, err := semaphore.NewAdaptive(cc.PerEndpoint, p.makeGuard("endpoint-concurrency"))
		if err != nil {
			return nil, err
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
				logger.WithValues("endpoint", epLabel),
			)
			metrics.SetAIMDConcurrencyLimit(epLabel, float64(cc.PerEndpoint))
		}
		limits[client] = &endpointLimit{sem: epSem, aimd: epAIMD, label: epLabel}
	}
	return limits, nil
}

// swapRouting atomically replaces the routing plane and schedules the old
// resolver's clients for closure after a grace period, letting in-flight
// jobs drain. (HTTPClient.Close closes idle connections only; it never
// interrupts in-flight requests.)
//
// Ownership boundary: the startup resolver (p.inference, installed by
// initConcurrencyControls) belongs to the Clientset, which closes it in
// procClients.Close() on shutdown — a swap must never schedule its closure,
// or the first reload would close it twice and out of Clientset ownership.
// Resolvers created by config reloads belong to the Processor: the replaced
// one is closed here after the grace period, and the still-installed one is
// closed by Stop (closeReloadedRouting).
func (p *Processor) swapRouting(newSnapshot *routingSnapshot, gracePeriod time.Duration, logger logr.Logger) {
	old := p.routing.Swap(newSnapshot)
	if old == nil || old.resolver == nil || old.resolver == p.inference {
		return
	}
	time.AfterFunc(gracePeriod, func() {
		_ = old.resolver.Close()
		logger.V(logging.INFO).Info("Closed replaced model gateway clients after grace period",
			"gracePeriod", gracePeriod)
	})
}

// resolvedGatewaysEqual reports whether two resolved gateway sets produce
// an identical routing plane. Async dispatch mode is never a valid reload
// target, so Async equality reduces to both-nil.
func resolvedGatewaysEqual(a, b *config.ResolvedGateways) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.Global == nil) != (b.Global == nil) {
		return false
	}
	if a.Global != nil && *a.Global != *b.Global {
		return false
	}
	return maps.Equal(a.PerModel, b.PerModel)
}

// routingDiff computes the per-model routing changes between two resolved
// gateway sets. Suitable for logging only.
func routingDiff(old, new *config.ResolvedGateways) (added, removed, updated []string) {
	for model, gw := range new.PerModel {
		if old == nil {
			added = append(added, model)
			continue
		}
		if oldGw, ok := old.PerModel[model]; ok {
			if oldGw != gw {
				updated = append(updated, model)
			}
		} else {
			added = append(added, model)
		}
	}
	if old != nil {
		for model := range old.PerModel {
			if _, ok := new.PerModel[model]; !ok {
				removed = append(removed, model)
			}
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(updated)
	return added, removed, updated
}

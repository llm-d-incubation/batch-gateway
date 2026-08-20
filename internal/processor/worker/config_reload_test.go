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

package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

// writeGatewayConfig writes a valid processor config whose sync gateway
// section contains exactly the given model -> URL mappings.
func writeGatewayConfig(t *testing.T, path string, models map[string]string) {
	t.Helper()
	content := ""
	for model, url := range models {
		content += fmt.Sprintf(`  %s:
    url: %q
    request_timeout: 30s
    max_retries: 1
    initial_backoff: 1s
    max_backoff: 5s
`, model, url)
	}
	if err := os.WriteFile(path, []byte("model_gateways:\n"+content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
}

// newReloadTestProcessor builds a Processor seeded with the same config the
// temp file holds, as if it had just started up from that file: the routing
// snapshot is published the way initConcurrencyControls does in Run().
func newReloadTestProcessor(t *testing.T, cfgPath string, interval time.Duration) *Processor {
	t.Helper()

	cfg, err := loadProcessorConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadProcessorConfig: %v", err)
	}
	p, err := NewProcessor(cfg, validProcessorClients(t), "test-processor", testLogger(t),
		WithModelGatewayConfigReload(cfgPath, interval))
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	p.makeGuard = func(string) func() { return func() {} }

	resolved, err := config.ResolveModelGateways(cfg)
	if err != nil {
		t.Fatalf("ResolveModelGateways: %v", err)
	}
	resolver, err := inference.NewPerModelResolver(resolved.PerModel, testLogger(t))
	if err != nil {
		t.Fatalf("NewPerModelResolver: %v", err)
	}
	limits, err := p.buildEndpointLimits(resolver, nil, testLogger(t))
	if err != nil {
		t.Fatalf("buildEndpointLimits: %v", err)
	}
	globalObjective, modelObjectives := objectivesFromConfig(cfg)
	p.routing.Store(&routingSnapshot{
		resolver:        resolver,
		endpointLimits:  limits,
		globalObjective: globalObjective,
		modelObjectives: modelObjectives,
		gateways:        resolved,
	})
	return p
}

func TestApplyModelGatewayConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 0)

	apply := func(t *testing.T, models map[string]string) error {
		newPath := filepath.Join(t.TempDir(), "config.yaml")
		writeGatewayConfig(t, newPath, models)
		newCfg, err := loadProcessorConfig(newPath)
		if err != nil {
			t.Fatalf("loadProcessorConfig: %v", err)
		}
		return p.applyModelGatewayConfig(newCfg, testLogger(t))
	}

	// First swap: add m2.
	if err := apply(t, map[string]string{"m1": "http://a:8000", "m2": "http://b:8000"}); err != nil {
		t.Fatalf("apply (add m2): %v", err)
	}
	rs := p.routingState()
	if rs.resolver.ClientFor("m2") == nil {
		t.Fatal("added model m2 must resolve after the swap")
	}
	if rs.resolver.ClientFor("m1") == nil {
		t.Fatal("kept model m1 must still resolve after the swap")
	}
	first := rs

	// No-op swap: same gateway set must not replace the snapshot.
	if err := apply(t, map[string]string{"m1": "http://a:8000", "m2": "http://b:8000"}); err != nil {
		t.Fatalf("apply (no-op): %v", err)
	}
	if got := p.routing.Load(); got != first {
		t.Fatalf("no-op reload replaced the routing snapshot (%p -> %p)", first, got)
	}

	// Second swap: remove m1. The removed model stops resolving — queued
	// jobs for it surface model_not_found via the existing preprocessor /
	// dispatcher ClientFor==nil paths (no new error plumbing needed).
	if err := apply(t, map[string]string{"m2": "http://b:8000"}); err != nil {
		t.Fatalf("apply (remove m1): %v", err)
	}
	rs = p.routingState()
	if rs.resolver.ClientFor("m1") != nil {
		t.Fatal("removed model m1 must stop resolving after the swap")
	}
	if rs.resolver.ClientFor("m2") == nil {
		t.Fatal("kept model m2 must still resolve after the swap")
	}

	// Endpoint limiter for the unchanged endpoint carries over (AIMD state).
	oldLimit := first.endpointLimits[first.resolver.ClientFor("m2")]
	if got := rs.endpointLimits[rs.resolver.ClientFor("m2")]; got != oldLimit {
		t.Fatal("limiter for unchanged endpoint did not survive the swap")
	}
}

func TestApplyModelGatewayConfigBadConfigKeepsState(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 0)

	before := p.routingState()

	// api_key_file points at a nonexistent path: gateway validate/resolve
	// fails, the old routing must stay in place.
	badPath := filepath.Join(t.TempDir(), "config.yaml")
	content := `model_gateways:
  m1:
    url: "http://a:8000"
    api_key_file: "/nonexistent/secret"
    request_timeout: 30s
    max_retries: 1
    initial_backoff: 1s
    max_backoff: 5s
`
	if err := os.WriteFile(badPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	newCfg, err := loadProcessorConfig(badPath)
	if err == nil {
		if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err == nil {
			t.Fatal("apply with unreadable api_key_file must fail")
		}
	}
	if got := p.routingState(); got.resolver != before.resolver || got.endpointLimits == nil && before.endpointLimits != nil {
		t.Fatal("failed reload must keep the old routing state")
	}
	if got := p.routingState(); got.resolver.ClientFor("m1") == nil {
		t.Fatal("failed reload removed model m1 from routing")
	}
}

// TestModelGatewayWatchLoop exercises the polling watcher end to end:
// change detection, atomic swap (models flipped), bad config tolerated,
// missing file tolerated, and graceful shutdown via context cancel.
func TestModelGatewayWatchLoop(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	defer cancel()
	go p.modelGatewayWatchLoop(ctx)

	// Let the watcher a few poll cycles to anchor its initial content hash
	// before the first edit below.
	time.Sleep(100 * time.Millisecond)

	waitFor := func(t *testing.T, desc string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for: %s", desc)
	}

	// Flip the model set: remove m1, add m2.
	writeGatewayConfig(t, cfgPath, map[string]string{"m2": "http://b:8000"})
	waitFor(t, "reloaded routing with m2", func() bool {
		rs := p.routing.Load()
		return rs != nil && rs.resolver.ClientFor("m2") != nil && rs.resolver.ClientFor("m1") == nil
	})

	// Corrupt the file: routing stays on the last good snapshot.
	good := p.routingState()
	if err := os.WriteFile(cfgPath, []byte("model_gateways: [not, a, map\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // a few poll cycles
	if got := p.routingState().resolver; got != good.resolver {
		t.Fatal("corrupt config must not replace routing")
	}

	// Delete the file entirely: tolerated, routing preserved.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := p.routingState().resolver; got != good.resolver {
		t.Fatal("missing config file must not replace routing")
	}

	// Restore a syntactically valid but incomplete config (no gateways):
	// fails validation, still no swap.
	writeGatewayConfig(t, cfgPath, map[string]string{})
	if err := os.WriteFile(cfgPath, []byte("num_workers: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := p.routingState().resolver; got != good.resolver {
		t.Fatal("gateway-less config must not replace routing")
	}

	// Recover with an updated endpoint for m2.
	writeGatewayConfig(t, cfgPath, map[string]string{"m2": "http://b2:8000"})
	waitFor(t, "recovery to m2 with new endpoint", func() bool {
		rs := p.routing.Load()
		return rs != nil && rs.resolver.ClientLabel(rs.resolver.ClientFor("m2")) == "http://b2:8000"
	})

	cancel() // watcher goroutine exits; nothing to assert — just no leaks/hang.
}

func TestStartModelGatewayWatcherDisabledByDefault(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 0) // zero interval = disabled

	p.startModelGatewayWatcher(context.Background())
	// No goroutine, no panic, nothing to assert further: a zero-interval
	// ticker would panic if the watcher had started.
	time.Sleep(50 * time.Millisecond)
}

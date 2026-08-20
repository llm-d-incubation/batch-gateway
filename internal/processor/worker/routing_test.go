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
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

func mustResolver(t *testing.T, urls map[string]string) *inference.GatewayResolver {
	t.Helper()
	configs := make(map[string]inference.GatewayClientConfig, len(urls))
	for model, url := range urls {
		configs[model] = inference.GatewayClientConfig{URL: url}
	}
	resolver, err := inference.NewPerModelResolver(configs, testLogger(t))
	if err != nil {
		t.Fatalf("NewPerModelResolver: %v", err)
	}
	return resolver
}

func TestRoutingSnapshotObjectiveFor(t *testing.T) {
	ptrCfg := func(gw config.ModelGatewayConfig) *config.ProcessorConfig {
		cfg := config.NewConfig()
		cfg.GlobalInferenceGateway = &gw
		return cfg
	}

	perModel := config.NewConfig()
	perModel.ModelGateways = map[string]config.ModelGatewayConfig{
		"m1": {URL: "http://a:8000", InferenceObjective: "obj-a"},
		"m2": {URL: "http://b:8000"},
	}

	globalObjective, modelObjectives := objectivesFromConfig(perModel)
	perModelSnapshot := &routingSnapshot{
		resolver:        mustResolver(t, map[string]string{"m1": "http://a:8000", "m2": "http://b:8000"}),
		globalObjective: globalObjective,
		modelObjectives: modelObjectives,
	}

	globalCfg := ptrCfg(config.ModelGatewayConfig{URL: "http://g:8000", InferenceObjective: "obj-global"})
	globalResolver, err := inference.NewGlobalResolver(inference.GatewayClientConfig{URL: "http://g:8000"}, testLogger(t))
	if err != nil {
		t.Fatalf("NewGlobalResolver: %v", err)
	}
	globalObjective, gModelObjectives := objectivesFromConfig(globalCfg)
	globalSnapshot := &routingSnapshot{
		resolver:        globalResolver,
		globalObjective: globalObjective,
		modelObjectives: gModelObjectives,
	}

	tests := []struct {
		name     string
		snapshot *routingSnapshot
		modelID  string
		want     string
	}{
		{name: "per-model hit", snapshot: perModelSnapshot, modelID: "m1", want: "obj-a"},
		{name: "per-model no objective", snapshot: perModelSnapshot, modelID: "m2", want: ""},
		{name: "per-model unknown model", snapshot: perModelSnapshot, modelID: "nope", want: ""},
		{name: "global uses its objective for any model", snapshot: globalSnapshot, modelID: "anything", want: "obj-global"},
		{name: "nil snapshot", snapshot: nil, modelID: "m1", want: ""},
		{name: "nil resolver", snapshot: &routingSnapshot{}, modelID: "m1", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.snapshot.objectiveFor(tt.modelID); got != tt.want {
				t.Fatalf("objectiveFor(%q) = %q, want %q", tt.modelID, got, tt.want)
			}
		})
	}
}

func TestResolvedGatewaysEqual(t *testing.T) {
	gw := func(url string) inference.GatewayClientConfig {
		return inference.GatewayClientConfig{URL: url, Timeout: 1, MaxRetries: 1}
	}

	tests := []struct {
		name string
		a, b *config.ResolvedGateways
		want bool
	}{
		{name: "both nil", a: nil, b: nil, want: true},
		{name: "one nil", a: &config.ResolvedGateways{}, b: nil, want: false},
		{
			name: "same per-model set",
			a:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://a:8000")}},
			b:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://a:8000")}},
			want: true,
		},
		{
			name: "different url for same model",
			a:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://a:8000")}},
			b:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://b:8000")}},
			want: false,
		},
		{
			name: "additional model",
			a:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://a:8000")}},
			b:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://a:8000"), "m2": gw("http://a:8000")}},
			want: false,
		},
		{
			name: "global vs per-model",
			a:    &config.ResolvedGateways{Global: &[]inference.GatewayClientConfig{gw("http://a:8000")}[0]},
			b:    &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{"m1": gw("http://a:8000")}},
			want: false,
		},
		{
			name: "same global",
			a:    &config.ResolvedGateways{Global: &[]inference.GatewayClientConfig{gw("http://a:8000")}[0]},
			b:    &config.ResolvedGateways{Global: &[]inference.GatewayClientConfig{gw("http://a:8000")}[0]},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolvedGatewaysEqual(tt.a, tt.b); got != tt.want {
				t.Fatalf("resolvedGatewaysEqual = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoutingDiff(t *testing.T) {
	gw := func(url string) inference.GatewayClientConfig {
		return inference.GatewayClientConfig{URL: url}
	}
	oldSet := &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{
		"keep":   gw("http://a:8000"),
		"gone":   gw("http://b:8000"),
		"change": gw("http://c:8000"),
	}}
	newSet := &config.ResolvedGateways{PerModel: map[string]inference.GatewayClientConfig{
		"keep":   gw("http://a:8000"),
		"change": gw("http://c2:8000"),
		"fresh":  gw("http://d:8000"),
	}}

	added, removed, updated := routingDiff(oldSet, newSet)
	if len(added) != 1 || added[0] != "fresh" {
		t.Fatalf("added = %v, want [fresh]", added)
	}
	if len(removed) != 1 || removed[0] != "gone" {
		t.Fatalf("removed = %v, want [gone]", removed)
	}
	if len(updated) != 1 || updated[0] != "change" {
		t.Fatalf("updated = %v, want [change]", updated)
	}

	added, removed, updated = routingDiff(nil, newSet)
	if len(added) != 3 {
		t.Fatalf("nil baseline: added = %v, want all 3 models", added)
	}
	if len(removed) != 0 || len(updated) != 0 {
		t.Fatalf("nil baseline: removed=%v updated=%v, want empty", removed, updated)
	}
}

func TestBuildEndpointLimitsCarryover(t *testing.T) {
	cfg := config.NewConfig()
	p, err := NewProcessor(cfg, validProcessorClients(t), "test-processor", testLogger(t))
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	p.makeGuard = func(string) func() { return func() {} }

	oldResolver := mustResolver(t, map[string]string{
		"m1": "http://a:8000",
		"m2": "http://b:8000",
	})
	limits, err := p.buildEndpointLimits(oldResolver, nil, testLogger(t))
	if err != nil {
		t.Fatalf("buildEndpointLimits: %v", err)
	}
	if len(limits) != 2 {
		t.Fatalf("got %d limits, want 2", len(limits))
	}
	keptClient := oldResolver.ClientFor("m1")
	keptLimit := limits[keptClient]
	if keptLimit == nil {
		t.Fatal("limit for m1 missing")
	}

	// Reload: m1 unchanged (same URL), m2 moved, m3 added.
	newResolver := mustResolver(t, map[string]string{
		"m1": "http://a:8000",
		"m2": "http://b2:8000",
		"m3": "http://c:8000",
	})
	newLimits, err := p.buildEndpointLimits(newResolver, limits, testLogger(t))
	if err != nil {
		t.Fatalf("buildEndpointLimits (reload): %v", err)
	}
	if len(newLimits) != 3 {
		t.Fatalf("got %d limits, want 3", len(newLimits))
	}
	if got := newLimits[newResolver.ClientFor("m1")]; got != keptLimit {
		t.Fatalf("unchanged endpoint did not keep its limiter (learned AIMD state would be lost)")
	}
	if got := newLimits[newResolver.ClientFor("m2")]; got == limits[oldResolver.ClientFor("m2")] {
		t.Fatal("moved endpoint m2 should get a fresh limiter")
	}
	if newLimits[newResolver.ClientFor("m3")] == nil {
		t.Fatal("new endpoint m3 must get a limiter")
	}
}

func TestRoutingStateBootstrapFallback(t *testing.T) {
	resolver := mustResolver(t, map[string]string{"m1": "http://a:8000"})
	cfg := config.NewConfig()
	cfg.ModelGateways = map[string]config.ModelGatewayConfig{
		"m1": {URL: "http://a:8000", InferenceObjective: "obj-a"},
	}
	cs := validProcessorClients(t)
	cs.Inference = resolver

	p, err := NewProcessor(cfg, cs, "test-processor", testLogger(t))
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	// Without Run(), routingState falls back to the startup resolver + cfg.
	rs := p.routingState()
	if rs == nil || rs.resolver == nil {
		t.Fatal("routingState() = nil, want bootstrap snapshot")
	}
	if rs.resolver.ClientFor("m1") == nil {
		t.Fatal("bootstrap snapshot must route to the startup resolver")
	}
	if got := rs.objectiveFor("m1"); got != "obj-a" {
		t.Fatalf("objectiveFor(m1) = %q, want %q", got, "obj-a")
	}
}

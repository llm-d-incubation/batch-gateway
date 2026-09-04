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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
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
	snapshot := &routingSnapshot{
		resolver:        resolver,
		endpointLimits:  limits,
		globalObjective: globalObjective,
		modelObjectives: modelObjectives,
		gateways:        resolved,
	}
	if filesHash, err := referencedFilesHash(cfg); err == nil {
		snapshot.referencedFilesHash = &filesHash
	}
	p.routing.Store(snapshot)
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

	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	p.startModelGatewayWatcher(ctx)
	// No goroutine, no panic, nothing to assert further: a zero-interval
	// ticker would panic if the watcher had started.
	time.Sleep(50 * time.Millisecond)
	cancel()
	p.watcherWG.Wait()
}

// writeGatewayConfigWithKeyFile writes a valid per-model config whose single
// gateway reads its API key from the given file.
func writeGatewayConfigWithKeyFile(t *testing.T, path, keyFile string) {
	t.Helper()
	content := fmt.Sprintf(`model_gateways:
  m1:
    url: "http://a:8000"
    api_key_file: %q
    request_timeout: 30s
    max_retries: 1
    initial_backoff: 1s
    max_backoff: 5s
`, keyFile)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
}

// TestApplyModelGatewayConfigRejectsRouteKeyMethodChange: route_key_method is
// static (the resolver key and request-side route key must come from the same
// method); a reload attempting to change it must fail loudly, not silently
// split the two sides of the lookup.
func TestApplyModelGatewayConfigRejectsRouteKeyMethodChange(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 0)
	before := p.routing.Load()

	cfgText, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(newPath, append([]byte("route_key_method: tenant\n"), cfgText...), 0o600); err != nil {
		t.Fatal(err)
	}
	newCfg, err := loadProcessorConfig(newPath)
	if err != nil {
		t.Fatalf("loadProcessorConfig: %v", err)
	}
	if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err == nil {
		t.Fatal("route_key_method change must be rejected")
	}
	if got := p.routing.Load(); got != before {
		t.Fatal("rejected route_key_method change must not swap routing")
	}
}

// TestModelGatewayWatchLoopRouteKeyMethodChangeRejected: a route_key_method
// edit is not hot-reloadable. The watcher must keep the current routing and
// record a failure on every tick (the applied fingerprint is only updated
// after a successful reload, so the same rejected content is retried).
func TestModelGatewayWatchLoopRouteKeyMethodChangeRejected(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 10*time.Millisecond)
	initial := p.routing.Load()

	failuresBefore := reloadFailureCount(t)

	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	p.watcherWG.Add(1)
	go func() {
		defer p.watcherWG.Done()
		p.modelGatewayWatchLoop(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // anchor initial fingerprint

	// Flip only route_key_method; the gateway set itself is unchanged.
	text, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append([]byte("route_key_method: tenant\n"), text...), 0o600); err != nil {
		t.Fatal(err)
	}

	// Two failures prove the same rejected content is retried, not skipped.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if reloadFailureCount(t) >= failuresBefore+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := reloadFailureCount(t); got < failuresBefore+2 {
		t.Fatalf("reload failure count = %v, want at least %v (rejected reloads must retry the same content)", got, failuresBefore+2)
	}
	if got := p.routing.Load(); got != initial {
		t.Fatal("rejected route_key_method change must keep current routing")
	}

	cancel()
	p.watcherWG.Wait()
}

// TestGatewayFingerprintCoversReferencedFiles: the reload fingerprint must
// change when an api key file's content rotates (identical config text), and
// when the config points at a different key file path — the referenced path
// set itself is hashed too.
func TestGatewayFingerprintCoversReferencedFiles(t *testing.T) {
	dir := t.TempDir()
	keyA := filepath.Join(dir, "a.key")
	keyB := filepath.Join(dir, "b.key")
	for _, f := range []string{keyA, keyB} {
		if err := os.WriteFile(f, []byte("key-one\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")

	fingerprint := func(t *testing.T) [32]byte {
		t.Helper()
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := loadProcessorConfig(cfgPath)
		if err != nil {
			t.Fatalf("loadProcessorConfig: %v", err)
		}
		fp, err := gatewayFingerprint(data, cfg)
		if err != nil {
			t.Fatalf("gatewayFingerprint: %v", err)
		}
		return fp
	}

	writeGatewayConfigWithKeyFile(t, cfgPath, keyA)
	base := fingerprint(t)

	// Same config text, rotated key content: must be detected.
	if err := os.WriteFile(keyA, []byte("key-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp := fingerprint(t); fp == base {
		t.Fatal("fingerprint must change when the api key file content rotates")
	}
	if err := os.WriteFile(keyA, []byte("key-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp := fingerprint(t); fp != base {
		t.Fatal("fingerprint must return to its base value when the key content is restored")
	}

	// Different key file path with identical content: the path set is hashed.
	writeGatewayConfigWithKeyFile(t, cfgPath, keyB)
	if fp := fingerprint(t); fp == base {
		t.Fatal("fingerprint must change when the referenced key file path changes")
	}
}

// writeGlobalGatewayConfigWithKeyFile writes a valid global-gateway config
// whose single gateway reads its API key from the given file.
func writeGlobalGatewayConfigWithKeyFile(t *testing.T, path, keyFile string) {
	t.Helper()
	content := fmt.Sprintf(`global_inference_gateway:
  url: "http://global:8000"
  api_key_file: %q
  request_timeout: 30s
  max_retries: 1
  initial_backoff: 1s
  max_backoff: 5s
`, keyFile)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
}

// TestGatewayFingerprintCoversGlobalFallbackSecret: ResolveModelGateways
// falls back to the mounted inference-api-key for the global gateway
// whenever the explicit key source resolves to an empty string (e.g. an
// api_key_file whose content is empty). The config layer hashes paths, not
// contents, so it cannot tell whether the fallback will actually be used —
// the fallback secret is therefore always part of the referenced file set
// for a global gateway, and rotating it must move the fingerprint even when
// a non-empty api_key_file makes the fallback unused at resolve time (one
// extra hashed file is acceptable conservative behavior). Per-model
// gateways have no such fallback and must not pull the secret into their
// file set.
func TestGatewayFingerprintCoversGlobalFallbackSecret(t *testing.T) {
	dir := t.TempDir()
	fallback := filepath.Join(dir, "inference-api-key")
	if err := os.WriteFile(fallback, []byte("fallback-one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := fallbackSecretPath
	fallbackSecretPath = fallback
	defer func() { fallbackSecretPath = orig }()

	keyFile := filepath.Join(dir, "api.key")
	if err := os.WriteFile(keyFile, []byte("explicit-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	writeGlobalGatewayConfigWithKeyFile(t, cfgPath, keyFile)

	fingerprint := func(t *testing.T) [32]byte {
		t.Helper()
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := loadProcessorConfig(cfgPath)
		if err != nil {
			t.Fatalf("loadProcessorConfig: %v", err)
		}
		fp, err := gatewayFingerprint(data, cfg)
		if err != nil {
			t.Fatalf("gatewayFingerprint: %v", err)
		}
		return fp
	}

	base := fingerprint(t)

	// Rotate only the fallback secret: the config text and the explicit key
	// are identical, but the fingerprint must still change — in the real
	// fallback scenario this is what reloads routing on secret rotation.
	if err := os.WriteFile(fallback, []byte("fallback-two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp := fingerprint(t); fp == base {
		t.Fatal("fingerprint must change when the global fallback secret rotates")
	}

	// Per-model gateways never use the fallback: their referenced file set
	// must exclude the fallback secret path.
	writeGatewayConfigWithKeyFile(t, cfgPath, keyFile)
	perModelCfg, err := loadProcessorConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadProcessorConfig: %v", err)
	}
	files := referencedGatewayFiles(perModelCfg)
	if slices.Contains(files, fallback) {
		t.Fatalf("per-model gateway must not reference the global fallback secret, got %v", files)
	}
}

// TestApplyModelGatewayConfigClosesResolverOnFailure: when a reload fails
// after the new resolver was created (here: endpoint limiter construction),
// the resolver must be closed, otherwise every failed reload leaks HTTP
// transports for the rest of the process lifetime.
func TestApplyModelGatewayConfigClosesResolverOnFailure(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	writeGatewayConfig(t, cfgPath, map[string]string{"m1": "http://a:8000"})
	p := newReloadTestProcessor(t, cfgPath, 0)
	before := p.routing.Load()

	fake := &closableFakeClient{}
	orig := newSyncResolver
	newSyncResolver = func(*config.ResolvedGateways, logr.Logger) (*inference.GatewayResolver, error) {
		return inference.NewSingleClientResolver(fake), nil
	}
	defer func() { newSyncResolver = orig }()

	// PerEndpoint=0 makes the new (changed) endpoint's limiter construction
	// fail after the resolver was built.
	p.cfg.Concurrency.PerEndpoint = 0

	newPath := filepath.Join(t.TempDir(), "config.yaml")
	writeGatewayConfig(t, newPath, map[string]string{"m1": "http://b:9000"})
	newCfg, err := loadProcessorConfig(newPath)
	if err != nil {
		t.Fatalf("loadProcessorConfig: %v", err)
	}
	if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err == nil {
		t.Fatal("apply with unbuildable endpoint limits must fail")
	}
	if !fake.closed.Load() {
		t.Fatal("failed reload must close the resolver it created")
	}
	if got := p.routing.Load(); got != before {
		t.Fatal("failed reload must keep the current routing")
	}
}

// TestModelGatewayWatchLoopAPIKeyRotationAndRetry: the watcher fingerprint
// covers referenced external files, and failed reloads retry the same
// content instead of waiting for the config text to change again.
//  1. Rotating the API key file alone (identical config text) reloads routing.
//  2. Deleting the key file fails reloads while content stays the same;
//     restoring it makes the very next polls succeed without any config edit.
func TestModelGatewayWatchLoopAPIKeyRotationAndRetry(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	keyPath := filepath.Join(dir, "api.key")
	if err := os.WriteFile(keyPath, []byte("key-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGatewayConfigWithKeyFile(t, cfgPath, keyPath)
	p := newReloadTestProcessor(t, cfgPath, 10*time.Millisecond)
	initial := p.routing.Load()

	ctx, cancel := context.WithCancel(testLoggerCtx(t))
	p.watcherWG.Add(1)
	go func() {
		defer p.watcherWG.Done()
		p.modelGatewayWatchLoop(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // anchor initial fingerprint

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

	// Rotate only the key file: config text is identical, but the resolved
	// client config changed, so routing must swap.
	if err := os.WriteFile(keyPath, []byte("key-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "routing swap after api key rotation", func() bool {
		return p.routing.Load() != initial
	})
	rotated := p.routing.Load()

	// Break the referenced file: validation fails. Polls must keep failing
	// against the same content without ever swapping.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := p.routing.Load(); got != rotated {
		t.Fatal("failed reloads must keep the current routing")
	}

	// Restore the key file with yet another value: the retry must pick it up
	// without the config text ever changing.
	if err := os.WriteFile(keyPath, []byte("key-c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retry after referenced-file restore", func() bool {
		return p.routing.Load() != rotated
	})

	cancel()
	p.watcherWG.Wait()
}

// writeTestCACert writes a fresh self-signed CA certificate (valid PEM,
// loadable by the resolver's TLS setup) to path. serial and commonName vary
// the file content between calls, simulating a certificate rotation.
func writeTestCACert(t *testing.T, path string, serial int64, commonName string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
}

// writeGatewayConfigWithTLS writes a valid per-model config whose single
// gateway trusts the given CA certificate file and (optionally) reads its
// API key from keyFile.
func writeGatewayConfigWithTLS(t *testing.T, path, caCertFile, keyFile string) {
	t.Helper()
	keyLine := ""
	if keyFile != "" {
		keyLine = fmt.Sprintf("    api_key_file: %q\n", keyFile)
	}
	content := fmt.Sprintf(`model_gateways:
  m1:
    url: "https://a:8000"
    tls_ca_cert_file: %q
%s    request_timeout: 30s
    max_retries: 1
    initial_backoff: 1s
    max_backoff: 5s
`, caCertFile, keyLine)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
}

// TestApplyModelGatewayConfigTLSRotation: TLS files are referenced by path
// in GatewayClientConfig, so rotating the certificate content in place
// leaves the resolved gateway set byte-identical. The snapshot's referenced
// files digest must still force a resolver rebuild — a genuine no-op must
// not.
func TestApplyModelGatewayConfigTLSRotation(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	writeTestCACert(t, caPath, 1, "ca-one")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeGatewayConfigWithTLS(t, cfgPath, caPath, "")
	p := newReloadTestProcessor(t, cfgPath, 0)

	apply := func(t *testing.T) {
		t.Helper()
		newCfg, err := loadProcessorConfig(cfgPath)
		if err != nil {
			t.Fatalf("loadProcessorConfig: %v", err)
		}
		if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err != nil {
			t.Fatalf("applyModelGatewayConfig: %v", err)
		}
	}

	// Pure no-op: nothing changed — the snapshot must be kept.
	before := p.routing.Load()
	apply(t)
	if got := p.routing.Load(); got != before {
		t.Fatal("unchanged config must not replace the routing snapshot")
	}

	// Content-only rotation at the same path: config equality can't see it,
	// the referenced-files digest must.
	writeTestCACert(t, caPath, 2, "ca-two")
	apply(t)
	rotated := p.routing.Load()
	if rotated == before {
		t.Fatal("TLS cert rotation in place must rebuild the routing snapshot")
	}
	if rotated.resolver == before.resolver {
		t.Fatal("TLS cert rotation in place must rebuild the resolver")
	}

	// Path + content change: also rebuilds (the resolved config differs).
	caPath2 := filepath.Join(dir, "ca2.pem")
	writeTestCACert(t, caPath2, 3, "ca-three")
	writeGatewayConfigWithTLS(t, cfgPath, caPath2, "")
	apply(t)
	if got := p.routing.Load(); got == rotated {
		t.Fatal("TLS cert path change must rebuild the routing snapshot")
	}
}

// TestApplyModelGatewayConfigNilBaselineHashForcesRebuild: when the startup
// baseline could not hash the referenced files (a file was unreadable at
// startup), the snapshot stores a nil digest — "unknown", not "unchanged".
// An otherwise-identical reload must then rebuild the resolver once to
// anchor the digest; treating nil as "same content" would skip the swap
// without anchoring and permanently hide subsequent TLS/key rotations.
func TestApplyModelGatewayConfigNilBaselineHashForcesRebuild(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	writeTestCACert(t, caPath, 1, "ca-one")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeGatewayConfigWithTLS(t, cfgPath, caPath, "")
	p := newReloadTestProcessor(t, cfgPath, 0)

	// Simulate a startup baseline whose referenced files were unreadable.
	baseline := p.routing.Load()
	baseline.referencedFilesHash = nil

	apply := func(t *testing.T) {
		t.Helper()
		newCfg, err := loadProcessorConfig(cfgPath)
		if err != nil {
			t.Fatalf("loadProcessorConfig: %v", err)
		}
		if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err != nil {
			t.Fatalf("applyModelGatewayConfig: %v", err)
		}
	}

	// Same gateways, same referenced content, unknown baseline digest:
	// rebuild once and anchor the digest instead of no-op'ing forever.
	apply(t)
	anchored := p.routing.Load()
	if anchored == baseline {
		t.Fatal("nil baseline digest must force a rebuild, not a permanent no-op")
	}
	if anchored.referencedFilesHash == nil {
		t.Fatal("the rebuilt snapshot must anchor the referenced files digest")
	}

	// Anchored now: an unchanged reload is a true no-op again.
	apply(t)
	if got := p.routing.Load(); got != anchored {
		t.Fatal("unchanged config after anchoring must not replace the routing snapshot")
	}

	// And a later content-only rotation must still rebuild the resolver.
	writeTestCACert(t, caPath, 2, "ca-two")
	apply(t)
	rotated := p.routing.Load()
	if rotated == anchored || rotated.resolver == anchored.resolver {
		t.Fatal("TLS cert rotation after anchoring must rebuild the resolver")
	}
}

// TestApplyModelGatewayConfigAddsReferencedFile: introducing a new
// referenced file can leave the resolved gateway set byte-identical (here:
// an api key file whose trimmed content is the empty key that was used
// before). The enlarged referenced-file set must still trigger a rebuild —
// the new file's content now governs future change detection.
func TestApplyModelGatewayConfigAddsReferencedFile(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	writeTestCACert(t, caPath, 1, "ca-one")
	cfgPath := filepath.Join(dir, "config.yaml")
	writeGatewayConfigWithTLS(t, cfgPath, caPath, "")
	p := newReloadTestProcessor(t, cfgPath, 0)
	before := p.routing.Load()

	keyPath := filepath.Join(dir, "api.key")
	if err := os.WriteFile(keyPath, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeGatewayConfigWithTLS(t, cfgPath, caPath, keyPath)

	newCfg, err := loadProcessorConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadProcessorConfig: %v", err)
	}
	resolved, err := config.ResolveModelGateways(newCfg)
	if err != nil {
		t.Fatalf("ResolveModelGateways: %v", err)
	}
	if !resolvedGatewaysEqual(before.gateways, resolved) {
		t.Fatal("test premise broken: adding an empty api key file must leave the resolved gateway set unchanged")
	}

	if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err != nil {
		t.Fatalf("applyModelGatewayConfig: %v", err)
	}
	if got := p.routing.Load(); got == before {
		t.Fatal("introducing a new referenced file must rebuild the routing snapshot")
	}

	// Subsequent identical reload stays a no-op.
	after := p.routing.Load()
	newCfg, err = loadProcessorConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadProcessorConfig: %v", err)
	}
	if err := p.applyModelGatewayConfig(newCfg, testLogger(t)); err != nil {
		t.Fatalf("applyModelGatewayConfig: %v", err)
	}
	if got := p.routing.Load(); got != after {
		t.Fatal("unchanged config after the rebuild must not replace the routing snapshot")
	}
}

type closableFakeClient struct {
	fakeInferenceClient
	closed atomic.Bool
}

func (c *closableFakeClient) Close() error {
	c.closed.Store(true)
	return nil
}

// TestStopClosesReloadedResolver: the startup resolver is owned and closed by
// the Clientset; a resolver installed by a config reload must be closed by
// the processor itself on shutdown, or its HTTP transports leak.
func TestStopClosesReloadedResolver(t *testing.T) {
	clients := validProcessorClients(t)
	p, err := NewProcessor(config.NewConfig(), clients, "test-processor", testLogger(t))
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}

	// No reload ever happened: snapshot resolver == startup resolver, which
	// the Clientset owns — Stop must not close it here.
	startup := inference.NewSingleClientResolver(&fakeInferenceClient{})
	p.inference = startup
	p.routing.Store(&routingSnapshot{resolver: startup})
	p.Stop(t.Context())

	// After a reload, the swap target is processor-owned: Stop closes it.
	reloaded := &closableFakeClient{}
	p.routing.Store(&routingSnapshot{resolver: inference.NewSingleClientResolver(reloaded)})
	p.Stop(t.Context())
	if !reloaded.closed.Load() {
		t.Fatal("Stop must close the reloaded routing resolver")
	}
}

// Copyright 2026 The llm-d Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package e2e_test

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/shared"
)

var (
	testApiserverURL      = getEnvOrDefault("TEST_APISERVER_URL", "https://localhost:8000")
	testApiserverObsURL   = getEnvOrDefault("TEST_APISERVER_OBS_URL", "http://localhost:8081")
	testProcessorObsURL   = getEnvOrDefault("TEST_PROCESSOR_OBS_URL", "http://localhost:9090")
	testJaegerURL         = getEnvOrDefault("TEST_JAEGER_URL", "http://localhost:16686")
	testTenantHeader      = getEnvOrDefault("TEST_TENANT_HEADER", "X-MaaS-Username")
	testTenantID          = getEnvOrDefault("TEST_TENANT_ID", "default")
	testNamespace         = getEnvOrDefault("TEST_NAMESPACE", "default")
	testHelmRelease       = getEnvOrDefault("TEST_HELM_RELEASE", "batch-gateway")
	testPostgresqlRelease = getEnvOrDefault("TEST_POSTGRESQL_RELEASE", "postgresql")
	testRedisRelease      = getEnvOrDefault("TEST_REDIS_RELEASE", "redis")

	// testDBClientType and testExchangeClientType are detected from Helm
	// releases at startup; see detectDBClientType / detectExchangeClientType.
	testDBClientType       string
	testExchangeClientType string

	testRunID = fmt.Sprintf("%d", time.Now().UnixNano())

	// testModel is the model name used in batch input; configurable via TEST_MODEL env var.
	testModel           = getEnvOrDefault("TEST_MODEL", "sim-model")
	testModelB          = getEnvOrDefault("TEST_MODEL_B", "sim-model-b")
	testModel429        = getEnvOrDefault("TEST_MODEL_429", "sim-model-429")
	testModelAlwaysFail = getEnvOrDefault("TEST_MODEL_ALWAYS_FAIL", "sim-model-always-fail")
	testModelAIMD       = getEnvOrDefault("TEST_MODEL_AIMD", "sim-model-aimd")

	// testSimService* hold the Kubernetes service names for the simulators.
	// Must match VLLM_SIM_*_NAME in dev-common.sh (overridable via env).
	testSimService429  = getEnvOrDefault("TEST_SIM_SERVICE_429", "vllm-sim-429")
	testSimServiceAIMD = getEnvOrDefault("TEST_SIM_SERVICE_AIMD", "vllm-sim-aimd")

	// testJSONL is a valid batch input file with two requests.
	// max_tokens is kept small so batches finish quickly. Default TEST_MODEL (sim-model)
	// on dev-deploy uses ~100ms inter-token latency (sim-model-b uses ~500ms).
	testJSONL = strings.Join([]string{
		fmt.Sprintf(`{"custom_id":"req-1","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"Hello"}]}}`, testModel),
		fmt.Sprintf(`{"custom_id":"req-2","method":"POST","url":"/v1/chat/completions","body":{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"World"}]}}`, testModel),
	}, "\n")

	// testHTTPClient is used for direct HTTP calls; skips TLS verification for self-signed certs.
	testHTTPClient = &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // dev/test only
		},
		Timeout: 10 * time.Second,
	}

	// testKubectlAvailable is set once at TestE2E startup; when false,
	// verifications that require kubectl (e.g. log grepping) are skipped.
	testKubectlAvailable bool

	// testPassThroughHeaders maps header names (matching apiserver pass_through_headers
	// configured by dev-deploy.sh) to the values the e2e client sends when asserting
	// pass-through behavior.
	testPassThroughHeaders = map[string]string{
		"X-E2E-Pass-Through-1": "test-value-1",
		"X-E2E-Pass-Through-2": "test-value-2",
	}

	// testBatchMetadata is attached to every batch created via mustCreateBatch
	// so that metadata round-tripping is verified as part of the lifecycle test.
	testBatchMetadata = shared.Metadata{
		"env":    "e2e-test",
		"run_id": testRunID,
	}
)

func TestE2E(t *testing.T) {
	if out, err := exec.Command("kubectl", "cluster-info").CombinedOutput(); err != nil {
		t.Logf("kubectl not available, some checks will be skipped: %v\n%s", err, out)
	} else {
		testKubectlAvailable = true
	}

	testDBClientType = detectDBClientType(t)
	testExchangeClientType = detectExchangeClientType(t)
	t.Logf("DB client type: %s, exchange client type: %s", testDBClientType, testExchangeClientType)

	waitForReady(t, testApiserverObsURL, 30*time.Second)

	t.Run("Files", testFiles)
	t.Run("Batches", testBatches)
	t.Run("Concurrent", testConcurrent)
	t.Run("MultiTenant", testMultiTenant)
	t.Run("GarbageCollection", testGarbageCollection)
	t.Run("Observability", testObservability)
	t.Run("ProcessorGracefulShutdown", testProcessorGracefulShutdown)
	t.Run("FlowControl", testFlowControl)
	t.Run("AIMD", testAIMD)
	t.Run("HelmUpgrade", testHelmUpgrade)
}

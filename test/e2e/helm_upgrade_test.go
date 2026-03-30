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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const helmCmdTimeout = 5 * time.Minute

var testChartPath = getEnvOrDefault("TEST_CHART_PATH", "../../charts/batch-gateway")

// execContextFailureHint appends a short note when CommandContext hit its deadline,
// so CI logs distinguish slow/hung helm or kubectl from other failures.
func execContextFailureHint(err error) string {
	if err != nil && errors.Is(err, context.DeadlineExceeded) {
		return "\n(context deadline exceeded — helm/kubectl may be slow or hung)"
	}
	return ""
}

func testHelmUpgrade(t *testing.T) {
	if !testKubectlAvailable {
		t.Skip("kubectl not available, skipping helm upgrade test")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not available, skipping helm upgrade test")
	}

	processorCMName := fmt.Sprintf("%s-processor-config", testHelmRelease)

	// dev-deploy uses per-model mode with testModel (e.g. "sim-model").
	baselineCM := kubectlGetConfigMap(t, processorCMName)
	originalMaxRetries := parseModelGatewayMaxRetries(t, baselineCM, testModel)
	targetMaxRetries := alternateIntForHelmTest(originalMaxRetries)
	if targetMaxRetries == originalMaxRetries {
		t.Fatalf("internal: targetMaxRetries must differ from baseline (baseline=%d)", originalMaxRetries)
	}

	// Snapshot current values so we can restore on failure.
	// Use --all so the file includes chart defaults + prior overrides; otherwise
	// helm upgrade -f may merge with a partial snapshot and leave --set changes behind.
	// Cleanup is idempotent — if RestoreOriginalValues already ran successfully,
	// this just re-applies the same values with no actual change.
	originalValues := helmGetValues(t)
	t.Cleanup(func() {
		t.Log("cleanup: restoring original helm values")
		valuesFile := filepath.Join(t.TempDir(), "restore-values.yaml")
		if err := os.WriteFile(valuesFile, originalValues, 0o600); err != nil {
			t.Errorf("cleanup: failed to write restore values: %v — MANUAL HELM RESTORE REQUIRED", err)
			return
		}
		args := []string{"upgrade", testHelmRelease, testChartPath, "-n", testNamespace, "-f", valuesFile}
		ctx, cancel := context.WithTimeout(context.Background(), helmCmdTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
		if err != nil {
			t.Errorf("cleanup: helm restore failed: %v%s\n%s\nMANUAL HELM RESTORE REQUIRED", err, execContextFailureHint(err), out)
			return
		}
		rollCtx, rollCancel := context.WithTimeout(context.Background(), helmCmdTimeout)
		defer rollCancel()
		out, err = exec.CommandContext(rollCtx, "kubectl", "rollout", "status",
			fmt.Sprintf("deployment/%s-processor", testHelmRelease), "-n", testNamespace, "--timeout=180s",
		).CombinedOutput()
		if err != nil {
			t.Errorf("cleanup: rollout wait failed: %v%s\n%s", err, execContextFailureHint(err), out)
		}
	})

	// 1. Upgrade: set per-model gateway maxRetries to a value different from the baseline
	// (proves helm --set changed the rendered config, not a fixed "must be zero" check).
	t.Run("OverrideModelGatewayMaxRetries", func(t *testing.T) {
		helmUpgrade(t,
			"--set", fmt.Sprintf("processor.config.modelGateways.%s.maxRetries=%d", testModel, targetMaxRetries),
		)

		cm := kubectlGetConfigMap(t, processorCMName)
		got := parseModelGatewayMaxRetries(t, cm, testModel)
		if got != targetMaxRetries {
			t.Fatalf("expected model_gateways[%s].max_retries %d, got %d; config:\n%s", testModel, targetMaxRetries, got, cm)
		}
		if got == originalMaxRetries {
			t.Fatalf("expected max_retries to change from baseline %d, still %d; config:\n%s",
				originalMaxRetries, got, cm)
		}
		t.Logf("ConfigMap max_retries changed from %d to %d (model_gateways[%s])", originalMaxRetries, got, testModel)

		waitForRollout(t, fmt.Sprintf("%s-processor", testHelmRelease))
		waitForReady(t, testProcessorObsURL, 60*time.Second)
		t.Log("processor healthy after helm upgrade")
	})

	// 2. Restore original values and verify max_retries matches baseline again
	t.Run("RestoreOriginalValues", func(t *testing.T) {
		helmUpgradeWithValues(t, originalValues)

		cm := kubectlGetConfigMap(t, processorCMName)
		if got := parseModelGatewayMaxRetries(t, cm, testModel); got != originalMaxRetries {
			t.Fatalf("expected model_gateways[%s].max_retries restored to %d, got %d; config:\n%s",
				testModel, originalMaxRetries, got, cm)
		}

		waitForRollout(t, fmt.Sprintf("%s-processor", testHelmRelease))
		waitForReady(t, testProcessorObsURL, 60*time.Second)
		t.Log("original values restored successfully")
	})
}

// helmGetValues returns the current release values as raw YAML bytes (all computed values).
func helmGetValues(t *testing.T) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), helmCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "helm", "get", "values",
		testHelmRelease, "-n", testNamespace, "-a", "-o", "yaml",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("helm get values failed: %v%s\n%s", err, execContextFailureHint(err), out)
	}
	return out
}

func helmUpgrade(t *testing.T, extraArgs ...string) {
	t.Helper()
	helmUpgradeWithValues(t, helmGetValues(t), extraArgs...)
}

// helmUpgradeWithValues upgrades the release using the given values YAML,
// with optional extra --set flags.
func helmUpgradeWithValues(t *testing.T, values []byte, extraArgs ...string) {
	t.Helper()

	valuesFile := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(valuesFile, values, 0o600); err != nil {
		t.Fatalf("failed to write values file: %v", err)
	}

	args := []string{
		"upgrade", testHelmRelease, testChartPath,
		"-n", testNamespace,
		"-f", valuesFile,
	}
	args = append(args, extraArgs...)

	t.Logf("helm %s", strings.Join(args, " "))
	ctx, cancel := context.WithTimeout(context.Background(), helmCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm upgrade failed: %v%s\n%s", err, execContextFailureHint(err), out)
	}
	t.Logf("helm upgrade output: %s", strings.TrimSpace(string(out)))
}

func kubectlGetConfigMap(t *testing.T, name string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), helmCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "get", "configmap", name,
		"-n", testNamespace,
		"-o", "jsonpath={.data['config\\.yaml']}",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl get configmap failed: %v%s\n%s", err, execContextFailureHint(err), out)
	}
	result := string(out)
	if strings.TrimSpace(result) == "" {
		t.Fatalf("configmap %s exists but data['config.yaml'] is empty or missing", name)
	}
	return result
}

func waitForRollout(t *testing.T, deployment string) {
	t.Helper()

	t.Logf("waiting for rollout of %s...", deployment)
	ctx, cancel := context.WithTimeout(context.Background(), helmCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		fmt.Sprintf("deployment/%s", deployment),
		"-n", testNamespace,
		"--timeout=180s",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("rollout of %s failed: %v%s\n%s", deployment, err, execContextFailureHint(err), out)
	}
	t.Logf("rollout of %s complete", deployment)
}

// parseModelGatewayMaxRetries returns model_gateways[model].max_retries from processor config.yaml.
func parseModelGatewayMaxRetries(t *testing.T, configYAML, model string) int {
	t.Helper()
	var root struct {
		ModelGateways map[string]struct {
			MaxRetries *int `yaml:"max_retries"`
		} `yaml:"model_gateways"`
	}
	if err := yaml.Unmarshal([]byte(configYAML), &root); err != nil {
		t.Fatalf("parse processor config.yaml: %v", err)
	}
	gw, ok := root.ModelGateways[model]
	if !ok {
		t.Fatalf("model_gateways[%s] missing in config:\n%s", model, configYAML)
	}
	if gw.MaxRetries == nil {
		t.Fatalf("model_gateways[%s].max_retries key is absent (possible template bug):\n%s", model, configYAML)
	}
	return *gw.MaxRetries
}

// alternateIntForHelmTest returns an integer guaranteed to differ from baseline,
// used so the upgrade test asserts a real change rather than a hard-coded sentinel.
func alternateIntForHelmTest(baseline int) int {
	if baseline == 0 {
		return 3 // typical chart default for model gateway maxRetries
	}
	return 0
}

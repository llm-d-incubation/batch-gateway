#!/bin/bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

echo "=== Case 1: k8s install + full uninstall ==="
./deploy-k8s.sh install
./deploy-k8s.sh test
UNINSTALL_ALL=1 ./deploy-k8s.sh uninstall

echo "=== Case 2: k8s partial uninstall + reinstall ==="
./deploy-k8s.sh install
./deploy-k8s.sh test
UNINSTALL_ALL=0 ./deploy-k8s.sh uninstall

echo "--- Verifying infrastructure still running after partial uninstall ---"
kubectl get deployment -n istio-ingress || { echo "FAIL: istio-ingress missing"; exit 1; }
kubectl get deployment -n kuadrant-system || { echo "FAIL: kuadrant missing"; exit 1; }
kubectl get deployment -n cert-manager || { echo "FAIL: cert-manager missing"; exit 1; }
echo "--- All infrastructure verified ---"

./deploy-k8s.sh install
./deploy-k8s.sh test
UNINSTALL_ALL=1 ./deploy-k8s.sh uninstall

echo "=== All test cases passed ==="

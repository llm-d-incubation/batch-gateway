#!/bin/bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

# TODO: remove after 3.4 is released
export RHOAI_CHANNEL=beta

echo "=== Case 1: RHOAI + full uninstall ==="
./deploy-rhoai.sh install
./deploy-rhoai.sh test
UNINSTALL_ALL=1 ./deploy-rhoai.sh uninstall

echo "=== Case 2: RHOAI + partial uninstall + reinstall ==="
./deploy-rhoai.sh install
./deploy-rhoai.sh test
UNINSTALL_ALL=0 ./deploy-rhoai.sh uninstall

echo "--- Verifying operators still running after partial uninstall ---"
oc get subscription rhods-operator -n redhat-ods-operator || { echo "FAIL: rhods-operator subscription missing"; exit 1; }
oc get subscription rhcl-operator -n kuadrant-system || { echo "FAIL: rhcl-operator subscription missing"; exit 1; }
oc get subscription openshift-cert-manager-operator -n cert-manager-operator || { echo "FAIL: cert-manager subscription missing"; exit 1; }
oc get subscription leader-worker-set -n openshift-lws-operator || { echo "FAIL: leader-worker-set subscription missing"; exit 1; }
echo "--- All operators verified ---"

./deploy-rhoai.sh install
./deploy-rhoai.sh test
UNINSTALL_ALL=1 ./deploy-rhoai.sh uninstall

echo "=== All test cases passed ==="

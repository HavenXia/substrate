#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#      http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Wipes every substrate install artifact off the cluster (the current install
# is 36d-broken; nothing precious). CA/JWT pools die with their namespaces, so
# bootstrap.sh must run again afterwards. PVCs die with namespace ate-system.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh && -z "${NO_DEV_ENV:-}" ]]; then
  source .ate-dev-env.sh
fi

run_kubectl() {
  kubectl \
    ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} \
    "$@"
}

echo "==> deleting demo namespaces"
run_kubectl delete namespace ate-demo-counter ate-demo-dual --ignore-not-found

echo "==> deleting the ate-install bundle (includes namespaces ate-system and podcertificate-controller-system)"
# Tolerate unknown-kind errors: SandboxConfig docs fail to map once the CRDs
# are already gone from a previous wipe.
run_kubectl delete --ignore-not-found -f manifests/ate-install || true
run_kubectl delete namespace podcertificate-controller-system --ignore-not-found

echo "==> deleting CRDs (current ate.dev trio + stale ate.gke.io pair)"
run_kubectl delete crd --ignore-not-found \
  actortemplates.ate.dev \
  workerpools.ate.dev \
  sandboxconfigs.ate.dev \
  actortemplates.ate.gke.io \
  workerpools.ate.gke.io

wait_ns_gone() {
  local ns="$1"
  local deadline=$((SECONDS + 300))
  while run_kubectl get namespace "${ns}" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      echo "error: namespace ${ns} still terminating after 300s" >&2
      return 1
    fi
    echo "waiting for namespace ${ns} to terminate..."
    sleep 5
  done
}

echo "==> waiting for namespaces to be gone"
wait_ns_gone ate-demo-counter
wait_ns_gone ate-demo-dual
wait_ns_gone ate-system
wait_ns_gone podcertificate-controller-system

echo "==> wipe complete"
run_kubectl get namespaces

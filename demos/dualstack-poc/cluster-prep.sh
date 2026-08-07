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

# Creates the two version-labeled node pools the dual-live POC schedules onto
# (atelet DaemonSets and worker pods pin to substrate-version node labels).
# Idempotent: existing pools are left alone.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
if [[ -f "${ROOT}/.ate-dev-env.sh" && -z "${NO_DEV_ENV:-}" ]]; then
  source "${ROOT}/.ate-dev-env.sh"
fi

CLUSTER_NAME="${CLUSTER_NAME:-substrate-poc}"
CLUSTER_LOCATION="${CLUSTER_LOCATION:-us-central1-c}"

run_gcloud() {
  gcloud "$@" \
    ${PROJECT_ID:+--project=${PROJECT_ID}}
}

run_kubectl() {
  kubectl \
    ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} \
    "$@"
}

ensure_pool() {
  local pool="$1"
  local version="$2"
  if run_gcloud container node-pools describe "${pool}" \
      --cluster "${CLUSTER_NAME}" --zone "${CLUSTER_LOCATION}" >/dev/null 2>&1; then
    echo "node pool ${pool} already exists, skipping"
    return 0
  fi
  echo "creating node pool ${pool} (substrate-version=${version})..."
  run_gcloud container node-pools create "${pool}" \
    --cluster "${CLUSTER_NAME}" \
    --zone "${CLUSTER_LOCATION}" \
    --machine-type e2-standard-4 \
    --num-nodes 2 \
    --node-labels "substrate-version=${version}"
}

ensure_pool poc-v1 0.1.0
ensure_pool poc-v2 0.2.0

run_kubectl get nodes -L substrate-version

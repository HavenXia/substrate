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

# CA/JWT/prereq bootstrap for the dual-live POC, lifted from
# hack/install-ate.sh. Creates the client-side-generated pools and ConfigMaps
# both stacks share, deploys the (shared, single-instance) podcertificate
# controller, and waits for its ClusterTrustBundles. Everything is
# create-if-missing so re-runs never rotate CAs out from under a live stack.
# Run after wipe.sh and before the operator applies the rendered bundles.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

if [[ -f .ate-dev-env.sh && -z "${NO_DEV_ENV:-}" ]]; then
  source .ate-dev-env.sh
fi

# run_ko below builds and pushes the podcertcontroller image.
: "${KO_DOCKER_REPO:?set by .ate-dev-env.sh (or the environment)}"

run_kubectl() {
  kubectl \
    ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} \
    "$@"
}

run_kubectl_ate() {
  go run ./cmd/kubectl-ate \
    ${KUBECTL_CONTEXT:+--context=${KUBECTL_CONTEXT}} \
    "$@"
}

# Replicates run_ko from hack/install-ate.sh for the apply subcommand.
run_ko_apply() {
  local ldflags=()
  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ -n "${line}" ]] && ldflags+=("--ldflags=${line}")
  done < <(make ldflags)
  ./hack/run-tool.sh ko apply "$@" \
    "${ldflags[@]}" \
    ${KUBECTL_CONTEXT:+-- --context="${KUBECTL_CONTEXT}"}
}

log_step() {
  echo "==> $1"
}

# Extract a CA pool secret's RootCertificateDER and emit it as a PEM certificate.
ca_pool_root_pem() {
  local secret="$1"
  local pool_json=""
  pool_json=$(run_kubectl get secret -n podcertificate-controller-system "${secret}" -o jsonpath='{.data.pool}' | base64 --decode)
  local der_base64=""
  der_base64=$(echo "${pool_json}" | grep -o '"RootCertificateDER":"[^"]*' | sed 's/"RootCertificateDER":"//')
  echo "${der_base64}" | base64 --decode | openssl x509 -inform der -outform pem
}

log_step "namespace ate-system"
run_kubectl create namespace ate-system --dry-run=client -o yaml \
  | run_kubectl apply -f -
run_kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/ate-system --timeout=60s

log_step "actor-id-jwt-pool"
if run_kubectl get secret -n ate-system actor-id-jwt-pool >/dev/null 2>&1; then
  echo "already exists, skipping"
else
  run_kubectl_ate admin make-jwt-pool \
    --key-id="1" \
    --name="actor-id-jwt-pool" \
    --secret-namespace=ate-system
fi

log_step "actor-id-ca-pool"
if run_kubectl get secret -n ate-system actor-id-ca-pool >/dev/null 2>&1; then
  echo "already exists, skipping"
else
  run_kubectl_ate admin make-ca-pool \
    --ca-id="1" \
    --name="actor-id-ca-pool" \
    --secret-namespace=ate-system
fi

# The podcertificate CA pools must exist before valkey-ca-certs, which is
# assembled from their roots.
log_step "podcertificate controller CA pools"
run_kubectl create namespace podcertificate-controller-system --dry-run=client -o yaml \
  | run_kubectl apply -f -
if run_kubectl get secret -n podcertificate-controller-system service-dns-ca-pool >/dev/null 2>&1; then
  echo "service-dns-ca-pool already exists, skipping"
else
  run_kubectl_ate admin make-ca-pool \
    --ca-id="1" \
    --name="service-dns-ca-pool" \
    --secret-namespace=podcertificate-controller-system
fi
if run_kubectl get secret -n podcertificate-controller-system pod-identity-ca-pool >/dev/null 2>&1; then
  echo "pod-identity-ca-pool already exists, skipping"
else
  run_kubectl_ate admin make-ca-pool \
    --ca-id="1" \
    --name="pod-identity-ca-pool" \
    --secret-namespace=podcertificate-controller-system
fi

log_step "valkey-ca-certs"
if run_kubectl get secret -n ate-system valkey-ca-certs >/dev/null 2>&1; then
  echo "already exists, skipping"
else
  # valkey verifies peers' server certs (servicedns CA) and client certs
  # (podidentity CA) with one CA file, so it needs both roots.
  servicedns_root=$(ca_pool_root_pem service-dns-ca-pool)
  podidentity_root=$(ca_pool_root_pem pod-identity-ca-pool)
  if [[ -z "${servicedns_root}" || -z "${podidentity_root}" ]]; then
    echo "error: failed to extract a CA root for valkey-ca-certs" >&2
    exit 1
  fi
  ca_certs=$(printf '%s\n%s\n' "${servicedns_root}" "${podidentity_root}")
  run_kubectl create secret generic valkey-ca-certs \
    --from-literal=ca.crt="${ca_certs}" \
    -n ate-system \
    --dry-run=client -o yaml \
    | run_kubectl apply -f -
fi

log_step "ate-api-server-envvars"
if run_kubectl get configmap -n ate-system ate-api-server-envvars >/dev/null 2>&1; then
  echo "already exists, skipping"
else
  jwt_issuer=""
  if [[ -n "${PROJECT_ID:-}" && -n "${CLUSTER_LOCATION:-}" && -n "${CLUSTER_NAME:-}" ]]; then
    jwt_issuer="https://container.googleapis.com/v1/projects/${PROJECT_ID}/locations/${CLUSTER_LOCATION}/clusters/${CLUSTER_NAME}"
  else
    jwt_issuer=$(run_kubectl get --raw /.well-known/openid-configuration 2>/dev/null | grep -o '"issuer":"[^"]*' | sed 's/"issuer":"//' || true)
    if [[ -z "${jwt_issuer}" ]]; then
      jwt_issuer="https://kubernetes.default.svc"
    fi
  fi
  run_kubectl create configmap -n ate-system ate-api-server-envvars \
    --from-literal=ATE_API_REDIS_ADDRESS="valkey-cluster.ate-system.svc:6379" \
    --from-literal=ATE_API_REDIS_USE_IAM_AUTH="false" \
    --from-literal=ATE_API_REDIS_TLS_SERVER_NAME="valkey-cluster.ate-system.svc" \
    --from-literal=ATE_API_REDIS_CLIENT_CERT="/run/podidentity.podcert.ate.dev/credential-bundle.pem" \
    --from-literal=ATE_API_K8SJWT_ISSUER="${jwt_issuer}" \
    --dry-run=client -o yaml \
    | run_kubectl apply -f -
fi

log_step "podcertificate controller"
run_ko_apply -f manifests/ate-install/pod-certificate-controller.yaml
run_kubectl rollout status deployment/podcertificate-controller \
  -n podcertificate-controller-system --timeout=120s

wait_for_ctb() {
  local name="$1"
  local deadline=$((SECONDS + 180))
  until run_kubectl get clustertrustbundles "${name}" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      echo "error: ClusterTrustBundle ${name} not created within 180s." >&2
      echo "debug: kubectl logs -n podcertificate-controller-system deploy/podcertificate-controller" >&2
      exit 1
    fi
    sleep 2
  done
  echo "ClusterTrustBundle ${name} ready"
}

log_step "waiting for ClusterTrustBundles"
wait_for_ctb "podidentity.podcert.ate.dev:identity:primary-bundle"
wait_for_ctb "servicedns.podcert.ate.dev:identity:primary-bundle"

log_step "bootstrap complete"

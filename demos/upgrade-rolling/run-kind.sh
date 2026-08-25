#!/usr/bin/env bash

# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# run-kind.sh -- scripted rolling-upgrade demo on a throwaway kind cluster.
# Follows the runbook in README.md: from-zero install at VERSION_A (the same
# source tree built under two -ldflags version stamps), a stateful counter
# actor, `kubectl ate upgrade run` to VERSION_B, verification that the actor's
# durable state survived, optional rollback + re-upgrade, then cleanup of the
# retired version.
#
# The continuity proof is the counter's "preserved file counter": the demo
# template snapshots durable-dir data on suspend (snapshotsConfig
# onCommit: Data), so the in-RAM count resets with the process while the
# file counter carries across every suspend/resume, roll included.
#
# kind only. The cluster is named ate-roll-demo and is DELETED on exit
# (KEEP_CLUSTER=true keeps it). Every kubectl call pins --context to that
# cluster; no GKE/cloud resource is ever touched.
#
# Knobs: VERSION_A, VERSION_B, ROUTER_PORT, DO_ROLLBACK, SHOW_DRAIN_GATE,
# KEEP_CLUSTER (see README.md).

set -o errexit -o nounset -o pipefail -o errtrace

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${ROOT}"

CLUSTER="ate-roll-demo"
CTX="kind-${CLUSTER}"
VERSION_A="${VERSION_A:-v0.0.0-roll-a}"
VERSION_B="${VERSION_B:-v0.0.0-roll-b}"
ROUTER_PORT="${ROUTER_PORT:-18080}"
DO_ROLLBACK="${DO_ROLLBACK:-true}"
SHOW_DRAIN_GATE="${SHOW_DRAIN_GATE:-true}"
KEEP_CLUSTER="${KEEP_CLUSTER:-false}"

ATESPACE="roll"
ACTOR="c1"
ACTOR_HOST="${ACTOR}.${ATESPACE}.actors.resources.substrate.ate.dev"
DEMO_NS="ate-demo-counter"
VERSION_LABEL_KEY="ate.dev/substrate-version"
POOL_SELECTOR="ate.dev/worker-pool=counter"

TMP="$(mktemp -d)"
ATECTL="${TMP}/kubectl-ate"
PF_PID=""
LAST_COUNT=""

log()  { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

trap 'printf "error: command failed at %s:%s: %s\n" "${BASH_SOURCE[0]}" "${LINENO}" "${BASH_COMMAND}" >&2' ERR

cleanup() {
  local rc=$?
  pf_stop
  if [[ "${KEEP_CLUSTER}" == "true" ]]; then
    log "KEEP_CLUSTER=true: leaving kind cluster '${CLUSTER}' up (kubectl --context ${CTX})"
  else
    log "Deleting kind cluster '${CLUSTER}'"
    "${ROOT}/hack/kind.sh" delete cluster --name "${CLUSTER}" || true
  fi
  rm -rf "${TMP}"
  if [[ ${rc} -eq 0 ]]; then
    log "Demo finished: PASS"
  else
    log "Demo finished: FAIL (exit ${rc})"
  fi
}
trap cleanup EXIT

kc()  { kubectl --context "${CTX}" "$@"; }
ate() { "${ATECTL}" --context "${CTX}" "$@"; }

# install_ate runs the kind installer wrapper with a pinned build version.
# VERSION rides the environment into `make ldflags`, so the same value is
# stamped into every ko-built binary and rendered into the versioned
# manifests (atelet DaemonSet suffix, node labels, worker set labels).
install_ate() {
  local version="$1"
  shift
  log "hack/install-ate-kind.sh $* (VERSION=${version})"
  VERSION="${version}" KIND_CLUSTER_NAME="${CLUSTER}" "${ROOT}/hack/install-ate-kind.sh" "$@"
}

preflight() {
  log "Preflight"
  local tool
  for tool in docker kubectl go jq curl git; do
    command -v "${tool}" >/dev/null 2>&1 || die "required tool not found: ${tool}"
  done
  docker info >/dev/null 2>&1 || die "docker daemon is not running"
  [[ "${VERSION_A}" != "${VERSION_B}" ]] || die "VERSION_A and VERSION_B must differ"
  # Same acceptance rule as the installer, from the same implementation.
  local v
  for v in "${VERSION_A}" "${VERSION_B}"; do
    go run ./internal/versionlabel/cmd "${v}" >/dev/null || die "version '${v}' is not a valid label value"
  done
  info "versions: ${VERSION_A} -> ${VERSION_B}"
  info "building the upgrade driver (cmd/kubectl-ate)"
  go build -o "${ATECTL}" ./cmd/kubectl-ate
}

pf_stop() {
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
    PF_PID=""
  fi
}

# pf_start (re)creates the router port-forward. It is restarted per traffic
# phase because a port-forward pins one router pod and dies with it when the
# control plane rolls between phases.
pf_start() {
  pf_stop
  kc port-forward -n ate-system svc/atenet-router "${ROUTER_PORT}:80" >"${TMP}/pf.log" 2>&1 &
  PF_PID=$!
  local i
  for i in $(seq 1 30); do
    if curl -s -o /dev/null --max-time 2 "http://127.0.0.1:${ROUTER_PORT}"; then
      return 0
    fi
    kill -0 "${PF_PID}" 2>/dev/null || die "router port-forward died: $(cat "${TMP}/pf.log")"
    sleep 1
  done
  die "router port-forward never became reachable on 127.0.0.1:${ROUTER_PORT}"
}

# hit_counter sends n requests to the actor through the router and records
# the last response's "preserved file counter" in LAST_COUNT. The file
# counter, not the memory count, is the continuity proof: the template
# suspends with a Data-scope snapshot, so the process (and its RAM count)
# ends at suspend while the durable-dir state carries over. The first
# request after a version flip auto-resumes the actor onto a new worker,
# which can be slow (snapshot download), hence the long timeout and retries.
hit_counter() {
  local n="$1" resp="" i attempt
  pf_start
  for i in $(seq 1 "${n}"); do
    resp=""
    for attempt in $(seq 1 5); do
      if resp="$(curl -s --max-time 180 -H "Host: ${ACTOR_HOST}" "http://127.0.0.1:${ROUTER_PORT}/")" \
          && [[ -n "${resp}" ]]; then
        break
      fi
      info "request ${i} attempt ${attempt} failed; retrying in 5s"
      sleep 5
      pf_start
    done
    [[ -n "${resp}" ]] || die "the counter actor did not answer through the router"
    info "response ${i}/${n}: ${resp}"
  done
  pf_stop
  LAST_COUNT="$(grep -oE 'preserved file counter: [0-9]+' <<<"${resp}" | grep -oE '[0-9]+' || true)"
  [[ -n "${LAST_COUNT}" ]] || die "response carries no file counter: ${resp}"
}

assert_count_grew() {
  local prev="$1" what="$2"
  [[ "${LAST_COUNT}" -gt "${prev}" ]] \
    || die "file counter did not continue across ${what} (${prev} -> ${LAST_COUNT})"
  info "file counter ${prev} -> ${LAST_COUNT}: durable state survived ${what}"
}

actor_state() {
  ate get actor "${ACTOR}" -a "${ATESPACE}" -o json 2>/dev/null \
    | jq -r '.actors[0].status.state // empty' || true
}

wait_actor_state() {
  local want="$1" timeout="${2:-180}" state="" start
  start="$(date +%s)"
  while :; do
    state="$(actor_state)"
    if [[ "${state}" == "${want}" ]]; then
      info "actor ${ACTOR} is ${want}"
      return 0
    fi
    (( $(date +%s) - start < timeout )) \
      || die "actor ${ACTOR} never reached ${want} (last state: ${state:-unknown})"
    info "actor state: ${state:-unknown}; waiting for ${want}"
    sleep 5
  done
}

suspend_actor() {
  log "Suspending actor ${ACTOR} (the 'customer drains at their own pace' step)"
  ate suspend actor "${ACTOR}" -a "${ATESPACE}"
  wait_actor_state ACTOR_STATE_SUSPENDED
}

worker_set_names() {
  kc get deploy -n "${DEMO_NS}" \
    -l "${POOL_SELECTOR},${VERSION_LABEL_KEY}=$1" -o name 2>/dev/null
}

wait_worker_set_exists() {
  local version="$1" i
  for i in $(seq 1 60); do
    if [[ -n "$(worker_set_names "${version}")" ]]; then
      info "worker set for ${version} exists"
      return 0
    fi
    sleep 2
  done
  die "atecontroller never rendered the ${version} worker set for pool counter"
}

show_state() {
  log "Cluster state"
  ate upgrade status
  kc get nodes -L "${VERSION_LABEL_KEY}"
  kc get deploy,pods -n "${DEMO_NS}" -L "${VERSION_LABEL_KEY}"
  kc get ds -n ate-system -l app=atelet -L "${VERSION_LABEL_KEY}"
}

assert_no_running_workers() {
  local version="$1" running=""
  running="$(kc get pods -n "${DEMO_NS}" \
    -l "${POOL_SELECTOR},${VERSION_LABEL_KEY}=${version}" \
    --field-selector=status.phase=Running -o name)"
  [[ -z "${running}" ]] \
    || die "unexpected Running ${version} worker pods after the roll: ${running}"
  info "no ${version} worker pod is Running (replacements stay Pending as the rollback spring)"
}

show_drain_gate() {
  [[ "${SHOW_DRAIN_GATE}" == "true" ]] || return 0
  log "Demonstrating the passive-drain gate: a live actor blocks the roll"
  info "the driver never force-suspends: with the actor still RUNNING the"
  info "passive wait cannot empty the node, so this bounded attempt must time out"
  local out=""
  if out="$(ate upgrade run --target-version "${VERSION_B}" --poll-interval 3s --drain-timeout 15s 2>&1)"; then
    printf '%s\n' "${out}"
    die "upgrade run succeeded while an actor was RUNNING; the drain gate is broken"
  fi
  printf '%s\n' "${out}"
  # It must fail on the actor-wait timeout, not on some earlier step.
  grep -q "actor(s) on node after" <<<"${out}" \
    || die "upgrade run failed before reaching the passive-drain gate"
  info "upgrade run refused to proceed past the live actor, as designed"
}

roll_to() {
  local verb="$1" version="$2"
  log "kubectl ate upgrade ${verb} --target-version ${version}"
  ate upgrade "${verb}" --target-version "${version}" --poll-interval 3s --ready-timeout 10m
}

main() {
  preflight

  log "Step 1: create throwaway kind cluster '${CLUSTER}'"
  KIND_CLUSTER_NAME="${CLUSTER}" "${ROOT}/hack/create-kind-cluster.sh"

  log "Step 2: install substrate at ${VERSION_A}"
  install_ate "${VERSION_A}" --deploy-ate-system --rollout-timeout=300s
  # Waits for the versioned worker set and for actortemplate/counter (baking
  # the golden snapshot pays first-install one-time costs).
  install_ate "${VERSION_A}" --deploy-demo-counter
  show_state

  log "Step 3: create the counter actor and drive traffic on ${VERSION_A}"
  ate create atespace "${ATESPACE}"
  ate create actor "${ACTOR}" -a "${ATESPACE}" --template "${DEMO_NS}/counter"
  hit_counter 3
  local count_a="${LAST_COUNT}"

  log "Step 4: stage the ${VERSION_B} control plane (CRDs, controller, atelet DS)"
  # Same tree, different stamp, applied in the proposal's order: CRDs and the
  # B controller first (renders a Pending B worker set; the A sets are
  # untouched per hands-off), then the atelet-B DaemonSet (zero pods until a
  # node is labeled B). ate-api and atenet stay at A until the dataplane has
  # rolled (step 6b), so no new-server/old-dataplane window opens.
  install_ate "${VERSION_B}" --deploy-ate-controller --rollout-timeout=300s
  install_ate "${VERSION_B}" --deploy-atelet --rollout-timeout=300s
  wait_worker_set_exists "${VERSION_B}"
  show_state

  show_drain_gate

  log "Step 5: customer suspends the actor at their own pace"
  suspend_actor

  log "Step 6: roll the node(s) to ${VERSION_B}"
  roll_to run "${VERSION_B}"
  assert_no_running_workers "${VERSION_A}"
  show_state

  log "Step 6b: finish the control plane: ate-api and atenet to ${VERSION_B}"
  install_ate "${VERSION_B}" --deploy-ate-apiserver --rollout-timeout=300s
  install_ate "${VERSION_B}" --deploy-atenet --rollout-timeout=300s

  log "Step 7: verify state continuity on ${VERSION_B}"
  hit_counter 1
  assert_count_grew "${count_a}" "the roll to ${VERSION_B}"
  local count_b="${LAST_COUNT}"

  if [[ "${DO_ROLLBACK}" == "true" ]]; then
    log "Step 8 (optional): roll back to ${VERSION_A}"
    info "the A worker sets kept Pending pods; relabeling the node seats them again"
    suspend_actor
    roll_to rollback "${VERSION_A}"
    show_state
    hit_counter 1
    assert_count_grew "${count_b}" "the rollback to ${VERSION_A}"
    count_b="${LAST_COUNT}"

    log "Step 8b: roll forward to ${VERSION_B} again before cleanup"
    suspend_actor
    roll_to run "${VERSION_B}"
    hit_counter 1
    assert_count_grew "${count_b}" "the second roll to ${VERSION_B}"
  fi

  log "Step 9: cleanup after soak: retire ${VERSION_A}"
  ate upgrade cleanup --version "${VERSION_A}"
  [[ -z "$(worker_set_names "${VERSION_A}")" ]] \
    || die "cleanup left ${VERSION_A} worker Deployments behind"
  [[ -z "$(kc get ds -n ate-system -l "app=atelet,${VERSION_LABEL_KEY}=${VERSION_A}" -o name)" ]] \
    || die "cleanup left the ${VERSION_A} atelet DaemonSet behind"
  show_state

  log "Rolling upgrade demo complete: ${VERSION_A} -> ${VERSION_B}, state preserved throughout"
}

main "$@"

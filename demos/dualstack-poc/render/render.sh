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

# Renders the dual-live POC bundles into demos/dualstack-poc/render/out/:
#   shared.yaml, stack-0.1.0.yaml, stack-0.2.0.yaml, crds.yaml
# Each stack is resolved with `ko resolve` under its own VERSION stamp, so the
# two stacks run genuinely different (differently version-stamped) images.
# NOTE: ko resolve builds AND pushes images to KO_DOCKER_REPO.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

# KO_DOCKER_REPO / KO_DEFAULTPLATFORMS / BUCKET_NAME come from the dev env.
if [[ ! -f .ate-dev-env.sh ]]; then
  echo "error: ${ROOT}/.ate-dev-env.sh not found (ko resolve needs KO_DOCKER_REPO; templates need BUCKET_NAME)" >&2
  exit 1
fi
source .ate-dev-env.sh

: "${KO_DOCKER_REPO:?must be set by .ate-dev-env.sh}"
: "${BUCKET_NAME:?must be set by .ate-dev-env.sh}"

RENDER_DIR="demos/dualstack-poc/render"
OUT_DIR="${RENDER_DIR}/out"
TMP_DIR="${OUT_DIR}/tmp"
mkdir -p "${TMP_DIR}"

INITIAL_VERSION="0.1.0"
VERSIONS="0.1.0 0.2.0"

# Replicates run_ko from hack/install-ate.sh: version-stamping ldflags from
# `make ldflags` (VERSION honored via its ?= default), pinned ko via
# hack/run-tool.sh. `ko resolve` must not get kubectl --context args.
run_ko_resolve() {
  local version="$1"
  local file="$2"
  local ldflags=()
  while IFS= read -r line || [[ -n "${line}" ]]; do
    [[ -n "${line}" ]] && ldflags+=("--ldflags=${line}")
  done < <(VERSION="v${version}" make ldflags)
  ./hack/run-tool.sh ko resolve -f "${file}" "${ldflags[@]}"
}

# substitute <version> <sfx> reads a template on stdin and expands the four
# placeholders. Other ${...} occurrences (valkey scripts, $(POD_*) env) pass
# through untouched.
substitute() {
  local version="$1"
  local sfx="$2"
  sed \
    -e "s|\${VERSION}|${version}|g" \
    -e "s|\${SFX}|${sfx}|g" \
    -e "s|\${INITIAL_VERSION}|${INITIAL_VERSION}|g" \
    -e "s|\${BUCKET_NAME}|${BUCKET_NAME}|g"
}

for v in ${VERSIONS}; do
  sfx="$(echo "${v}" | tr . -)"
  tmp="${TMP_DIR}/stack-${v}.yaml"
  substitute "${v}" "${sfx}" <"${RENDER_DIR}/stack.yaml.tmpl" >"${tmp}"
  echo "==> rendering stack-${v}.yaml (ko resolve, VERSION=v${v})"
  run_ko_resolve "${v}" "${tmp}" >"${OUT_DIR}/stack-${v}.yaml"
done

tmp="${TMP_DIR}/shared.yaml"
initial_sfx="$(echo "${INITIAL_VERSION}" | tr . -)"
substitute "${INITIAL_VERSION}" "${initial_sfx}" <"${RENDER_DIR}/shared.yaml.tmpl" >"${tmp}"
echo "==> rendering shared.yaml (ko resolve, VERSION=v${INITIAL_VERSION})"
run_ko_resolve "${INITIAL_VERSION}" "${tmp}" >"${OUT_DIR}/shared.yaml"

# crds.yaml: generated CRDs + ClusterRole ate-controller + the operator's
# Substrate CRD. Each generated file already opens with `---`.
SUBSTRATE_CRD="demos/dualstack-poc/operator/substrate-crd.yaml"
if [[ ! -f "${SUBSTRATE_CRD}" ]]; then
  echo "error: ${SUBSTRATE_CRD} not found (operator work package delivers it)" >&2
  exit 1
fi
crds_tmp="${TMP_DIR}/crds.yaml"
cat manifests/ate-install/generated/*.yaml >"${crds_tmp}"
# Add a doc separator only if the CRD file does not open with one.
first_line="$(awk 'NF && $1 !~ /^#/ {print; exit}' "${SUBSTRATE_CRD}")"
if [[ "${first_line}" != "---" ]]; then
  printf '\n---\n' >>"${crds_tmp}"
fi
cat "${SUBSTRATE_CRD}" >>"${crds_tmp}"
mv "${crds_tmp}" "${OUT_DIR}/crds.yaml"

echo "rendered bundles in ${ROOT}/${OUT_DIR}:"
ls -l "${OUT_DIR}"/shared.yaml "${OUT_DIR}"/stack-*.yaml "${OUT_DIR}"/crds.yaml

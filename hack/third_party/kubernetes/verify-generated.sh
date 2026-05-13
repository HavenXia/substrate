#!/usr/bin/env bash

# Copyright 2014 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

if ! git diff HEAD --exit-code &>/dev/null; then
  echo
  echo "Unexpected dirty working directory:"
  git status -s
  echo
  echo "Please commit or stash these changes to run verification."
  exit 1
fi

name="$1"; shift
if [[ -z "${name}" ]]; then
  echo "Usage: $0 <name> [args...]" >&2
  echo "Example: $0 licenses" >&2
  exit 1
fi

# Work in a temporary worktree.
tmpdir="$(mktemp -d -t "verify-generated-${name}.XXXXXX")"
git worktree add -f -q "${tmpdir}" HEAD
trap "git worktree remove -f ${tmpdir:?}; rm -rf ${tmpdir:?}" EXIT
cd "${tmpdir}"

# Update generated files.
update_cmd="./hack/update/${name}.sh"
verify_cmd="./hack/verify/${name}.sh"
"${update_cmd}" "$@"

# Test for diffs
diffs=$(git status --porcelain | wc -l)
if [[ ${diffs} -gt 0 ]]; then
  echo "${verify_cmd} resulted in a diff:" >&2
  git status >&2
  git diff >&2
  echo
  echo "Run ${update_cmd} to update generated files." >&2
  exit 1
fi

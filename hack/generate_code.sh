#!/bin/bash

# Copyright 2026 Flant JSC
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

# Run from any directory; paths are resolved from the repository root.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
API_PATHS="${ROOT_DIR}/api/v1alpha1;${ROOT_DIR}/api/storage/v1alpha1"

# controller-gen is pinned in hack/tools/go.mod and built into a temporary
# directory. It used to be installed with `go install ...@v0.18.0` into
# $GOPATH/bin, which pins the version but writes outside the repository, leaves
# whatever was installed last on the developer's PATH and is not covered by any
# go.sum. The version now lives in one place for both generators.
TOOLS_DIR="$(mktemp -d)"
trap 'rm -rf "${TOOLS_DIR}"' EXIT

echo "Building controller-gen (pinned in hack/tools/go.mod)..."
go -C "${ROOT_DIR}/hack/tools" build -o "${TOOLS_DIR}" \
    sigs.k8s.io/controller-tools/cmd/controller-gen

echo "Generating deepcopy code..."
"${TOOLS_DIR}/controller-gen" object:headerFile="${ROOT_DIR}/hack/boilerplate.txt" paths="${API_PATHS}"

# The CRD manifests are rendered and then enriched; that pair lives in its own
# script because the shared .crds_generated_check CI job runs it by name.
echo "Generating CRD manifests..."
"${ROOT_DIR}/hack/generate_crds.sh"

echo "Deepcopy and CRD generation complete."

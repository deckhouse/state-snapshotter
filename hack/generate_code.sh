#!/bin/bash

# Copyright 2025 Flant JSC
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

# Keep in sync with api/README.md and CRD annotation controller-gen.kubebuilder.io/version.
CONTROLLER_GEN_VERSION=v0.18.0
CONTROLLER_GEN_BIN="$(go env GOPATH)/bin/controller-gen"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
API_DIR="${ROOT_DIR}/api/v1alpha1"

echo "Ensuring controller-gen ${CONTROLLER_GEN_VERSION}..."
go install "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}"

echo "Generating deepcopy code..."
"${CONTROLLER_GEN_BIN}" object:headerFile="${ROOT_DIR}/hack/boilerplate.txt" paths="${API_DIR}"

echo "Generating CRD manifests..."
"${CONTROLLER_GEN_BIN}" crd:crdVersions=v1 output:crd:dir="${ROOT_DIR}/crds" paths="${API_DIR}"

echo "Deepcopy and CRD generation complete."

#!/usr/bin/env bash

# Copyright 2018 The Kubernetes Authors.
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

# This script automates the download of binaries used by deployment
# and testing of KubeFed.

set -o errexit
set -o nounset
set -o pipefail

# Use DEBUG=1 ./scripts/download-binaries.sh to get debug output
curl_args="-fsSL"
[[ -z "${DEBUG:-""}" ]] || {
  set -x
  curl_args="-fL"
}

logEnd() {
  local msg='done.'
  [ "$1" -eq 0 ] || msg='Error downloading assets'
  echo "$msg"
}
trap 'logEnd $?' EXIT

echo "About to download some binaries. This might take a while..."

root_dir="$(cd "$(dirname "$0")/.." ; pwd)"
dest_dir="${root_dir}/bin"
mkdir -p "${dest_dir}"

platform=$(uname -s|tr A-Z a-z)

# helm
helm_version="3.3.4"
helm_tgz="helm-v${helm_version}-${platform}-amd64.tar.gz"
helm_url="https://get.helm.sh/$helm_tgz"
curl "${curl_args}" "${helm_url}" \
    | tar xzP -C "${dest_dir}" --strip-components=1 "${platform}-amd64/helm"

# kubectl
kubectl_version=v1.20.0
kubectl_path="${dest_dir}/kubectl"
kubectl_url=https://dl.k8s.io/release/v1.20.0/bin/${platform}/amd64/kubectl
curl -fLo "${kubectl_path}" "${kubectl_url}" && chmod +x "${kubectl_path}"
# kind
kind_version="v0.9.0"
kind_path="${dest_dir}/kind"
kind_url="https://github.com/kubernetes-sigs/kind/releases/download/${kind_version}/kind-${platform}-amd64"
curl -fLo "${kind_path}" "${kind_url}" && chmod +x "${kind_path}"

echo    "# destination:"
echo    "#   ${dest_dir}"
echo    "# versions:"
echo -n "#   kubectl:        "; "${dest_dir}/kubectl" version --client --short
echo -n "#   helm:           "; "${dest_dir}/helm" version --client --short
echo    "# kind installation:  ${kind_path}"

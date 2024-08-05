#!/usr/bin/env bash

# Copyright 2013 The Kubernetes Authors.
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

set -o errexit
set -o nounset
set -o pipefail

# This script creates and pushes new semver tags, according to https://github.com/kubernetes-sigs/apiserver-network-proxy/blob/master/RELEASE.md.
# Only repository admins can perform this; open an issue and assign to an approver at https://github.com/kubernetes-sigs/apiserver-network-proxy/blob/master/OWNERS.

# Example: assuming that branch release-0.0 exists, create and push tags v0.0.1 and konnectivity-client/v0.0.1
#
#     ./hack/push-release-tags.sh v0.0.1 "Fix outstanding CVEs."

VERSION="$1"
VERSION="${VERSION#[vV]}"
VERSION_MAJOR="${VERSION%%\.*}"
VERSION_MINOR="${VERSION#*.}"
VERSION_MINOR="${VERSION_MINOR%.*}"
VERSION_PATCH="${VERSION##*.}"

echo "Version: ${VERSION}"
echo "Version [major]: ${VERSION_MAJOR}"
echo "Version [minor]: ${VERSION_MINOR}"
echo "Version [patch]: ${VERSION_PATCH}"

MESSAGE="$2"
if [ -z ${MESSAGE+x} ]; then
    echo "MESSAGE is required."
    exit 1
else
    echo "MESSAGE is set to '$MESSAGE'"
fi

UPSTREAM_REMOTE=${UPSTREAM_REMOTE:-upstream}
echo "UPSTREAM_REMOTE is set to '$UPSTREAM_REMOTE'"

RELEASE_BRANCH="release-${VERSION_MAJOR}.${VERSION_MINOR}"
echo "Verifying branch."
if git ls-remote --exit-code --heads "${UPSTREAM_REMOTE}" "refs/heads/${RELEASE_BRANCH}" ; then
    echo "Branch '${RELEASE_BRANCH}' verified."
else
    echo "Branch '${RELEASE_BRANCH}' not found, exiting."
    exit 1
fi

TAG="v${VERSION_MAJOR}.${VERSION_MINOR}.${VERSION_PATCH}"
echo "Creating tag: ${TAG}"

if git ls-remote --exit-code "${UPSTREAM_REMOTE}" "refs/tags/${TAG}" ; then
    echo "Tag refs/tags/${TAG} is already pushed, exiting".
    exit 1
else
    echo "Will push tag ${TAG}".
fi

SIGN_OPTION=${SIGN_OPTION:-}
echo "SIGN_OPTION is set to '$SIGN_OPTION'"

git fetch "${UPSTREAM_REMOTE}"
git tag -a "${TAG}" ${SIGN_OPTION} -m "${MESSAGE}" "${UPSTREAM_REMOTE}/${RELEASE_BRANCH}"
git tag -a "konnectivity-client/${TAG}" ${SIGN_OPTION} -m "${MESSAGE}" "${UPSTREAM_REMOTE}/${RELEASE_BRANCH}"
git push "${UPSTREAM_REMOTE}" "${TAG}"
git push "${UPSTREAM_REMOTE}" "konnectivity-client/${TAG}"

echo "Finished."

#! /bin/bash

set -o pipefail
set -o nounset
set -o errexit

: "${KUBECONFIG:?}"

ROOT_DIR=$(dirname "${BASH_SOURCE[0]}")/..
MOD_FLAGS=${MOD_FLAGS:=""}

echo "Running e2e tests"
go test -v \
    ${MOD_FLAGS} \
    -failfast \
    -timeout 150m \
    "${ROOT_DIR}/test/e2e/..." \
    -namespace=openshift-operators \
    -kubeconfig="${KUBECONFIG}" \
    -olmNamespace=openshift-operator-lifecycle-manager \
    -dummyImage=bitnami/nginx:latest \
    -ginkgo.flakeAttempts=3

#! /bin/bash

set -o pipefail
set -o nounset
set -o errexit

: "${KUBECONFIG:?}"

ROOT_DIR=$(dirname "${BASH_SOURCE[0]}")/..
MOD_FLAGS=${MOD_FLAGS:=""}
GATHER_TEST_ARTIFACTS=${GATHER_TEST_ARTIFACTS:=false}

function gather_test_artifacts() {
    exit_status=$?

    if [[ ${GATHER_TEST_ARTIFACTS} = true ]]; then
        echo "Gathering test artifacts..."
        "${ROOT_DIR}/scripts/gather_test_artifacts.sh"
    fi

    exit "$exit_status"
}

trap gather_test_artifacts EXIT

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

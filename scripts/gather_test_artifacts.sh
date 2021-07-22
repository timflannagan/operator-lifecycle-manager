#! /bin/bash

set -o pipefail
set -o nounset
set -o errexit

# TODO(tflannag): Where should this be stored?
LOG_DIR="${LOG_DIR:=$PWD/must-gather}"
POD_LOG_PATH=${POD_LOG_PATH:="${LOG_DIR}/pod_logs"}
mkdir -p "${POD_LOG_PATH}"/

# General namespace resources
resources=()
resources+=(pods)
resources+=(deployments)
resources+=(statefulsets)
resources+=(services)
resources+=(serviceaccounts)
resources+=(subscriptions)
resources+=(operatorgroups)
resources+=(clusterserviceversions)
resources+=(installplans)


echo "Storing the must-gather output at $LOG_DIR"
for resource in "${resources[@]}"; do
    for namespace in $(kubectl get ns); do
        echo "Collecting dump of ${resource} in the ${namespace} namespace" | tee -a  "${LOG_DIR}"/gather-debug.log
        { timeout 120 oc adm --namespace "${namespace}" --dest-dir="${LOG_DIR}" inspect "${resource}"; } >> "${LOG_DIR}"/gather-debug.log 2>&1
    done
done

echo "Deleting any empty test artifact files" >> "${LOG_DIR}"/gather-debug.log
find "${LOG_DIR}" -empty -delete >> "${LOG_DIR}"/gather-debug.log 2>&1
exit 0

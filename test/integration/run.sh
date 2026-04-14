#!/usr/bin/env bash
# Copyright 2026 Open Defense and dependency-controller contributors
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

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
FIXTURES_DIR="${ROOT_DIR}/test/fixtures"

KIND_CLUSTER_NAME="dep-ctrl-integration"
KIND_NAMESPACE="dependency-system"
IMAGE_NAME="dependency-controller:integration-test"

CERT_MANAGER_VERSION="v1.17.2"

# kcp helm chart configuration.
KCP_NAMESPACE="kcp-system"
KCP_HOSTNAME="kcp.local"
KCP_CLUSTER_IP="10.96.100.1"
KCP_NODE_PORT="31500"
KCP_EXTERNAL_PORT="8443"

# Temp directory for kubeconfigs and certs.
TMP_DIR=""

# Host kubeconfig for talking to kcp from the test script.
KCP_HOST_KUBECONFIG=""

# Workspace names (children of root).
WS_DEP_CTRL="dep-ctrl"
WS_NETWORK_PROVIDER="network-provider"
WS_COMPUTE_PROVIDER="compute-provider"
WS_CONSUMER1="consumer1"
WS_CONSUMER2="consumer2"

# --------------------------------------------------------------------------- #
#  Helpers                                                                     #
# --------------------------------------------------------------------------- #

info()  { echo "==> $*"; }
ok()    { echo "  ✓ $*"; }
fail()  { echo "  ✗ $*" >&2; return 1; }

# kubectl wrapper that targets the kcp API at a specific workspace path.
kcpctl() {
    local ws_path="$1"; shift
    kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" \
        --server "https://localhost:${KCP_NODE_PORT}/clusters/root:${ws_path}" \
        "$@"
}

# kubectl wrapper for the kcp root workspace.
kcpctl_root() {
    kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" \
        --server "https://localhost:${KCP_NODE_PORT}/clusters/root" \
        "$@"
}

# kubectl wrapper for the kind cluster.
kindctl() {
    kubectl --context "kind-${KIND_CLUSTER_NAME}" "$@"
}

# Wait for a condition with a timeout. Usage: wait_for <timeout_seconds> <description> <command...>
wait_for() {
    local timeout="$1" desc="$2"; shift 2
    local deadline=$((SECONDS + timeout))
    while ! "$@" 2>/dev/null; do
        if ((SECONDS >= deadline)); then
            fail "timed out waiting for: ${desc}"
        fi
        sleep 1
    done
    ok "${desc}"
}

# Apply a YAML fixture to a kcp workspace, substituting placeholders.
apply_fixture_to_ws() {
    local ws="$1" file="$2"; shift 2
    local content
    content="$(cat "${file}")"
    for subst in "$@"; do
        local key="${subst%%=*}"
        local val="${subst#*=}"
        content="${content//\$\{${key}\}/${val}}"
    done
    echo "${content}" | kcpctl "${ws}" apply -f -
}

# --------------------------------------------------------------------------- #
#  Cleanup                                                                     #
# --------------------------------------------------------------------------- #

cleanup() {
    info "Cleaning up"

    if kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
        kind delete cluster --name "${KIND_CLUSTER_NAME}" 2>/dev/null || true
    fi

    if [[ -n "${TMP_DIR}" && -d "${TMP_DIR}" ]]; then
        rm -rf "${TMP_DIR}"
    fi
}

# --------------------------------------------------------------------------- #
#  Phase 1: Infrastructure                                                     #
# --------------------------------------------------------------------------- #

create_kind_cluster() {
    info "Creating kind cluster '${KIND_CLUSTER_NAME}'"

    if kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
        info "Kind cluster already exists, reusing"
        return
    fi

    kind create cluster \
        --name "${KIND_CLUSTER_NAME}" \
        --config "${FIXTURES_DIR}/kind-config.yaml" \
        --wait 60s
}

install_cert_manager() {
    info "Installing cert-manager ${CERT_MANAGER_VERSION}"

    kindctl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"

    wait_for 120 "cert-manager ready" \
        kindctl -n cert-manager wait deployment cert-manager-webhook \
        --for=condition=Available --timeout=1s

    # Create self-signed ClusterIssuer.
    kindctl apply -f "${FIXTURES_DIR}/cert-manager-selfsigned-issuer.yaml"
}

deploy_kcp() {
    info "Deploying kcp via helm chart"

    helm repo add kcp https://kcp-dev.github.io/helm-charts 2>/dev/null || true
    helm repo update kcp

    helm upgrade --install kcp kcp/kcp \
        --namespace "${KCP_NAMESPACE}" \
        --create-namespace \
        --values "${FIXTURES_DIR}/integration-values-kcp.yaml" \
        --wait --timeout 300s

    ok "kcp deployed"
}

patch_coredns() {
    info "Patching CoreDNS to resolve ${KCP_HOSTNAME}"

    # Add a dedicated server block for kcp.local that resolves to the
    # front-proxy ClusterIP.  Prepend it to the existing Corefile so
    # all pods in the kind cluster can reach kcp.
    local existing
    existing=$(kindctl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')

    local kcp_block
    kcp_block="$(cat <<EOF
${KCP_HOSTNAME}:53 {
    errors
    cache 30
    hosts {
        ${KCP_CLUSTER_IP} ${KCP_HOSTNAME}
        fallthrough
    }
}

EOF
)"

    local new_corefile="${kcp_block}${existing}"

    kindctl -n kube-system create configmap coredns \
        --from-literal="Corefile=${new_corefile}" \
        --dry-run=client -o yaml | kindctl apply -f -

    kindctl -n kube-system rollout restart deployment coredns
    wait_for 30 "CoreDNS restarted" \
        kindctl -n kube-system rollout status deployment coredns --timeout=1s

    ok "CoreDNS patched"
}

build_admin_kubeconfig() {
    info "Building kcp admin kubeconfig"

    TMP_DIR="$(mktemp -d)"
    KCP_HOST_KUBECONFIG="${TMP_DIR}/kcp-host.kubeconfig"

    # Issue an admin client certificate via cert-manager.
    kindctl apply -f "${FIXTURES_DIR}/kcp-admin-cert.yaml"
    wait_for 60 "admin client cert issued" \
        kindctl -n "${KCP_NAMESPACE}" get secret kcp-admin-client-cert -o jsonpath='{.data.tls\.crt}'

    # Extract the front-proxy CA certificate.
    kindctl -n "${KCP_NAMESPACE}" get secret kcp-front-proxy-cert \
        -o jsonpath='{.data.tls\.crt}' | base64 -d > "${TMP_DIR}/ca.crt"

    # Extract admin client certificate and key.
    kindctl -n "${KCP_NAMESPACE}" get secret kcp-admin-client-cert \
        -o jsonpath='{.data.tls\.crt}' | base64 -d > "${TMP_DIR}/client.crt"
    kindctl -n "${KCP_NAMESPACE}" get secret kcp-admin-client-cert \
        -o jsonpath='{.data.tls\.key}' | base64 -d > "${TMP_DIR}/client.key"

    # Build a kubeconfig for host access (via NodePort on localhost).
    # Use --insecure-skip-tls-verify since NodePort goes through localhost
    # but the cert SAN may only include kcp.local.
    kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" config set-cluster kcp \
        --server="https://localhost:${KCP_NODE_PORT}" \
        --certificate-authority="${TMP_DIR}/ca.crt" \
        --embed-certs=true
    kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" config set-credentials kcp-admin \
        --client-certificate="${TMP_DIR}/client.crt" \
        --client-key="${TMP_DIR}/client.key" \
        --embed-certs=true
    kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" config set-context kcp \
        --cluster=kcp --user=kcp-admin
    kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" config use-context kcp

    # Verify connectivity.
    wait_for 30 "kcp API reachable" \
        kubectl --kubeconfig "${KCP_HOST_KUBECONFIG}" \
            --server "https://localhost:${KCP_NODE_PORT}" get --raw /readyz

    ok "admin kubeconfig ready"
}

build_and_load_image() {
    info "Building image '${IMAGE_NAME}'"
    docker build -t "${IMAGE_NAME}" "${ROOT_DIR}"

    info "Loading image into kind"
    kind load docker-image "${IMAGE_NAME}" --name "${KIND_CLUSTER_NAME}"
}

# --------------------------------------------------------------------------- #
#  Phase 2: kcp workspace topology                                             #
# --------------------------------------------------------------------------- #

setup_kcp_workspaces() {
    info "Setting up kcp workspaces"

    # Create all workspaces under root.
    for ws in "${WS_DEP_CTRL}" "${WS_NETWORK_PROVIDER}" "${WS_COMPUTE_PROVIDER}" "${WS_CONSUMER1}" "${WS_CONSUMER2}"; do
        kcpctl_root create -f - <<EOF || true
apiVersion: tenancy.kcp.io/v1alpha1
kind: Workspace
metadata:
  name: ${ws}
EOF
    done

    # Wait for workspaces to be ready.
    for ws in "${WS_DEP_CTRL}" "${WS_NETWORK_PROVIDER}" "${WS_COMPUTE_PROVIDER}" "${WS_CONSUMER1}" "${WS_CONSUMER2}"; do
        wait_for 60 "workspace ${ws} ready" \
            test "$(kcpctl_root get workspace "${ws}" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Ready"
    done

    # Apply dep-ctrl APIResourceSchemas and APIExport.
    kcpctl "${WS_DEP_CTRL}" apply -f "${ROOT_DIR}/config/kcp/apiresourceschema-dependencyrules.dependencies.opendefense.cloud.yaml"
    kcpctl "${WS_DEP_CTRL}" apply -f "${ROOT_DIR}/config/kcp/apiexport-dependencies.opendefense.cloud.yaml"

    # Apply test provider schemas and exports.
    kcpctl "${WS_NETWORK_PROVIDER}" apply -f "${FIXTURES_DIR}/apiresourceschema-vpcs.yaml"
    kcpctl "${WS_NETWORK_PROVIDER}" apply -f "${FIXTURES_DIR}/apiexport-network.test.io.yaml"
    kcpctl "${WS_COMPUTE_PROVIDER}" apply -f "${FIXTURES_DIR}/apiresourceschema-virtualmachines.yaml"
    kcpctl "${WS_COMPUTE_PROVIDER}" apply -f "${FIXTURES_DIR}/apiexport-compute.test.io.yaml"

    # Create APIBindings — providers bind to dep-ctrl.
    local dep_ctrl_path="root:${WS_DEP_CTRL}"
    local network_path="root:${WS_NETWORK_PROVIDER}"
    local compute_path="root:${WS_COMPUTE_PROVIDER}"

    apply_fixture_to_ws "${WS_COMPUTE_PROVIDER}" "${FIXTURES_DIR}/apibinding-dependencies.opendefense.cloud.yaml" \
        "DEP_CTRL_PATH=${dep_ctrl_path}"
    apply_fixture_to_ws "${WS_NETWORK_PROVIDER}" "${FIXTURES_DIR}/apibinding-dependencies.opendefense.cloud.yaml" \
        "DEP_CTRL_PATH=${dep_ctrl_path}"

    # Consumers bind to network and compute.
    for consumer in "${WS_CONSUMER1}" "${WS_CONSUMER2}"; do
        apply_fixture_to_ws "${consumer}" "${FIXTURES_DIR}/apibinding-network.test.io.yaml" \
            "NETWORK_PROVIDER_PATH=${network_path}"
        apply_fixture_to_ws "${consumer}" "${FIXTURES_DIR}/apibinding-compute.test.io.yaml" \
            "COMPUTE_PROVIDER_PATH=${compute_path}"
    done

    # Wait for all bindings to be bound.
    for ws_binding in \
        "${WS_COMPUTE_PROVIDER}/dependencies.opendefense.cloud" \
        "${WS_NETWORK_PROVIDER}/dependencies.opendefense.cloud" \
        "${WS_CONSUMER1}/network.test.io" "${WS_CONSUMER1}/compute.test.io" \
        "${WS_CONSUMER2}/network.test.io" "${WS_CONSUMER2}/compute.test.io"; do
        local ws="${ws_binding%%/*}"
        local binding="${ws_binding##*/}"
        wait_for 60 "binding ${binding} in ${ws} bound" \
            test "$(kcpctl "${ws}" get apibinding "${binding}" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Bound"
    done

    # Wait for APIExportEndpointSlices to have endpoints.
    for ws_ep in \
        "${WS_DEP_CTRL}/dependencies.opendefense.cloud" \
        "${WS_NETWORK_PROVIDER}/network.test.io" \
        "${WS_COMPUTE_PROVIDER}/compute.test.io"; do
        local ws="${ws_ep%%/*}"
        local ep="${ws_ep##*/}"
        wait_for 60 "endpoints for ${ep} in ${ws}" \
            test -n "$(kcpctl "${ws}" get apiexportendpointslice "${ep}" -o jsonpath='{.status.apiExportEndpoints[0].url}' 2>/dev/null)"
    done

    ok "kcp workspace topology ready"
}

# --------------------------------------------------------------------------- #
#  Phase 3: Deploy helm charts                                                 #
# --------------------------------------------------------------------------- #

deploy_charts() {
    info "Creating namespace and kcp kubeconfig secret"

    kindctl create namespace "${KIND_NAMESPACE}" 2>/dev/null || true

    # Build a kubeconfig for pods that points to the in-cluster kcp front-proxy.
    # Pods resolve kcp.local via the CoreDNS entry patched earlier.
    local kcp_internal_server="https://${KCP_HOSTNAME}:${KCP_EXTERNAL_PORT}/clusters/root:${WS_DEP_CTRL}"

    local internal_kubeconfig="${TMP_DIR}/kcp-internal.kubeconfig"
    kubectl --kubeconfig "${internal_kubeconfig}" config set-cluster kcp \
        --server="${kcp_internal_server}" \
        --certificate-authority="${TMP_DIR}/ca.crt" \
        --embed-certs=true
    kubectl --kubeconfig "${internal_kubeconfig}" config set-credentials kcp-admin \
        --client-certificate="${TMP_DIR}/client.crt" \
        --client-key="${TMP_DIR}/client.key" \
        --embed-certs=true
    kubectl --kubeconfig "${internal_kubeconfig}" config set-context kcp \
        --cluster=kcp --user=kcp-admin
    kubectl --kubeconfig "${internal_kubeconfig}" config use-context kcp

    kindctl -n "${KIND_NAMESPACE}" create secret generic kcp-kubeconfig \
        --from-file=kubeconfig="${internal_kubeconfig}" \
        --dry-run=client -o yaml | kindctl apply -f -

    info "Deploying webhook chart"
    helm upgrade --install dep-webhook "${ROOT_DIR}/charts/dependency-webhook" \
        --namespace "${KIND_NAMESPACE}" \
        --values "${FIXTURES_DIR}/integration-values-webhook.yaml" \
        --wait --timeout 120s

    info "Deploying controller chart"
    helm upgrade --install dep-ctrl "${ROOT_DIR}/charts/dependency-controller" \
        --namespace "${KIND_NAMESPACE}" \
        --values "${FIXTURES_DIR}/integration-values-controller.yaml" \
        --wait --timeout 120s

    ok "helm charts deployed"
}

# --------------------------------------------------------------------------- #
#  Phase 4: Integration tests                                                  #
# --------------------------------------------------------------------------- #

run_tests() {
    info "Running integration tests"

    local network_path="root:${WS_NETWORK_PROVIDER}"
    local compute_path="root:${WS_COMPUTE_PROVIDER}"

    # ------------------------------------------------------------------ #
    test_create_dependency_rule "${compute_path}" "${network_path}"
    test_block_deletion
    test_consumer2_unaffected
    test_allow_after_dependent_deleted
    test_rule_deletion "${compute_path}"
    # ------------------------------------------------------------------ #

    echo ""
    info "All integration tests passed!"
}

test_create_dependency_rule() {
    local compute_path="$1" network_path="$2"
    info "Test: create DependencyRule and verify webhook installation"

    # Create VPC in consumer1.
    apply_fixture_to_ws "${WS_CONSUMER1}" "${FIXTURES_DIR}/vpc-my-vpc.yaml"

    # Create DependencyRule in compute-provider.
    apply_fixture_to_ws "${WS_COMPUTE_PROVIDER}" "${FIXTURES_DIR}/dependencyrule-vm-dependencies.yaml" \
        "COMPUTE_PROVIDER_PATH=${compute_path}" \
        "NETWORK_PROVIDER_PATH=${network_path}"

    # Create VM referencing the VPC in consumer1.
    apply_fixture_to_ws "${WS_CONSUMER1}" "${FIXTURES_DIR}/vm-my-vm.yaml"

    # Wait for the ValidatingWebhookConfiguration to appear in the network-provider workspace.
    wait_for 60 "webhook installed in network-provider" \
        kcpctl "${WS_NETWORK_PROVIDER}" get validatingwebhookconfiguration dependency-controller

    ok "DependencyRule created and webhook installed"
}

test_block_deletion() {
    info "Test: VPC deletion is blocked while VM references it"

    # Retry: wait for the indexed cache to sync and the webhook to start blocking.
    wait_for 60 "webhook blocks VPC deletion" check_deletion_blocked

    ok "VPC deletion correctly blocked"
}

check_deletion_blocked() {
    local output
    output="$(kcpctl "${WS_CONSUMER1}" delete vpc my-vpc --namespace default 2>&1)" && return 1
    echo "${output}" | grep -q "still referenced by"
}

test_consumer2_unaffected() {
    info "Test: consumer2 is not affected (no VMs referencing VPCs)"

    apply_fixture_to_ws "${WS_CONSUMER2}" "${FIXTURES_DIR}/vpc-isolated-vpc.yaml"

    # Deletion should succeed (no dependents in consumer2).
    wait_for 30 "VPC deletion succeeds in consumer2" \
        kcpctl "${WS_CONSUMER2}" delete vpc isolated-vpc --namespace default

    ok "consumer2 unaffected"
}

test_allow_after_dependent_deleted() {
    info "Test: VPC deletion allowed after VM is deleted"

    kcpctl "${WS_CONSUMER1}" delete virtualmachine my-vm --namespace default

    # Wait for the indexed cache to reflect the deletion.
    wait_for 60 "VPC deletion allowed after VM removal" \
        kcpctl "${WS_CONSUMER1}" delete vpc my-vpc --namespace default

    ok "VPC deletion allowed after dependent removed"
}

test_rule_deletion() {
    local compute_path="$1"
    info "Test: webhook removed after DependencyRule deletion"

    # Set up: create VPC + VM + verify blocking.
    apply_fixture_to_ws "${WS_CONSUMER1}" "${FIXTURES_DIR}/vpc-cleanup-vpc.yaml"
    apply_fixture_to_ws "${WS_CONSUMER1}" "${FIXTURES_DIR}/vm-cleanup-vm.yaml"
    wait_for 60 "webhook blocks cleanup-vpc deletion" check_cleanup_vpc_blocked

    # Delete the DependencyRule.
    kcpctl "${WS_COMPUTE_PROVIDER}" delete dependencyrule vm-dependencies

    # Wait for the webhook to be removed from the network-provider workspace.
    wait_for 60 "webhook removed from network-provider" \
        check_webhook_removed

    # VPC deletion should now succeed.
    kcpctl "${WS_CONSUMER1}" delete vpc cleanup-vpc --namespace default
    kcpctl "${WS_CONSUMER1}" delete virtualmachine cleanup-vm --namespace default 2>/dev/null || true

    ok "webhook correctly removed after DependencyRule deletion"
}

check_cleanup_vpc_blocked() {
    local output
    output="$(kcpctl "${WS_CONSUMER1}" delete vpc cleanup-vpc --namespace default 2>&1)" && {
        # Deletion succeeded — recreate and retry.
        apply_fixture_to_ws "${WS_CONSUMER1}" "${FIXTURES_DIR}/vpc-cleanup-vpc.yaml"
        return 1
    }
    echo "${output}" | grep -q "still referenced by"
}

check_webhook_removed() {
    ! kcpctl "${WS_NETWORK_PROVIDER}" get validatingwebhookconfiguration dependency-controller 2>/dev/null
}

# --------------------------------------------------------------------------- #
#  Main                                                                        #
# --------------------------------------------------------------------------- #

main() {
    if [[ "${1:-}" == "cleanup" ]]; then
        cleanup
        exit 0
    fi

    trap cleanup EXIT

    create_kind_cluster
    install_cert_manager
    deploy_kcp
    patch_coredns
    build_admin_kubeconfig
    build_and_load_image
    setup_kcp_workspaces
    deploy_charts
    run_tests
}

main "$@"

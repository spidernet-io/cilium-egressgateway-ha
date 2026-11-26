#!/usr/bin/env bash
# Creates the kind cluster on its own docker network and installs Cilium with
# the egress gateway enabled. Cilium defaults to the latest chart version;
# set CILIUM_VERSION to pin one.
set -euo pipefail

: "${E2E_KIND_CLUSTER:?}"
: "${E2E_KUBECONFIG:?}"
CILIUM_VERSION="${CILIUM_VERSION:-}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"

if kind get clusters 2>/dev/null | grep -qx "${E2E_KIND_CLUSTER}"; then
  echo "kind cluster ${E2E_KIND_CLUSTER} already exists"
else
  # A dedicated network keeps the egress IPs off other clusters' L2 segment.
  export KIND_EXPERIMENTAL_DOCKER_NETWORK="${E2E_KIND_CLUSTER}"
  kind create cluster --name "${E2E_KIND_CLUSTER}" --config "${DIR}/kind/config.yaml" \
    ${KIND_NODE_IMAGE:+--image "${KIND_NODE_IMAGE}"} --wait 0s
fi
kind get kubeconfig --name "${E2E_KIND_CLUSTER}" > "${E2E_KUBECONFIG}"
export KUBECONFIG="${E2E_KUBECONFIG}"

if helm -n kube-system status cilium >/dev/null 2>&1; then
  echo "cilium already installed"
else
  helm install cilium cilium --repo https://helm.cilium.io -n kube-system \
    ${CILIUM_VERSION:+--version "${CILIUM_VERSION}"} \
    --set k8sServiceHost="${E2E_KIND_CLUSTER}-control-plane" \
    --set k8sServicePort=6443 \
    --set kubeProxyReplacement=true \
    --set routingMode=tunnel \
    --set tunnelProtocol=vxlan \
    --set ipam.mode=kubernetes \
    --set bpf.masquerade=true \
    --set egressGateway.enabled=true \
    --set operator.replicas=1
fi
echo "cilium chart: $(helm -n kube-system list -f '^cilium$' -o json | python3 -c 'import sys,json; print(json.load(sys.stdin)[0]["chart"])')"
kubectl -n kube-system rollout status ds/cilium --timeout=10m
kubectl wait --for=condition=Ready nodes --all --timeout=5m

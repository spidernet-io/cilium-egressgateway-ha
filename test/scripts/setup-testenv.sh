#!/usr/bin/env bash
# Prepares the test environment on the kind cluster: egress node labels, the
# external target container and the client pod. Writes the variables used by
# the tests and the install script to E2E_ENV_FILE.
set -euo pipefail

: "${E2E_KIND_CLUSTER:?}"
: "${E2E_KUBECONFIG:?}"
: "${E2E_ENV_FILE:?}"
AGNHOST_IMAGE="${AGNHOST_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.53}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
export KUBECONFIG="${E2E_KUBECONFIG}"

CP="${E2E_KIND_CLUSTER}-control-plane"
W1="${E2E_KIND_CLUSTER}-worker"
W2="${E2E_KIND_CLUSTER}-worker2"
W3="${E2E_KIND_CLUSTER}-worker3"
TARGET="${E2E_KIND_CLUSTER}-target"

# Pick the target and egress IPs from the end of the IPv4 subnet of the kind
# network, away from the addresses docker assigns to containers.
read -r TARGET_IP EGRESS_START EGRESS_END < <(docker network inspect "${E2E_KIND_CLUSTER}" \
  -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | python3 -c '
import ipaddress, sys
net = next(n for n in map(ipaddress.ip_network, sys.stdin.read().split()) if n.version == 4)
print(net.broadcast_address - 5, net.broadcast_address - 60, net.broadcast_address - 50)')
CP_IP="$(docker inspect -f "{{(index .NetworkSettings.Networks \"${E2E_KIND_CLUSTER}\").IPAddress}}" "${CP}")"

docker pull -q "${AGNHOST_IMAGE}" >/dev/null
if ! docker inspect "${TARGET}" >/dev/null 2>&1; then
  docker run -d --name "${TARGET}" --network "${E2E_KIND_CLUSTER}" --ip "${TARGET_IP}" \
    "${AGNHOST_IMAGE}" netexec --http-port=8080 >/dev/null
fi

kubectl label node "${CP}" "${W1}" "${W2}" egressgateway-ha.spidernet.io/egress-node=true --overwrite
sed -e "s|@AGNHOST_IMAGE@|${AGNHOST_IMAGE}|" -e "s|@NODE@|${W3}|" "${DIR}/yamls/client.yaml" | kubectl apply -f -
kubectl wait --for=condition=Ready pod/client --timeout=5m

cat > "${E2E_ENV_FILE}" <<ENV
KUBECONFIG=${E2E_KUBECONFIG}
E2E_KIND_CLUSTER=${E2E_KIND_CLUSTER}
E2E_CONTROL_PLANE=${CP}
E2E_CONTROL_PLANE_IP=${CP_IP}
E2E_WORKER1=${W1}
E2E_WORKER2=${W2}
E2E_CLIENT_NODE=${W3}
E2E_TARGET_ADDR=${TARGET_IP}:8080
E2E_EGRESS_IP_RANGE=${EGRESS_START}-${EGRESS_END}
E2E_NAMESPACE=${E2E_NAMESPACE:-kube-system}
ENV
echo "wrote ${E2E_ENV_FILE}:"; cat "${E2E_ENV_FILE}"

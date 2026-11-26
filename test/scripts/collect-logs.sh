#!/usr/bin/env bash
# Collects debugging information into OUTPUT_DIR after a failed run.
set -uo pipefail

: "${E2E_ENV_FILE:?}" "${OUTPUT_DIR:?}"
set -a; . "${E2E_ENV_FILE}"; set +a
mkdir -p "${OUTPUT_DIR}"
cd "${OUTPUT_DIR}"

kubectl get nodes -o wide --show-labels > nodes.txt 2>&1
kubectl get ciliumnodes -o yaml > ciliumnodes.yaml 2>&1
kubectl get egressgatewayha -o yaml > egressgatewayha.yaml 2>&1
kubectl get ciliumegressgatewaypolicies -o yaml > policies.yaml 2>&1
kubectl get pods -A -o wide > pods.txt 2>&1
kubectl get events.events.k8s.io -A --sort-by=.metadata.creationTimestamp > events.txt 2>&1
for p in $(kubectl -n "${E2E_NAMESPACE}" get pods -l app.kubernetes.io/instance=egwha -o name 2>/dev/null); do
  kubectl -n "${E2E_NAMESPACE}" logs "$p" --all-containers > "logs-${p#pod/}.txt" 2>&1
  # Only exists after a container restart.
  if prev="$(kubectl -n "${E2E_NAMESPACE}" logs "$p" --all-containers --previous 2>/dev/null)"; then
    echo "$prev" > "logs-${p#pod/}-previous.txt"
  fi
done
for p in $(kubectl -n kube-system get pods -l k8s-app=cilium -o name 2>/dev/null); do
  kubectl -n kube-system logs "$p" -c cilium-agent > "logs-${p#pod/}.txt" 2>&1
done
for n in $(kind get nodes --name "${E2E_KIND_CLUSTER}"); do
  { docker exec "$n" ip -4 addr; docker exec "$n" ip neigh; docker exec "$n" iptables -S; } > "node-${n}.txt" 2>&1
done
kind export logs --name "${E2E_KIND_CLUSTER}" kind-logs >/dev/null 2>&1 || true
echo "logs collected in ${OUTPUT_DIR}"

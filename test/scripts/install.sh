#!/usr/bin/env bash
# Loads the controller and agent images into kind and installs the chart.
set -euo pipefail

: "${E2E_ENV_FILE:?}"
: "${REGISTRY:?}" "${REPOSITORY:?}" "${TAG:?}"
set -a; . "${E2E_ENV_FILE}"; set +a
DIR="$(cd "$(dirname "$0")/.." && pwd)"
CHART="${DIR}/../charts/cilium-egressgateway-ha"

kind load docker-image --name "${E2E_KIND_CLUSTER}" \
  "${REGISTRY}/${REPOSITORY}/controller:${TAG}" "${REGISTRY}/${REPOSITORY}/agent:${TAG}"
# helm install/upgrade never updates CRDs under crds/.
kubectl apply --server-side --force-conflicts -f "${CHART}/crds"
helm upgrade --install egwha "${CHART}" -n "${E2E_NAMESPACE}" -f "${DIR}/yamls/values.yaml" \
  --set "egressGatewayHA.egressIPs={${E2E_EGRESS_IP_RANGE}}" \
  --set controller.image.registry="${REGISTRY}" --set controller.image.repository="${REPOSITORY}/controller" \
  --set controller.image.tag="${TAG}" --set controller.image.pullPolicy=Never \
  --set agent.image.registry="${REGISTRY}" --set agent.image.repository="${REPOSITORY}/agent" \
  --set agent.image.tag="${TAG}" --set agent.image.pullPolicy=Never
# Pick up a rebuilt image with the same tag.
kubectl -n "${E2E_NAMESPACE}" rollout restart ds/egwha-agent deploy/egwha-controller
kubectl -n "${E2E_NAMESPACE}" rollout status ds/egwha-agent --timeout=5m
kubectl -n "${E2E_NAMESPACE}" rollout status deploy/egwha-controller --timeout=5m

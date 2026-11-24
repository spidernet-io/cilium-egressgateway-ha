# cilium-egressgateway-ha

High availability for the Cilium community Egress Gateway. See the [design doc](docs/design.md).

## Install

```bash
helm install egwha charts/cilium-egressgateway-ha -n kube-system \
  --set global.egressNodeSelector.matchLabels.node-role\.example\.com/egress=true \
  --set egressGatewayHA.interface=eth0 \
  --set 'egressGatewayHA.egressIPs={192.168.10.100-192.168.10.110}'
kubectl get egressgatewayha
```

Then create `CiliumEgressGatewayPolicy` objects whose `egressGateway.nodeSelector` is
`matchLabels: {egress-gw-active: "true"}`. See the [design doc](docs/design.md) for
the full values and behavior.

## Development

```bash
make generate   # regenerate deepcopy and CRD (charts/cilium-egressgateway-ha/crds)
make build
make test       # unit tests; netops tests need root and run in an isolated netns
make image      # build binaries and the controller and agent images
KUBECONFIG=<kind kubeconfig> make e2e   # load the image into kind "egw-ha", install the chart (controller + agent), run test/e2e
```

The e2e tests expect the kind cluster `egw-ha`: candidate nodes labelled
`egress-gw-candidate=true`, a `client` pod (agnhost) on a non-candidate node and
an agnhost `netexec` target at `172.18.0.6:8080`. The tests are disruptive:
they kill and restart a worker container, stop kubelet, stop the API server for about a
minute and partition a worker with iptables. Use a dedicated cluster.

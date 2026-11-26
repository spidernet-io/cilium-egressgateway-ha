# cilium-egressgateway-ha

High availability for the Cilium community Egress Gateway. See the [design doc](docs/design.md).

- [Get started](docs/get-started.md): install, create a managed policy and switch the active node.
- [Metrics](docs/metrics.md): metrics of the controller and agent, and alert examples.
- [Run e2e tests locally](docs/e2e-test.md)

## Install

```bash
kubectl label node <egress nodes> egressgateway-ha.spidernet.io/egress-node=true
helm repo add egressgateway-ha https://spidernet-io.github.io/cilium-egressgateway-ha
helm install egwha egressgateway-ha/cilium-egressgateway-ha -n kube-system \
  --set 'egressGatewayHA.egressIPs={192.168.10.100-192.168.10.110}'
kubectl get egressgatewayha
```

Then create `CiliumEgressGatewayPolicy` objects whose `egressGateway.nodeSelector` is
`matchLabels: {egressgateway-ha.spidernet.io/active-node: "true"}`. See [Get started](docs/get-started.md).

## Development

```bash
make generate   # regenerate deepcopy and CRD (charts/cilium-egressgateway-ha/crds)
make build
make test       # unit tests; netops tests need root and run in an isolated netns
make image      # build the controller and agent images
```

### E2E tests

The e2e tests are [Ginkgo](https://onsi.github.io/ginkgo/) suites under `test/e2e`, one
directory per area: `crd`, `webhook`, `switchover`, `failover` and `reliability`. They run on
every pull request in GitHub Actions, and locally with docker, kind and helm:

```bash
make e2e          # all of the steps below
make e2e-cluster  # kind cluster "egw-e2e" on its own docker network, latest Cilium, client pod and target
make e2e-deploy   # build the images of the current tree, load them into kind and install the chart
make e2e-test     # run the suites with ginkgo, e.g. E2E_LABEL='!disruptive'
make e2e-logs     # collect logs into .e2e/logs
make e2e-clean    # delete the cluster
```

Cilium uses the latest chart version unless `CILIUM_VERSION` is set. The `failover` and
`reliability` suites are labelled `disruptive`: they kill and restart a node container,
stop kubelet, stop the API server for about a minute and partition a node with iptables.
kind may need `fs.inotify.max_user_instances=512` on the host.

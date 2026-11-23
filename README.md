# cilium-egressgateway-ha

High availability for the Cilium community Egress Gateway. See the [design doc](docs/design.md).

## Development

```bash
make generate   # regenerate deepcopy and CRD (charts/cilium-egressgateway-ha/crds)
make build
make test       # unit tests; netops tests need root and run in an isolated netns
make image      # build binaries and the image
KUBECONFIG=<kind kubeconfig> make e2e   # load the image into kind "egw-ha", install the chart, run test/e2e
```

The e2e tests expect the kind cluster `egw-ha`: candidate nodes labelled
`egress-gw-candidate=true`, a `client` pod (agnhost) on a non-candidate node and
an agnhost `netexec` target at `172.18.0.6:8080`.

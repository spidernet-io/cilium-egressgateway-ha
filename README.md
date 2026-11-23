# cilium-egressgateway-ha

High availability for the Cilium community Egress Gateway. See the [design doc](docs/design.md).

## Development

```bash
make generate   # regenerate deepcopy and CRD (config/crd)
make build
make test       # unit tests
KUBECONFIG=<kind kubeconfig> make e2e   # installs the CRD and runs test/e2e
```

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
KUBECTL ?= kubectl

.PHONY: all
all: generate build test

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd

.PHONY: build
build:
	go build ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test ./...

# Runs against the cluster in $KUBECONFIG, e.g. KUBECONFIG=$(kind get kubeconfig --name egw-ha).
.PHONY: e2e
e2e:
	$(KUBECTL) apply --server-side -f config/crd
	$(KUBECTL) wait --for=condition=Established crd/egressgatewayhas.egressgateway-ha.spidernet.io --timeout=60s
	go test -tags e2e -count=1 -v ./test/e2e/...

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
KUBECTL ?= kubectl
HELM ?= helm
KIND ?= kind

CHART := charts/cilium-egressgateway-ha
ARCH ?= amd64
REGISTRY ?= ghcr.io
REPOSITORY ?= spidernet-io/cilium-egressgateway-ha
COMPONENTS := controller agent
TAG ?= dev
E2E_KIND_CLUSTER ?= egw-e2e
E2E_DIR ?= $(CURDIR)/.e2e
E2E_KUBECONFIG ?= $(E2E_DIR)/kubeconfig
E2E_ENV_FILE ?= $(E2E_DIR)/env
# Ginkgo label filter, e.g. E2E_LABEL='!disruptive'.
E2E_LABEL ?=
E2E_TIMEOUT ?= 90m

.PHONY: all
all: generate build test

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=$(CHART)/crds

.PHONY: build
build:
	go build ./...

.PHONY: bin
bin:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o bin/linux_$(ARCH)/ ./cmd/...

# Builds $(REGISTRY)/$(REPOSITORY)/controller:$(TAG) and .../agent:$(TAG).
.PHONY: image
image: bin
	for c in $(COMPONENTS); do \
		docker build --build-arg TARGETARCH=$(ARCH) --build-arg COMPONENT=$$c \
			-t $(REGISTRY)/$(REPOSITORY)/$$c:$(TAG) . || exit 1; \
	done

.PHONY: vet
vet:
	go vet ./...
	go vet -tags e2e ./test/...

# netops tests run in an isolated netns when executed as root, and are skipped otherwise.
.PHONY: test
test:
	go test ./...

.PHONY: lint-chart
lint-chart:
	$(HELM) lint $(CHART) --set global.egressNodeSelector.matchLabels.a=b --set 'egressGatewayHA.egressIPs={192.0.2.1}'

export E2E_KIND_CLUSTER E2E_KUBECONFIG E2E_ENV_FILE

# Creates the kind cluster with Cilium and prepares the test environment.
.PHONY: e2e-cluster
e2e-cluster:
	mkdir -p $(E2E_DIR)
	test/scripts/setup-cluster.sh
	test/scripts/setup-testenv.sh

# Builds the images of the current tree and installs the chart.
.PHONY: e2e-deploy
e2e-deploy: image
	REGISTRY=$(REGISTRY) REPOSITORY=$(REPOSITORY) TAG=$(TAG) test/scripts/install.sh

# Runs every suite under test/e2e in order. The suites are disruptive.
.PHONY: e2e-test
e2e-test:
	set -a; . $(E2E_ENV_FILE); set +a; \
	go tool ginkgo -r --tags e2e -v --keep-going --timeout $(E2E_TIMEOUT) \
		--label-filter '$(E2E_LABEL)' --junit-report junit.xml --output-dir $(E2E_DIR)/report \
		test/e2e

.PHONY: e2e
e2e: e2e-cluster e2e-deploy e2e-test

.PHONY: e2e-logs
e2e-logs:
	OUTPUT_DIR=$(E2E_DIR)/logs test/scripts/collect-logs.sh

.PHONY: e2e-clean
e2e-clean:
	-$(KIND) delete cluster --name $(E2E_KIND_CLUSTER)
	-docker rm -f $(E2E_KIND_CLUSTER)-target
	-docker network rm $(E2E_KIND_CLUSTER)
	rm -rf $(E2E_DIR)

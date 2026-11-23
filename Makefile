CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.21.0
KUBECTL ?= kubectl
HELM ?= helm
KIND ?= kind

CHART := charts/cilium-egressgateway-ha
ARCH ?= amd64
IMAGE ?= ghcr.io/spidernet-io/cilium-egressgateway-ha
TAG ?= dev
KIND_CLUSTER ?= egw-ha
NAMESPACE ?= kube-system
E2E_VALUES ?= test/e2e/values.yaml

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

.PHONY: image
image: bin
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMAGE):$(TAG) .

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
	$(HELM) lint $(CHART) -f $(E2E_VALUES)

# Builds the image, loads it into the kind cluster and installs the chart.
.PHONY: kind-deploy
kind-deploy: image
	$(KIND) load docker-image --name $(KIND_CLUSTER) $(IMAGE):$(TAG)
	$(HELM) upgrade --install egwha $(CHART) -n $(NAMESPACE) -f $(E2E_VALUES) \
		--set image.repository=$(IMAGE) --set image.tag=$(TAG) --set image.pullPolicy=Never
	$(KUBECTL) -n $(NAMESPACE) rollout restart ds/egwha-agent deploy/egwha-controller
	$(KUBECTL) -n $(NAMESPACE) rollout status ds/egwha-agent --timeout=120s
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/egwha-controller --timeout=120s

# Runs against the kind cluster in $KUBECONFIG, e.g. KUBECONFIG=$(kind get kubeconfig --name egw-ha).
.PHONY: e2e
e2e: kind-deploy
	go test -tags e2e -count=1 -v -timeout 20m ./test/e2e/...

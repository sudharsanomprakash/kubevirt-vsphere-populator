# Reproducible build and test entry points. Go is fetched automatically via
# GOTOOLCHAIN from go.mod's toolchain line.
SHELL := /usr/bin/env bash
GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/version.Version=$(VERSION) -X github.com/sudharsanomprakash/kubevirt-vsphere-populator/internal/version.Commit=$(COMMIT)
REGISTRY ?= ghcr.io/sudharsanomprakash
CONTROLLER_IMAGE ?= $(REGISTRY)/kubevirt-vsphere-populator-controller:$(VERSION)
WORKER_IMAGE ?= $(REGISTRY)/kubevirt-vsphere-populator-worker:$(VERSION)
CONTROLLER_GEN ?= $(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.22.0
SETUP_ENVTEST ?= $(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.23
ENVTEST_K8S_VERSION ?= 1.37.0
GOLANGCI_LINT ?= golangci-lint

.PHONY: all build generate verify-generate fmt vet lint test test-race test-short fixtures envtest images e2e-kind clean

all: build

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/vsphere-populator-controller ./cmd/vsphere-populator-controller
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/vsphere-populator-worker ./cmd/vsphere-populator-worker

generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=deploy/crd

verify-generate: generate
	git diff --exit-code -- api deploy/crd

fmt:
	gofmt -l -w $$(git ls-files '*.go')

vet:
	$(GO) vet ./...

lint:
	$(GOLANGCI_LINT) run ./...

fixtures:
	./hack/fetch-fixtures.sh

envtest:
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path

# Full test suite: needs qemu-img, the fetched fixtures and envtest binaries.
# Loop-device tests additionally need root and are skipped otherwise.
test: fixtures
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	KVSP_REQUIRE_QEMU_IMG=1 KVSP_REQUIRE_FIXTURES=1 KVSP_REQUIRE_ENVTEST=1 \
	$(GO) test -count=1 -timeout 20m ./...

test-race: fixtures
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	KVSP_REQUIRE_QEMU_IMG=1 KVSP_REQUIRE_FIXTURES=1 KVSP_REQUIRE_ENVTEST=1 \
	$(GO) test -race -count=1 -timeout 30m ./...

test-short:
	$(GO) test -count=1 ./internal/vmdk/... ./internal/digest/... ./internal/redact/... ./internal/transfer/...

images:
	docker build -f build/Dockerfile.controller --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(CONTROLLER_IMAGE) .
	docker build -f build/Dockerfile.worker --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(WORKER_IMAGE) .

# End-to-end test in a disposable kind cluster against the fake vSphere
# (not a real vSphere). See test/e2e/README.md.
e2e-kind:
	./test/e2e/run.sh

clean:
	rm -rf bin dist

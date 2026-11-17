GO ?= go
CONTROLLER_GEN ?= controller-gen
IMAGE ?= accelerator-control-plane:dev

.PHONY: fmt vet test envtest build generate manifests container-build verify

fmt:
	$(GO) fmt ./...

test:
	$(GO) test ./...

# Requires setup-envtest and KUBEBUILDER_ASSETS to point at matching binaries.
envtest:
	$(GO) test ./internal/controller -run TestCRDsAndAtomicEndpointClaimsWithEnvtest -count=1 -v

vet:
	$(GO) vet ./...

build:
	$(GO) build -trimpath -o bin/manager ./cmd/manager

generate:
	$(CONTROLLER_GEN) object paths=./api/v1alpha1

manifests:
	$(CONTROLLER_GEN) crd paths=./api/v1alpha1 output:crd:dir=config/crd/bases

container-build:
	docker build -t $(IMAGE) .

verify: fmt vet test build

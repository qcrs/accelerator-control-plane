GO ?= go
CONTROLLER_GEN ?= controller-gen

.PHONY: fmt test manifests

fmt:
	$(GO) fmt ./...

test:
	$(GO) test ./...

manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=accelerator-manager-role paths=./... output:crd:artifacts:config=config/crd/bases

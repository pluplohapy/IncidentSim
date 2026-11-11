# IncidentSim — developer entry points.
#
# The Makefile is intentionally thin: every target is a plain `go` invocation
# so that it behaves identically on a laptop, in CI and inside a container.

TASK        ?= db-connection-leak
BINARY      := incidentsim
WORKLOAD    := incidentsim-workload
GO          := go
GOFLAGS     ?=
# VERSION and COMMIT are resolved *before* LDFLAGS uses them: LDFLAGS is
# expanded immediately (`:=`), so a `?=` below it would stamp an empty
# version into the binary.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS     := -s -w \
	-X incidentsim/internal/version.Version=$(VERSION) \
	-X incidentsim/internal/version.Commit=$(COMMIT)
PKGS        := ./...
LINT        := golangci-lint

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?##' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the CLI and the simulated workload service
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(WORKLOAD) ./cmd/$(WORKLOAD)

.PHONY: test
test: ## Run unit tests
	$(GO) test $(GOFLAGS) -race -count=1 $(PKGS)

.PHONY: test-short
test-short: ## Run unit tests without the race detector (faster feedback)
	$(GO) test $(GOFLAGS) -count=1 $(PKGS)

.PHONY: cover
cover: ## Run unit tests with a coverage report
	$(GO) test $(GOFLAGS) -count=1 -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format the tree
	$(GO) fmt $(PKGS)

.PHONY: vet
vet: ## Run go vet
	$(GO) vet $(PKGS)

.PHONY: lint
lint: ## Run golangci-lint (installed by `make tools`)
	$(LINT) run ./...

.PHONY: check
check: fmt vet test ## Everything CI runs on every push

.PHONY: tools
tools: ## Install the linter
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8

.PHONY: workload-image
workload-image: ## Build the simulated service image used by compose tasks
	docker build -q -t incidentsim/workload:dev -f build/workload.Dockerfile .

.PHONY: colima
colima: ## Start the local Linux VM on macOS (Colima + Docker)
	colima start --cpu 4 --memory 8 --disk 60 --vm-type vz

.PHONY: demo
demo: build ## Run one task end to end: broken, masked, fixed, postmortem
	./scripts/demo.sh $(TASK)

.PHONY: ansible-check
ansible-check: ## Validate the Firecracker playbook without running it
	ansible-playbook --syntax-check -i ansible/inventory.example.yml ansible/firecracker.yml

.PHONY: run-dev
run-dev: build ## Run the core API against the compose catalog
	./bin/$(BINARY) serve --config incidentsim.yaml

IMG ?= camilorivera/cert-manager-acm-sync
TAG ?= dev

# ──────────────────────────────────────────────────────────────────────────────
# FIRST TIME SETUP
# Run this once to download modules and generate go.sum (commit go.sum after):
#   make setup
# ──────────────────────────────────────────────────────────────────────────────
setup: mod-tidy

# Run any go command inside the dev container
# Usage: make go CMD="build ./..."
go:
	docker compose run --rm dev go $(CMD)

# Download Go modules
mod-download:
	docker compose run --rm dev go mod download

# Tidy go.mod and go.sum
mod-tidy:
	docker compose run --rm dev go mod tidy

# Build the manager binary
build:
	docker compose run --rm \
		-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 \
		dev go build -o bin/manager ./cmd/manager

# Run unit tests (fast, no envtest)
test-unit:
	docker compose run --rm -e CGO_ENABLED=0 \
		dev go test -v -short ./...

# Run all tests including integration tests (requires envtest binaries)
test: setup-envtest
	docker compose -f docker-compose.test.yml run --rm test

# Download envtest binaries into .envtest/bin
setup-envtest:
	docker compose run --rm dev go run \
		sigs.k8s.io/controller-runtime/tools/setup-envtest@latest \
		use --bin-dir .envtest/bin -p path

# Run golangci-lint
lint:
	docker compose run --rm lint

# Build the production Docker image
docker-build:
	docker build --platform linux/amd64 -t $(IMG):$(TAG) .

# Build multi-arch image (requires buildx)
docker-buildx:
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t $(IMG):$(TAG) -t $(IMG):latest --push .

# Push image to Docker Hub (requires docker login)
docker-push:
	docker push $(IMG):$(TAG)

# Package the Helm chart
helm-package:
	helm package charts/cert-manager-acm-sync -d .helm-output

# Deploy to the current kubectl context (raw YAML)
deploy:
	kubectl apply -f config/rbac/
	kubectl apply -f config/manager/

.PHONY: go mod-download mod-tidy build test-unit test setup-envtest lint \
        docker-build docker-buildx docker-push helm-package deploy

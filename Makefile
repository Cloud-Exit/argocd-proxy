.PHONY: build build-server build-agent test test-unit test-e2e lint docker clean

REGISTRY ?= ghcr.io/cloud-exit
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

build: build-server build-agent

build-server:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/proxy-server ./cmd/server

build-agent:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/proxy-agent ./cmd/agent

test: test-unit

test-unit:
	go test -race -count=1 ./pkg/...

test-e2e:
	go test -race -count=1 -timeout 10m -tags e2e ./test/e2e/

lint:
	golangci-lint run ./pkg/... ./cmd/...

docker: docker-server docker-agent

docker-server:
	docker build -t $(REGISTRY)/argocd-cluster-proxy-server:$(VERSION) -f Dockerfile.server .

docker-agent:
	docker build -t $(REGISTRY)/argocd-cluster-proxy-agent:$(VERSION) -f Dockerfile.agent .

clean:
	rm -rf bin/ dist/

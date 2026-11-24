VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/yousafkhamza/changefeed/internal/version.Version=$(VERSION)

.PHONY: build test lint vuln run chart image check

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/changefeed ./cmd/changefeed

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...

run:
	go run ./cmd/changefeed serve --demo --log-format text

chart:
	helm lint charts/changefeed --strict
	helm lint charts/changefeed --strict -f test/chart/full-values.yaml
	helm template cf charts/changefeed -f test/chart/full-values.yaml > /dev/null

image:
	docker build --build-arg VERSION=$(VERSION) -t changefeed:$(VERSION) .

check: test lint vuln chart

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
PKG     := github.com/nedanwr/vario/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

CH_IMAGE ?= clickhouse/clickhouse-server:25.8
CH_PORT  ?= 18123

.PHONY: build test race lint fmt integration clickhouse-up clickhouse-down

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/vario ./cmd/vario

test:
	go test ./...

race:
	go test -race ./...

fmt:
	gofmt -w .

lint:
	@test -z "$$(gofmt -l .)" || (gofmt -l .; echo "run make fmt"; exit 1)
	go vet ./...
	go run honnef.co/go/tools/cmd/staticcheck@v0.7.0 ./...

clickhouse-up:
	docker run -d --name vario-test-ch -p 127.0.0.1:$(CH_PORT):8123 \
		-e CLICKHOUSE_USER=vario -e CLICKHOUSE_PASSWORD=testpass -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
		--ulimit nofile=262144:262144 $(CH_IMAGE)

clickhouse-down:
	docker rm -f vario-test-ch

integration:
	VARIO_TEST_CLICKHOUSE_URL=http://127.0.0.1:$(CH_PORT) VARIO_TEST_CLICKHOUSE_USER=vario \
		VARIO_TEST_CLICKHOUSE_PASSWORD=testpass go test -count=1 -run Integration -v ./internal/storage/clickhouse/ ./internal/collector/

.PHONY: all build build-all clean test e2e demo demo-stop run-server run-client install deps fmt lint

# Variables
BINARY=bin/drip
VERSION?=dev
COMMIT=$(shell git rev-parse --short=10 HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-s -w -X main.Version=${VERSION} -X main.GitCommit=${COMMIT} -X main.BuildTime=${BUILD_TIME}"

# Default target
all: clean deps test build

# Install dependencies
deps:
	go mod download
	go mod tidy

# Build unified binary
build:
	@echo "Building Drip..."
	@mkdir -p bin
	go build ${LDFLAGS} -o ${BINARY} ./cmd/drip
	@echo "Build complete!"

# Build for all platforms
build-all: clean
	@echo "Building for multiple platforms..."
	@mkdir -p bin

	# Linux AMD64
	GOOS=linux GOARCH=amd64 go build ${LDFLAGS} -o bin/drip-linux-amd64 ./cmd/drip

	# Linux ARM64
	GOOS=linux GOARCH=arm64 go build ${LDFLAGS} -o bin/drip-linux-arm64 ./cmd/drip

	# macOS AMD64
	GOOS=darwin GOARCH=amd64 go build ${LDFLAGS} -o bin/drip-darwin-amd64 ./cmd/drip

	# macOS ARM64 (Apple Silicon)
	GOOS=darwin GOARCH=arm64 go build ${LDFLAGS} -o bin/drip-darwin-arm64 ./cmd/drip

	# Windows AMD64
	GOOS=windows GOARCH=amd64 go build ${LDFLAGS} -o bin/drip-windows-amd64.exe ./cmd/drip

	# Windows ARM64
	GOOS=windows GOARCH=arm64 go build ${LDFLAGS} -o bin/drip-windows-arm64.exe ./cmd/drip

	@echo "Multi-platform build complete!"

# Run tests
test:
	go test -v -race -cover ./...

# Full end-to-end functional tests (starts local server/client/backends)
e2e: build
	bash scripts/test/e2e-full.sh

# Local demo: server, admin panel, control plane, two connected clients
demo:
	bash scripts/dev/local-demo.sh

demo-stop:
	bash scripts/dev/local-demo.sh stop

# Run tests with coverage
test-coverage:
	go test -v -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"

# Benchmark tests
bench:
	go test -bench=. -benchmem ./...

# Run server locally
run-server:
	go run ./cmd/drip server

# Run client locally (example)
run-client:
	go run ./cmd/drip http 3000

# Install globally
install:
	go install ${LDFLAGS} ./cmd/drip

# Format code
fmt:
	go fmt ./...
	gofmt -s -w .

# Lint code
lint:
	@which golangci-lint > /dev/null || (echo "Installing golangci-lint..." && go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest)
	golangci-lint run ./...

# Clean build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@rm -f coverage.out coverage.html
	@echo "Clean complete!"

# Docker build
docker-build:
	docker build -t drip-server:${VERSION} -f deployments/Dockerfile .

# Docker run
docker-run:
	docker run -p 80:80 -p 8080:8080 drip-server:${VERSION}

# Generate test certificates
gen-certs:
	@echo "Generating test TLS 1.3 certificates..."
	@mkdir -p certs
	openssl req -x509 -newkey rsa:4096 -nodes \
	    -keyout certs/server-key.pem \
	    -out certs/server-cert.pem \
	    -days 365 \
	    -subj "/CN=localhost"
	@echo "Test certificates generated in certs/"
	@echo "⚠️  Warning: These are self-signed certificates for testing only!"

# Help
help:
	@echo "Drip - Available Make Targets:"
	@echo ""
	@echo "  make build        - Build server and client"
	@echo "  make build-all    - Build for all platforms"
	@echo "  make test         - Run unit tests"
	@echo "  make e2e          - Run full end-to-end functional tests"
	@echo "  make test-coverage - Run tests with coverage report"
	@echo "  make bench        - Run benchmark tests"
	@echo "  make run-server   - Run server locally"
	@echo "  make run-client   - Run client locally (port 3000)"
	@echo "  make gen-certs    - Generate test TLS certificates"
	@echo "  make install      - Install client globally"
	@echo "  make fmt          - Format code"
	@echo "  make lint         - Lint code"
	@echo "  make clean        - Clean build artifacts"
	@echo "  make deps         - Install dependencies"
	@echo "  make docker-build - Build Docker image"
	@echo "  make docker-run   - Run Docker container"
	@echo ""
	@echo "Build info:"
	@echo "  VERSION=${VERSION}"
	@echo "  COMMIT=${COMMIT}"
	@echo "  BUILD_TIME=${BUILD_TIME}"

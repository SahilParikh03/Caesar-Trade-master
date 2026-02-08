.PHONY: build test lint proto clean dev-up dev-down

# Build all binaries
build:
	go build -o bin/caesar ./cmd/caesar
	go build -o bin/signer ./cmd/signer

# Run all tests
test:
	go test ./... -v -race -count=1

# Run tests with coverage
test-cover:
	go test ./... -race -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

# Lint
lint:
	golangci-lint run ./...

# Generate protobuf stubs
proto:
	buf generate

# Lint protobuf
proto-lint:
	buf lint

# Start dev infrastructure
dev-up:
	docker compose up -d
	@echo "Waiting for services..."
	@sleep 3
	@docker compose ps

# Stop dev infrastructure
dev-down:
	docker compose down

# Clean build artifacts
clean:
	rm -rf bin/ coverage.out coverage.html internal/gen/

.PHONY: build run run-server run-worker temporal test race lint fmt

# Build both binaries into bin/.
build:
	go build -o bin/ ./cmd/...

# Run the server and the worker together. Ctrl-C stops both.
# Needs the Temporal dev server running (make temporal).
run:
	$(MAKE) -j2 run-server run-worker

run-server:
	go run ./cmd/server

run-worker:
	go run ./cmd/worker

# Local Temporal server with the web UI on http://localhost:8233.
temporal:
	temporal server start-dev

test:
	go test ./...

race:
	go test -race ./...

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

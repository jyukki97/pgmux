BINARY_NAME=db-proxy
BUILD_DIR=bin

.PHONY: build run test lint clean

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/db-proxy

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

test:
	go test ./... -v

test-coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

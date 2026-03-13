BINARY_NAME=pgmux
BUILD_DIR=bin

.PHONY: build run test test-integration test-coverage bench bench-compare lint clean docker-up docker-down docker-build

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/pgmux

run: build
	./$(BUILD_DIR)/$(BINARY_NAME)

test:
	go test ./... -v

test-integration:
	go test ./tests/ -v -count=1

test-coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

bench:
	go test ./tests/ -bench=. -benchmem -count=3

bench-compare:
	./scripts/bench-compare.sh

lint:
	golangci-lint run ./...

docker-up:
	docker-compose up -d
	@echo "Waiting for services to be healthy..."
	@sleep 10
	@echo "Services ready."

docker-down:
	docker-compose down -v

docker-build:
	docker build -t $(BINARY_NAME):latest .

clean:
	rm -rf $(BUILD_DIR) coverage.out coverage.html

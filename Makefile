.PHONY: build test run migrate tidy lint docker-build

build:
	CGO_ENABLED=0 go build -o bin/bughan ./cmd/bughan

test:
	go test ./...

run: migrate
	go run ./cmd/bughan serve

migrate:
	go run ./cmd/bughan migrate

tidy:
	go mod tidy

lint:
	go vet ./...

docker-build:
	docker build -t ghcr.io/atash03/bughan:dev .

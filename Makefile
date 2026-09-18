VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ragmux/ragmux:latest

.PHONY: build run test vet docker-build docker-run clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/ragmux ./cmd/ragmux

run: build
	DATA_DIR=./data PORT=8080 ./bin/ragmux

test:
	go test ./...

vet:
	go vet ./...

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run: docker-build
	docker run -d -p 8080:8080 -v ./gateway_data:/app/data --name ragmux $(IMAGE)

clean:
	rm -rf bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ragmux/ragmux:latest

# Local pgvector Postgres started by `make dev-db` (docker-compose.dev.yml).
TEST_DATABASE_URL ?= postgres://ragmux:ragmux@localhost:5433/ragmux_test?sslmode=disable
export TEST_DATABASE_URL

.PHONY: build run test vet dev-db dev-db-down docker-build docker-run backup restore clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/ragmux ./cmd/ragmux

# Runs against the dev database; SECRET_KEY falls back to ./data/secret.key.
run: build
	DATABASE_URL=$(TEST_DATABASE_URL) DATA_DIR=./data PORT=8765 ./bin/ragmux

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

dev-db:
	docker compose -f docker-compose.dev.yml up -d --wait

dev-db-down:
	docker compose -f docker-compose.dev.yml down

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-run: docker-build
	docker compose up -d

# Logical backup of the compose database into ./backups (scripts/backup.sh).
backup:
	scripts/backup.sh

# Restore a dump: make restore FILE=backups/ragmux-....dump YES=1
# Without YES=1 the script only prints its plan.
restore:
	@test -n "$(FILE)" || { echo "usage: make restore FILE=<dump> [YES=1]"; exit 2; }
	scripts/restore.sh $(if $(YES),--yes) "$(FILE)"

clean:
	rm -rf bin

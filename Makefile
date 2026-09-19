VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ragmux/ragmux:latest

# Local pgvector Postgres started by `make dev-db` (docker-compose.dev.yml).
TEST_DATABASE_URL ?= postgres://ragmux:ragmux@localhost:5433/ragmux_test?sslmode=disable
export TEST_DATABASE_URL

# Local ParadeDB Postgres started by `make dev-db-paradedb`
# (docker-compose.dev.paradedb.yml). One port up from the pgvector dev
# database so both can run at once: the plain one is what proves the fallback
# path, the ParadeDB one is what exercises BM25.
PARADEDB_TEST_DATABASE_URL ?= postgres://ragmux:ragmux@localhost:5434/ragmux_test?sslmode=disable

.PHONY: build run test test-paradedb vet dev-db dev-db-down dev-db-paradedb dev-db-paradedb-down \
	docker-build docker-build-app docker-build-paradedb docker-run backup restore clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/ragmux ./cmd/ragmux

# Runs against the dev database; SECRET_KEY falls back to ./data/secret.key.
run: build
	DATABASE_URL=$(TEST_DATABASE_URL) DATA_DIR=./data PORT=8765 ./bin/ragmux

test:
	go test -race -count=1 ./...

# The same suite against ParadeDB: the pg_search tests skip themselves on the
# plain database and run here. Both jobs matter -- `make test` is what proves
# a store configured for pg_search degrades instead of failing.
test-paradedb:
	TEST_DATABASE_URL="$(PARADEDB_TEST_DATABASE_URL)" go test -race -count=1 ./...

vet:
	go vet ./...

dev-db:
	docker compose -f docker-compose.dev.yml up -d --wait

dev-db-down:
	docker compose -f docker-compose.dev.yml down

dev-db-paradedb:
	docker compose -f docker-compose.dev.paradedb.yml up -d --wait

dev-db-paradedb-down:
	docker compose -f docker-compose.dev.paradedb.yml down

# All-in-one image (gateway + embedded PostgreSQL), what docker-compose.yml runs.
docker-build:
	docker build -f Dockerfile.aio --build-arg VERSION=$(VERSION) -t $(IMAGE) .

# Gateway-only distroless image for an external database (published as *-app).
docker-build-app:
	docker build -f Dockerfile --build-arg VERSION=$(VERSION) -t $(IMAGE)-app .

# All-in-one image on ParadeDB (pg_search + pgvector), published as *-paradedb.
docker-build-paradedb:
	docker build -f Dockerfile.aio.paradedb --build-arg VERSION=$(VERSION) -t $(IMAGE)-paradedb .

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

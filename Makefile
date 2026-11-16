# Run in Cloud Shell or the devbox VM (Go + Docker available); nothing runs on the laptop.

COMPOSE := docker compose -f deployments/compose.yaml
DEV_DATABASE_URL ?= postgres://postgres:postgres@localhost:5432/app?sslmode=disable
APP_DATABASE_URL ?= postgres://app_dev:app_dev@localhost:5432/app?sslmode=disable

.PHONY: tidy up down migrate dev-login test itest lint run-api

tidy:            ## resolve modules and write go.sum
	go mod tidy

up:              ## start the dev test stack
	$(COMPOSE) up -d --wait

down:
	$(COMPOSE) down -v

migrate:         ## apply migrations as the owning role (order: up, dev-login, migrate)
	DATABASE_URL=$(DEV_DATABASE_URL) APP_DB_LOGIN_ROLES=app_dev go run ./cmd/migrate

dev-login:       ## create the local app_dev login (member of app only)
	$(COMPOSE) exec -T postgres psql -U postgres -d app -v ON_ERROR_STOP=1 -c \
	  "DO \$$\$$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='app_dev') THEN CREATE ROLE app_dev LOGIN PASSWORD 'app_dev'; END IF; END \$$\$$;"

test:            ## unit tests
	go vet ./...
	go test -race ./...

itest:           ## integration + isolation tests (testcontainers, needs Docker)
	go test -race -tags integration -count=1 ./tests/... ./internal/...

lint:
	golangci-lint run ./...

run-api:         ## run the API against the compose database as app_dev
	DATABASE_URL=$(APP_DATABASE_URL) \
	JWT_SIGNING_KEY=$${JWT_SIGNING_KEY:-dev-only-signing-key-change-me-0123456789} \
	go run ./cmd/api

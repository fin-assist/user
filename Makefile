# Common commands; CI runs the same steps (.github/workflows/ci.yml).
GOLANGCI_LINT_VERSION := v2.15.0
BIN := bin/user
export GOPRIVATE := github.com/fin-assist/*

.PHONY: build test cover lint fmt run migrate image up down dev-key

build: ## build the binary into bin/
	go build -trimpath -o $(BIN) ./cmd/user

test: ## go.mod tidy, unit and integration tests (Docker or TEST_DATABASE_URL)
	go mod tidy -diff
	go vet ./...
	go test -race -count=1 ./...

cover: ## tests with a coverage profile, then coverage of the lines changed against origin/main
	go test -race -count=1 -coverpkg=./... -coverprofile=cover.out ./...
	python3 scripts/coverage-diff.py --base origin/main

lint: ## golangci-lint of the pinned version
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run ./...

fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt ./...

image: ## the service image
	docker build -t user:dev .

up: dev-key ## PostgreSQL, migrations and the service in Docker
	LOCAL_UID=$$(id -u) LOCAL_GID=$$(id -g) docker compose up --build

down:
	docker compose down

run: build dev-key ## the service on the host against the compose PostgreSQL
	SERVICE_NAME=user ENV=dev AUTH_MODE=insecure-dev LOG_FORMAT=text \
	JWT_SIGNING_KEY_FILE=.local/jwt-signing.pem JWT_KEY_ID=local JWT_ISSUER=local-user JWT_AUDIENCE=local-app \
	DB_PRIMARY_DSN='postgres://user_service:user_service@localhost:5432/user?sslmode=disable' \
	$(BIN) serve

migrate: build ## migrate up against the compose PostgreSQL
	DB_PRIMARY_DSN='postgres://user_service:user_service@localhost:5432/user?sslmode=disable' $(BIN) migrate up

# A throwaway local user-signing key; never replace an existing key on repeated runs.
dev-key:
	@mkdir -p .local
	@go run ./cmd/dev-key .local/jwt-signing.pem

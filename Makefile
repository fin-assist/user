# Common commands; CI runs the same steps (.github/workflows/ci.yml).
GOLANGCI_LINT_VERSION := v2.15.0
BIN := bin/user
export GOPRIVATE := github.com/fin-assist/*

.PHONY: build test cover lint fmt run migrate image up down dev-key release-check

build: ## build the binary into bin/
	go build -trimpath -o $(BIN) ./cmd/user

test: ## go.mod tidy, unit/integration and script tests, changelog format
	go mod tidy -diff
	go vet ./...
	go test -race -count=1 ./...
	python3 -m unittest discover -s scripts -p 'test_*.py'
	python3 scripts/changelog.py check

release-check: ## VERSION=vX.Y.Z: validate the CHANGELOG section before tagging
	@test -n "$(VERSION)" || { echo "usage: make release-check VERSION=vX.Y.Z"; exit 2; }
	python3 scripts/changelog.py check
	test "$$(python3 scripts/changelog.py latest)" = "$(VERSION)"

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
	AUTH_IDEMPOTENCY_KEY_FILE=.local/auth-idempotency.key \
	PHONE_RESULT_KEY_FILE=.local/phone-result.key \
	PEER_BANK_STATEMENT_DATA_ADDR="$${PEER_BANK_STATEMENT_DATA_ADDR:-dns:///127.0.0.1:9092}" \
	PEER_ANALYTICS_ADDR="$${PEER_ANALYTICS_ADDR:-dns:///127.0.0.1:9093}" \
	PEER_ASSISTANT_ADDR="$${PEER_ASSISTANT_ADDR:-dns:///127.0.0.1:9094}" \
	DB_PRIMARY_DSN='postgres://user_service:user_service@localhost:5432/user?sslmode=disable' \
	$(BIN) serve

migrate: build ## migrate up against the compose PostgreSQL
	DB_PRIMARY_DSN='postgres://user_service:user_service@localhost:5432/user?sslmode=disable' $(BIN) migrate up

# A throwaway local user-signing key; never replace an existing key on repeated runs.
dev-key:
	@mkdir -p .local
	@go run ./cmd/dev-key .local/jwt-signing.pem
	@go run ./cmd/dev-key --aes .local/auth-idempotency.key
	@go run ./cmd/dev-key --aes .local/phone-result.key

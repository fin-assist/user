# Common commands; CI runs the same steps (.github/workflows/ci.yml).
SHELL := /bin/bash
GOLANGCI_LINT_VERSION := v2.15.0
GO_TEST_COVERAGE_VERSION := v2.20.1
TEST_TIMEOUT ?= 8m
COVERAGE := go run github.com/vladopajic/go-test-coverage/v2@$(GO_TEST_COVERAGE_VERSION)
BIN := bin/user
export GOPRIVATE := github.com/fin-assist/*

.PHONY: build test test-names cover collect-unit collect-integration collect-api check-unit check-integration check-api cover-unit cover-integration cover-api cover-combined lint fmt run migrate image up down dev-key release-check

build: ## build the binary into bin/
	go build -trimpath -o $(BIN) ./cmd/user

test: test-names ## Go race suites, script tests and changelog format
	go mod tidy -diff
	go vet ./...
	go test -race -count=1 -timeout=$(TEST_TIMEOUT) ./...
	python3 -m unittest discover -s scripts -p 'test_*.py'
	python3 scripts/changelog.py check

release-check: ## VERSION=vX.Y.Z: validate the CHANGELOG section before tagging
	@test -n "$(VERSION)" || { echo "usage: make release-check VERSION=vX.Y.Z"; exit 2; }
	python3 scripts/changelog.py check
	test "$$(python3 scripts/changelog.py latest)" = "$(VERSION)"

test-names: ## reject tests, fuzz targets and examples omitted from the three selectors
	go test -race -list '^(Test|Fuzz|Example)' -timeout=8m ./... > test-names.txt
	@if grep -E '^(Test|Fuzz)' test-names.txt | grep -Ev '^(Test|Fuzz)(Unit|Integration|API).+'; then \
	  echo 'Name every test TestUnit..., TestIntegration... or TestAPI... and every fuzz target FuzzUnit..., FuzzIntegration... or FuzzAPI...' >&2; exit 1; fi
	@if grep -E '^Example' test-names.txt; then \
	  echo 'Examples run outside the three suites; a service has no public Go API: write a TestUnit/TestIntegration/TestAPI instead' >&2; exit 1; fi

# All suites instrument the same whole module. No production files are excluded.
# Ordinary packages without tests may be reported as skipped; skipped Test events fail.
collect-unit collect-integration collect-api: test-names
	@set -euo pipefail; level='$(@:collect-%=%)'; case "$$level" in unit) prefix=Unit;; integration) prefix=Integration;; api) prefix=API;; esac; \
	rm -f "$$level.out" "$$level.jsonl"; \
	CI=true go test -json -race -count=1 -timeout=8m -coverpkg=./... \
	  -coverprofile="$$level.out" -run "^(Test|Fuzz)$$prefix" ./... | tee "$$level.jsonl"; \
	bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"

check-unit check-integration check-api:
	@set -euo pipefail; level='$(@:check-%=%)'; \
	bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"; \
	$(COVERAGE) --config .testcoverage.yml --profile "$$level.out" \
	  --breakdown-file-name "$$level.coverage.txt" | tee "$$level.report.txt"

cover-unit: collect-unit
	$(MAKE) check-unit
cover-integration: collect-integration
	$(MAKE) check-integration
cover-api: collect-api
	$(MAKE) check-api

cover-combined: ## informational union; the tool merges profiles, never percentages
	@set -euo pipefail; for level in unit integration api; do bash scripts/check-suite.sh "$$level.jsonl" "$$level.out"; done
	set -euo pipefail; $(COVERAGE) --profile unit.out,integration.out,api.out \
	  --breakdown-file-name combined.coverage.txt | tee combined.report.txt

cover: collect-unit collect-integration collect-api ## collect all profiles before enforcing the three gates
	@status=0; for level in unit integration api; do $(MAKE) check-$$level || status=1; done; \
	$(MAKE) cover-combined || status=1; exit "$$status"

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

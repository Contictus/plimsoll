# The command vocabulary from CLAUDE.md §4. `make test` must stay fast enough to run on
# every save; if it ever needs Docker, an engine has grown a dependency it should not
# have (L4).

ifeq ($(OS),Windows_NT)
  # NOT plain `bash`: on Windows that resolves to C:\Windows\System32\bash.exe, the WSL
  # launcher, which would run every recipe in a different filesystem and toolchain.
  SHELL := C:/Program Files/Git/bin/bash.exe
else
  SHELL := /bin/bash
endif
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

BACKEND := backend
COMPOSE := docker compose -f deploy/compose.yaml --env-file .env

.PHONY: help generate migrate migrate-down test test-integration lint docs-check up down psql smoke

help: ## Show this help
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  %-18s %s\n", $$1, $$2}'

generate: ## sqlc — regenerate typed queries; output is committed
	cd $(BACKEND) && go tool sqlc generate

migrate: ## goose up, as plimsoll_owner (never the app role)
	cd $(BACKEND) && go tool goose -dir migrations postgres "$$PLIMSOLL_OWNER_DSN" up

migrate-down: ## goose down one step, as plimsoll_owner
	cd $(BACKEND) && go tool goose -dir migrations postgres "$$PLIMSOLL_OWNER_DSN" down

test: ## unit: pure engines, no Docker, fast
	cd $(BACKEND) && go test ./...

test-integration: ## //go:build integration — real Postgres via compose
	cd $(BACKEND) && go test -tags=integration -count=1 ./...

lint: ## golangci-lint
	cd $(BACKEND) && go tool golangci-lint run

docs-check: ## CLAUDE.md must be byte-identical to AGENTS.md
	@diff -u CLAUDE.md AGENTS.md && echo "docs-check: OK"

up: ## start the stack and wait for health
	$(COMPOSE) up -d --wait

down: ## stop the stack and drop volumes
	$(COMPOSE) down -v

psql: ## interactive psql as the app role (to inspect what RLS actually allows)
	$(COMPOSE) exec postgres psql "$$PLIMSOLL_APP_DSN"

smoke: ## the M0 exit check, end to end through Caddy
	bash deploy/smoke.sh

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

# Load .env so `make migrate` and `make test-integration` work without the caller having
# to source it first. Values are exported to every recipe.
ifneq (,$(wildcard .env))
  include .env
  export
endif

BACKEND := backend
COMPOSE := docker compose -f deploy/compose.yaml --env-file .env

.PHONY: test-integration-v help generate migrate migrate-down test test-integration lint docs-check up down psql smoke frontend-check

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

frontend-check: ## dashboard: types, the no-float-parsing guard (L1), and the money tests
	cd frontend && npm run check

docs-check: ## CLAUDE.md must be byte-identical to AGENTS.md
	@diff -u CLAUDE.md AGENTS.md && echo "docs-check: OK"

up: ## build, migrate and start the stack, waiting for health
	# --build is not an optimisation to drop. plimsollctl carries the migrations
	# embedded in its binary (K46), so an image built before the newest migration
	# would start the stack against a schema older than the code -- which is the exact
	# mismatch embedding them was chosen to make impossible. The build cache makes a
	# no-change rebuild cheap; a stale schema is not cheap at all.
	$(COMPOSE) up -d --build --wait

down: ## stop the stack and drop volumes
	$(COMPOSE) down -v

psql: ## interactive psql as the app role (to inspect what RLS actually allows)
	$(COMPOSE) exec postgres psql "$$PLIMSOLL_APP_DSN"

smoke: ## the M0 exit check, end to end through Caddy
	bash deploy/smoke.sh
test-integration-v: ## integration tests, verbose
	cd $(BACKEND) && go test -tags=integration -count=1 -v ./...

SHELL := /usr/bin/env bash

GO              ?= go
MODULE          := github.com/kilolockio/kilolock
BIN_DIR         := bin
KL_BIN   := $(BIN_DIR)/kl
KLD_BIN := $(BIN_DIR)/kld
VERSION         ?= 0.0.0-dev
GIT_COMMIT      ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
GIT_DIRTY       ?= $(shell if [ -n "$$(git status --porcelain 2>/dev/null)" ]; then echo dirty; else echo clean; fi)
BUILD_TIME      ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS         ?= -X 'main.version=$(VERSION)' -X 'main.buildCommit=$(GIT_COMMIT)' -X 'main.buildTime=$(BUILD_TIME)' -X 'main.buildDirty=$(GIT_DIRTY)'

# COMPOSE picks either the docker-compose v2 standalone binary or the
# `docker compose` plugin form. Override with `make COMPOSE='docker compose' ...`
# if your environment has only the plugin.
COMPOSE         ?= docker-compose

POSTGRES_DSN    ?= postgres://kl:kl@localhost:5432/kl?sslmode=disable

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} \
		/^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

# ---------------------------------------------------------------------------
# Local Postgres
# ---------------------------------------------------------------------------

.PHONY: db-up
db-up: ## Start the local Postgres container.
	$(COMPOSE) up -d postgres
	@echo "Postgres available at $(POSTGRES_DSN)"

.PHONY: db-down
db-down: ## Stop the local Postgres container.
	$(COMPOSE) down

.PHONY: db-reset
db-reset: ## Wipe Postgres data and re-create the schema from scratch.
	$(COMPOSE) down -v
	$(COMPOSE) up -d postgres

.PHONY: db-shell
db-shell: ## Open a psql shell against the local Postgres.
	$(COMPOSE) exec postgres psql -U kl -d kl

.PHONY: migrate
migrate: build ## Apply pending schema migrations to the configured database.
	@echo "Applying database migrations..."
	@KL_DATABASE_URL=$(POSTGRES_DSN) $(KLD_BIN) migrate

.PHONY: compose-up
compose-up: ## Build and start Postgres + kl server (docker compose).
	$(COMPOSE) up --build -d
	@echo "Health:  curl -s http://localhost:8080/healthz"
	@echo "API:     curl -s -H 'Authorization: Bearer \$${KL_AUTH_TOKEN:-dev-local-token-change-me}' http://localhost:8080/states/example"

.PHONY: compose-down
compose-down: ## Stop the full docker compose stack.
	$(COMPOSE) down

.PHONY: compose-logs
compose-logs: ## Follow kl server logs.
	$(COMPOSE) logs -f kl

.PHONY: compose-quick-up
compose-quick-up: ## Start quick local dev stack (open auth, no token flow).
	docker compose -f docker-compose.quick.yml up --build -d

.PHONY: compose-quick-down
compose-quick-down: ## Stop quick local dev stack.
	docker compose -f docker-compose.quick.yml down

.PHONY: compose-cloud-up
compose-cloud-up: ## Start prod-like docker compose with Cloud features enabled (build tag cloud).
	KL_GO_TAGS=cloud $(COMPOSE) up --build -d

.PHONY: compose-cloud-down
compose-cloud-down: ## Stop prod-like docker compose (Cloud-tag build).
	$(COMPOSE) down

.PHONY: compose-quick-cloud-up
compose-quick-cloud-up: ## Start quick local dev stack with Cloud features enabled (build tag cloud).
	KL_GO_TAGS=cloud docker compose -f docker-compose.quick.yml up --build -d

.PHONY: compose-quick-cloud-down
compose-quick-cloud-down: ## Stop quick local dev stack (Cloud-tag build).
	docker compose -f docker-compose.quick.yml down

.PHONY: ci-multi-instance-smoke
ci-multi-instance-smoke: ## CI smoke: multi-instance compose + routed provision + validation.
	@./scripts/ci-multi-instance-smoke.sh

.PHONY: ci-multi-instance-failure-drill
ci-multi-instance-failure-drill: ## CI drill: stop premium DB, verify premium fails while shared stays healthy.
	@./scripts/ci-multi-instance-failure-drill.sh

.PHONY: ci-prod-critical
ci-prod-critical: ## CI gate: prod-critical package test/build set.
	@./scripts/ci-prod-critical.sh

.PHONY: ci-migration-upgrade-smoke
ci-migration-upgrade-smoke: ## CI gate: migrate N-1 -> latest and boot-check serve/control.
	@./scripts/ci-migration-upgrade-smoke.sh

.PHONY: ci-quick-compose-smoke
ci-quick-compose-smoke: ## CI smoke: quick compose health + control-plane policy flip + scoped-demo/deletion + confirm-scope gate.
	@bash ./scripts/ci-quick-compose-smoke.sh

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

.PHONY: build
build: ## Build the client and server/runtime binaries into ./bin/.
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(KL_BIN) ./cmd/kl
	$(GO) build -ldflags "$(LDFLAGS)" -o $(KLD_BIN) ./cmd/kld

.PHONY: run
run: ## Run the kl client CLI (passes ARGS through).
	$(GO) run ./cmd/kl $(ARGS)

.PHONY: run-server
run-server: ## Run the kld server/runtime binary (passes ARGS through).
	$(GO) run ./cmd/kld $(ARGS)

.PHONY: test
test: ## Run unit tests (skips integration build tag).
	$(GO) test ./...

.PHONY: test-prod-critical
test-prod-critical: ## Run prod-critical package test gate.
	$(GO) test ./cmd/kl ./cmd/kld ./cmd/klc ./pkg/store ./internal/backend

.PHONY: test-integration
test-integration: ## Run integration tests (requires KL_DATABASE_URL).
	# -p=1 forces packages to run sequentially. Integration tests
	# share one Postgres database; without sequential packages, the
	# cleanup routine in one package can race with row-level writes
	# in another, producing flaky failures that are hard to debug.
	# Per-package parallelism is preserved (tests within a package
	# still go through t.Parallel where they opt in).
	$(GO) test -tags=integration -p=1 ./...

.PHONY: fmt
fmt: ## Format Go sources.
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet across all packages.
	$(GO) vet ./...

.PHONY: tidy
tidy: ## Tidy go.mod / go.sum.
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove build artifacts.
	rm -rf $(BIN_DIR)

# ---------------------------------------------------------------------------
# Demos
# ---------------------------------------------------------------------------

.PHONY: parallel-tf-a
parallel-tf-a: build migrate ## HEADLINE: terminal 1, plain `terraform apply` bumping time_sleep.slow_a (optimistic merge in backend, no -target).
	@./examples/big-state/parallel-demo.sh tf_a

.PHONY: parallel-tf-b
parallel-tf-b: build migrate ## HEADLINE: terminal 2, plain `terraform apply` bumping time_sleep.slow_b (optimistic merge in backend, no -target).
	@./examples/big-state/parallel-demo.sh tf_b

.PHONY: parallel-tf
parallel-tf: build migrate ## Single-terminal variant of the plain-terraform parallel demo (both applies backgrounded).
	@./examples/big-state/parallel-demo.sh tf_both

.PHONY: parallel-demo-a
parallel-demo-a: build migrate ## ADVANCED: terminal 1, `kl apply` with explicit reservations on time_sleep.slow_a.
	@./examples/big-state/parallel-demo.sh slow_a

.PHONY: parallel-demo-b
parallel-demo-b: build migrate ## ADVANCED: terminal 2, `kl apply` with explicit reservations on time_sleep.slow_b.
	@./examples/big-state/parallel-demo.sh slow_b

.PHONY: parallel-demo
parallel-demo: build migrate ## Single-terminal variant of the `kl apply` parallel demo (both applies backgrounded).
	@./examples/big-state/parallel-demo.sh both

.PHONY: demo-policy-show
demo-policy-show: build ## Show current control-plane coexistence policy for big-state.
	@./examples/big-state/parallel-demo.sh policy_show

.PHONY: demo-policy-warn
demo-policy-warn: build ## Flip big-state coexistence policy to warn via control API.
	@./examples/big-state/parallel-demo.sh policy_warn

.PHONY: demo-policy-strict
demo-policy-strict: build ## Flip big-state coexistence policy to strict via control API.
	@./examples/big-state/parallel-demo.sh policy_strict

.PHONY: wait-demo-blocker
wait-demo-blocker: build migrate ## Two-terminal RESERVATION-CONFLICT demo, terminal 1: hold slow_a for ~30s.
	@./examples/big-state/wait-demo.sh blocker

.PHONY: wait-demo-waiter
wait-demo-waiter: build ## Two-terminal RESERVATION-CONFLICT demo, terminal 2: contend for slow_a, watch wait block stream live.
	@./examples/big-state/wait-demo.sh waiter

.PHONY: wait-demo
wait-demo: build migrate ## Single-terminal variant of the reservation-wait demo (waiter in fg, blocker in bg).
	@./examples/big-state/wait-demo.sh both

.PHONY: file-scope-compare
file-scope-compare: build migrate ## ADR-0014 demo: compare full-plan vs file-scoped plan timing.
	@./examples/big-state/file-scope-demo.sh compare

.PHONY: file-scope-demo-a
file-scope-demo-a: build migrate ## ADR-0014 demo terminal 1: file-scoped plan/apply for slow_a.tf.
	@./examples/big-state/file-scope-demo.sh slow_a

.PHONY: file-scope-demo-b
file-scope-demo-b: build migrate ## ADR-0014 demo terminal 2: file-scoped plan/apply for slow_b.tf.
	@./examples/big-state/file-scope-demo.sh slow_b

.PHONY: file-scope-demo
file-scope-demo: build migrate ## ADR-0014 single-terminal: file-scoped plans/applies in parallel.
	@./examples/big-state/file-scope-demo.sh both

.PHONY: deletion-scope-demo
deletion-scope-demo: build migrate ## ADR-0014 hardening: scoped deletes still surface after block removal (ownership cache).
	@bash ./examples/big-state/deletion-scope-demo.sh run

.PHONY: target-scope-compare
target-scope-compare: build migrate ## ADR-0017 demo: compare full-plan vs --target plan timing.
	@./examples/big-state/target-scope-demo.sh compare

.PHONY: target-scope-noop-parity
target-scope-noop-parity: build migrate ## ADR-0017 guard: full vs --target no-op parity check.
	@./examples/big-state/target-scope-demo.sh noop-parity

.PHONY: file-scope-noop-parity
file-scope-noop-parity: build migrate ## ADR-0014 guard: full vs --file no-op parity check.
	@./examples/big-state/file-scope-demo.sh noop-parity

.PHONY: demo-snapshot
demo-snapshot: ## Capture the local DB (incl. big-state) to examples/big-state/big-state.dump.
	@./scripts/demo-snapshot.sh

.PHONY: demo-warm
demo-warm: ## Restore examples/big-state/big-state.dump into local Postgres (DESTRUCTIVE; ~5s).
	@./scripts/demo-warm.sh

.PHONY: demo-status
demo-status: ## Quick "is big-state alive" sanity check before/after a demo.
	@if [ -z "$$KL_DATABASE_URL" ]; then \
		echo "KL_DATABASE_URL not set"; exit 2; \
	fi
	@printf 'big-state current resources: '
	@psql -tA "$$KL_DATABASE_URL" -c \
		"SELECT count(*) FROM current_resources WHERE state_name = 'big-state';"
	@printf 'big-state version count:     '
	@psql -tA "$$KL_DATABASE_URL" -c \
		"SELECT count(*) FROM state_versions sv JOIN states s ON s.id = sv.state_id WHERE s.name = 'big-state';"

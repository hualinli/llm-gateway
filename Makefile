.PHONY: tidy up down run vet test test-up test-down integration-test mock-primary mock-backup demo-up demo-observability demo-config demo-smoke demo-smoke-backup demo-smoke-unavailable demo-accept demo-down demo-clean ensure-master-key

TEST_REDIS_ADDR ?= 127.0.0.1:16379
TEST_POSTGRES_DSN ?= postgres://gateway_test:test_only_local@127.0.0.1:15433/gateway_test?sslmode=disable
TEST_PACKAGES ?= ./...
TEST_RUN ?= .
TEST_FLAGS ?= -race

tidy:
	go mod tidy

up: ensure-master-key
	HOST_UID="$$(id -u)" HOST_GID="$$(id -g)" docker compose up -d --build --wait

down:
	docker compose down

demo-up: up

demo-observability: ensure-master-key
	HOST_UID="$$(id -u)" HOST_GID="$$(id -g)" docker compose --profile observability up -d --build --wait

demo-config:
	go run ./cmd/democtl configure

demo-smoke:
	go run ./cmd/democtl request -expect-provider primary

# Stop the primary mock first; this proves pre-first-event failover.
demo-smoke-backup:
	go run ./cmd/democtl request -expect-provider backup

# Stop both mocks first; this proves failure is explicit and no hidden mock is used.
demo-smoke-unavailable:
	go run ./cmd/democtl request -expect-status 502

# Three complete primary/failover/outage/reconciliation runs on one checkout.
demo-accept:
	bash scripts/demo-accept.sh

demo-down: down

# Explicitly destructive: removes PostgreSQL, Redis, Grafana and spool volumes.
demo-clean:
	docker compose down -v

ensure-master-key:
	go run ./cmd/democtl ensure-master-key

run:
	go run ./cmd/gateway

# Local OpenAI-compatible fakes. Gateway treats these as real HTTP providers.
mock-primary:
	go run ./cmd/upstream-mock -listen :18080 -name primary -ttft 20ms -token-interval 15ms

mock-backup:
	go run ./cmd/upstream-mock -listen :18081 -name backup -ttft 80ms -token-interval 15ms

vet:
	go vet ./...

test:
	go test ./... -count=1

# Dedicated disposable dependencies; never reuse the demo/production volumes.
test-up:
	docker compose -f docker-compose.test.yml up -d --wait

test-down:
	docker compose -f docker-compose.test.yml down

# Live provider calls are always opt-in through a separate command.
integration-test: test-up
	env -u OPENAI_BASE_URL -u OPENAI_API_KEY -u OPENAI_MODEL \
		METERING_TEST_REDIS_ADDR='$(TEST_REDIS_ADDR)' \
		LIMITER_TEST_REDIS_ADDR='$(TEST_REDIS_ADDR)' \
		CONTROL_TEST_POSTGRES_DSN='$(TEST_POSTGRES_DSN)' \
		METERING_TEST_POSTGRES_DSN='$(TEST_POSTGRES_DSN)' \
		go test $(TEST_FLAGS) $(TEST_PACKAGES) -run '$(TEST_RUN)' -count=1 -timeout=120s

# Explicit schema changes; export POSTGRES_DSN for the intended database.
.PHONY: migrate-up migrate-version migrate-down-one
migrate-up:
	go run ./cmd/migrate up

migrate-version:
	go run ./cmd/migrate version

# Rolls back one version. Review its down.sql before use.
migrate-down-one:
	go run ./cmd/migrate down-one

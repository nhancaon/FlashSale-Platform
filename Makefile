SHELL := bash
COMPOSE := docker compose --env-file .env -f deploy/compose/docker-compose.yml
# Redis published by `make up` (host port from .env, default 6380) as seen from a container.
TEST_REDIS_ADDR ?= host.docker.internal:6380
GO_IMAGE ?= golang:1.26
# Run a command inside a Linux Go container (race detector needs cgo + gcc, absent on Windows).
# MSYS_NO_PATHCONV stops Git Bash rewriting /src; do not set it globally (it breaks mvnw).
GO_DOCKER = MSYS_NO_PATHCONV=1 docker run --rm -v "$(CURDIR):/src" -v flashsale-gomod:/go/pkg/mod -v flashsale-gobuild:/root/.cache/go-build -e TEST_REDIS_ADDR=$(TEST_REDIS_ADDR)

GO_SERVICES := ratelimiter-go inventory-go
JAVA_SERVICES := ratelimiter-java inventory-java

.PHONY: help up down logs ps db-migrate db-shell db-reset test test-go test-go-race test-java check-lua contract-test

help: ## Liệt kê lệnh
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ -/'

.env:
	cp .env.example .env

up: .env ## Chạy hạ tầng (Oracle, Redis, Kafka, Prometheus, Grafana) và chờ healthy
	$(COMPOSE) up -d --wait

down: .env ## Dừng hạ tầng, giữ dữ liệu
	$(COMPOSE) down

logs: .env ## Xem log (SERVICE=oracle để lọc)
	$(COMPOSE) logs -f --tail=100 $(SERVICE)

ps: .env ## Trạng thái container
	$(COMPOSE) ps

db-migrate: ## Áp dụng db/migrations lên Oracle (idempotent)
	bash db/migrate.sh

db-shell: .env ## Mở sqlplus vào Oracle
	@. ./.env && $(COMPOSE) exec oracle sqlplus $$APP_USER/$$APP_USER_PASSWORD@//localhost:1521/$${ORACLE_SERVICE:-FREEPDB1}

db-reset: .env ## XOÁ toàn bộ dữ liệu Oracle rồi dựng lại và migrate
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait
	bash db/migrate.sh

test: check-lua test-go test-java ## Chạy toàn bộ test (cần Docker cho Testcontainers)

test-go: ## go test cho mọi service Go (Testcontainers Redis)
	@for s in $(GO_SERVICES); do echo "== $$s"; (cd services/$$s && go vet ./... && go test -count=1 ./...) || exit 1; done

test-go-race: .env ## go test -race trong container Linux (cần `make up` để có Redis)
	@for s in $(GO_SERVICES); do echo "== $$s (race)"; $(GO_DOCKER) -w /src/services/$$s $(GO_IMAGE) sh -c 'go vet ./... && go test -race -count=1 ./...' || exit 1; done

test-java: ## ./mvnw test cho mọi service Java
	@for s in $(JAVA_SERVICES); do echo "== $$s"; (cd services/$$s && ./mvnw -B -q test) || exit 1; done

check-lua: ## Script Lua của rate limiter phải giống nhau giữa Go và Java
	@for f in fixed_window sliding_window token_bucket; do \
	  diff -q services/ratelimiter-go/internal/limiter/scripts/$$f.lua services/ratelimiter-java/src/main/resources/scripts/$$f.lua || exit 1; \
	done; echo "lua scripts identical"

contract-test: ## Contract test cho inventory-go và inventory-java x 3 chiến lược (cần make up + db-migrate)
	bash contract-tests/run.sh

SHELL := bash
COMPOSE := docker compose --env-file .env -f deploy/compose/docker-compose.yml
GO_IMAGE ?= golang:1.26
# Tests of the Go services that need Oracle / Kafka read these (values come from .env, nothing is hard coded).
LOAD_ENV = set -a; . ./.env; set +a; export DB_PASSWORD=$$APP_USER_PASSWORD KAFKA_BROKERS=localhost:29092 TEST_REDIS_ADDR=localhost:$${REDIS_HOST_PORT:-6380};
# Run a command inside a Linux Go container (race detector needs cgo + gcc, absent on Windows). The container joins
# the compose network, so it reaches the infrastructure of `make up` by service name (oracle, redis, kafka).
# MSYS_NO_PATHCONV stops Git Bash rewriting /src; do not set it globally (it breaks mvnw).
GO_DOCKER = MSYS_NO_PATHCONV=1 docker run --rm --network flashsale_default -v "$(CURDIR):/src" -v flashsale-gomod:/go/pkg/mod -v flashsale-gobuild:/root/.cache/go-build -e TEST_REDIS_ADDR=redis:6379 -e DB_HOST=oracle -e DB_PASSWORD -e KAFKA_BROKERS=kafka:9092

GO_SERVICES := ratelimiter-go inventory-go outbox-worker notification gateway
JAVA_SERVICES := ratelimiter-java inventory-java order

.PHONY: lab-go lab-java lab-report lab-up lab-images lab-site lab-check lab-rolling lab-down lab-destroy lint security-scan loadtest-smoke db-tune trace-check dashboard env-sync e2e-gateway up-apps down-apps e2e e2e-outbox chaos help up down logs ps db-migrate db-shell db-reset test test-go test-go-race test-java check-lua contract-test

help: ## Liệt kê lệnh
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | sed 's/:.*##/ -/'

.env:
	cp .env.example .env

env-sync: ## Thêm vào .env các biến mới của .env.example (không đổi giá trị cũ)
	@bash scripts/sync-env.sh

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

db-tune: ## Tăng redo log Oracle lên 3 x 512 MB (idempotent; cần trước load test)
	bash db/tune-redo.sh

db-shell: .env ## Mở sqlplus vào Oracle
	@. ./.env && $(COMPOSE) exec oracle sqlplus $$APP_USER/$$APP_USER_PASSWORD@//localhost:1521/$${ORACLE_SERVICE:-FREEPDB1}

db-reset: .env ## XOÁ toàn bộ dữ liệu Oracle rồi dựng lại và migrate
	$(COMPOSE) down -v
	$(COMPOSE) up -d --wait
	bash db/migrate.sh

test: check-lua test-go test-java ## Chạy toàn bộ test (cần Docker cho Testcontainers)

# The outbox-worker integration tests seed events and expect only their own workers to claim them. Running
# outbox-worker containers (make up-apps) would steal those rows, so they are stopped for the test run and restarted after.
PAUSE_OUTBOX = ids=$$(docker ps -q --filter "name=flashsale-outbox-worker"); if [ -n "$$ids" ]; then echo "pausing outbox-worker containers during the tests"; docker stop $$ids > /dev/null; trap 'docker start $$ids > /dev/null' EXIT; fi;

test-go: .env ## go test cho mọi service Go (cần make up + make db-migrate cho outbox-worker, notification)
	@$(LOAD_ENV) $(PAUSE_OUTBOX) for s in $(GO_SERVICES); do echo "== $$s"; (cd services/$$s && go vet ./... && go test -count=1 ./...) || exit 1; done

test-go-race: .env ## go test -race trong container Linux (cần make up + make db-migrate)
	@$(LOAD_ENV) $(PAUSE_OUTBOX) for s in $(GO_SERVICES); do echo "== $$s (race)"; $(GO_DOCKER) -w /src/services/$$s $(GO_IMAGE) sh -c 'go vet ./... && go test -race -count=1 ./...' || exit 1; done

test-java: ## ./mvnw test cho mọi service Java
	@for s in $(JAVA_SERVICES); do echo "== $$s"; (cd services/$$s && ./mvnw -B -q test) || exit 1; done

lint: ## Như CI: golangci-lint (services/.golangci.yml) + Checkstyle (services/checkstyle.xml) + actionlint
	@for s in $(GO_SERVICES); do echo "== golangci-lint $$s"; MSYS_NO_PATHCONV=1 docker run --rm -v "$$PWD/services:/src" -v flashsale-gomod:/go/pkg/mod -v flashsale-gocache:/root/.cache -w /src/$$s golangci/golangci-lint:v2.14 golangci-lint run ./... || exit 1; done
	@for s in $(JAVA_SERVICES); do echo "== checkstyle $$s"; (cd services/$$s && ./mvnw -B -q checkstyle:check) || exit 1; done
	@MSYS_NO_PATHCONV=1 docker run --rm -v "$$PWD:/repo" -w /repo rhysd/actionlint:latest -no-color -oneline && echo "workflows ok"

security-scan: ## Như CI: Trivy trên dependency Go, cấu hình, và image đã build (cần make up-apps trước)
	@MSYS_NO_PATHCONV=1 docker run --rm -v "$$PWD:/repo" -v flashsale-trivy:/root/.cache -w /repo aquasec/trivy:latest fs --quiet --scanners vuln --severity CRITICAL,HIGH --ignore-unfixed --exit-code 1 --skip-dirs services/ratelimiter-java,services/inventory-java,services/order services
	@MSYS_NO_PATHCONV=1 docker run --rm -v "$$PWD:/repo" -v flashsale-trivy:/root/.cache -w /repo aquasec/trivy:latest config --quiet --severity CRITICAL,HIGH --exit-code 1 .
	@for i in $$(docker images --format '{{.Repository}}' | grep '^flashsale/' | sort -u); do echo "== $$i"; MSYS_NO_PATHCONV=1 docker run --rm -v /var/run/docker.sock:/var/run/docker.sock -v flashsale-trivy:/root/.cache aquasec/trivy:latest image --quiet --severity CRITICAL --ignore-unfixed --exit-code 1 $$i:latest || exit 1; done

loadtest-smoke: ## k6 smoke qua gateway + reconcile (cần make up-apps với RL_IP_LIMIT lớn, xem loadtest.yml)
	bash loadtest/smoke.sh

# ---- Concurrency lab (Phase 2b): cùng 10 thí nghiệm Go/Java trong container --cpus 2 --memory 3g ----
lab-go: ## Chạy mọi thí nghiệm Go -> labs/concurrency-lab/results/go.jsonl (~1 phút)
	bash labs/concurrency-lab/run.sh go

lab-java: ## Chạy mọi thí nghiệm Java -> results/java.jsonl (~20 phút, 1M platform thread mất ~15 phút)
	bash labs/concurrency-lab/run.sh java

lab-report: ## Gom kết quả -> docs/concurrency-comparison.md + docs/img/lab-*.svg (flame graph, biểu đồ)
	node labs/concurrency-lab/report.mjs

# ---- Ansible lab (Phase 8): 3 container "VM" + controller; cần ~5 GB RAM cho Docker, nên make down trước ----
lab-up: env-sync ## Dựng 3 node lab (systemd + SSH) và controller Ansible
	bash scripts/lab.sh up

lab-images: ## Build image service, tag theo commit, lưu tar cho Ansible
	bash scripts/lab.sh images

lab-site: ## ansible-playbook site.yml: common, docker, oracle, redis_kafka, monitoring, k3s, app_deploy
	bash scripts/lab.sh ansible site.yml

lab-check: ## site.yml --check --diff (chạy lại phải không còn thay đổi)
	bash scripts/lab.sh check

lab-rolling: ## rolling-update.yml (serial: 1) trong lúc gọi gateway mỗi giây
	bash scripts/lab.sh rolling

lab-down: ## Dừng lab (giữ volume)
	bash scripts/lab.sh down

lab-destroy: ## Xoá lab và volume
	bash scripts/lab.sh destroy

check-lua: ## Script Lua của rate limiter phải giống nhau giữa Go và Java
	@for f in fixed_window sliding_window token_bucket; do \
	  diff -q services/ratelimiter-go/pkg/limiter/scripts/$$f.lua services/ratelimiter-java/src/main/resources/scripts/$$f.lua || exit 1; \
	done; echo "lua scripts identical"

contract-test: ## Contract test cho inventory-go và inventory-java x 3 chiến lược (cần make up + db-migrate)
	bash contract-tests/run.sh

# ---- application stack (needs make up + make db-migrate first) ----
INVENTORY_IMPL ?= go
OUTBOX_REPLICAS ?= 3
COMPOSE_APPS = $(COMPOSE) -f deploy/compose/docker-compose.apps.yml

up-apps: env-sync ## Build và chạy order, inventory, outbox-worker (x OUTBOX_REPLICAS), notification
	INVENTORY_IMPL=$(INVENTORY_IMPL) $(COMPOSE_APPS) --profile inventory-$(INVENTORY_IMPL) up -d --build --wait --scale outbox-worker=$(OUTBOX_REPLICAS) order inventory-$(INVENTORY_IMPL) outbox-worker notification gateway

down-apps: .env ## Dừng các service ứng dụng
	$(COMPOSE_APPS) --profile inventory-go --profile inventory-java --profile ratelimiter-go --profile ratelimiter-java stop order inventory-go inventory-java ratelimiter-go ratelimiter-java outbox-worker notification gateway

e2e: ## End-to-end: tạo đơn qua order -> inventory -> Oracle -> outbox (cần make up-apps)
	bash scripts/e2e-order.sh

e2e-outbox: ## 3 outbox worker, giết 1 giữa chừng: không mất event, mỗi event 1 thông báo (cần make up-apps)
	bash scripts/e2e-outbox.sh

e2e-gateway: ## Qua gateway: login, tạo đơn, 401/405/429 (cần make up-apps)
	bash scripts/e2e-gateway.sh

chaos: ## Chaos test: tắt inventory, breaker mở, tự hồi phục (cần make up-apps)
	bash scripts/chaos-order.sh

trace-check: ## Gửi 1 đơn qua gateway và kiểm tra Jaeger có 1 trace gateway -> order -> inventory (cần make up-apps)
	bash scripts/check-tracing.sh

dashboard: ## Sinh lại Grafana dashboard JSON và kiểm tra từng query với Prometheus
	node scripts/gen-dashboard.mjs && node scripts/check-dashboard.mjs

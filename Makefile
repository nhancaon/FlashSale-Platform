SHELL := bash
COMPOSE := docker compose --env-file .env -f deploy/compose/docker-compose.yml

.PHONY: help up down logs ps db-migrate db-shell db-reset test

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

test: ## Chạy test (chưa có service nào ở Phase 0)
	@echo "Chưa có test: sẽ thêm từ Phase 1."

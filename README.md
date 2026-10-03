# FlashSale Platform

Hệ thống flash sale (bán hàng giới hạn, tải đột biến) dùng để so sánh **Go vs Java** trên cùng một API contract, với Oracle làm source of truth. Spec đầy đủ: [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md). Hướng dẫn làm việc với Claude Code: [CLAUDE_CODE_WORKFLOW.md](CLAUDE_CODE_WORKFLOW.md).

## Trạng thái

| Phase | Nội dung | Trạng thái |
|---|---|---|
| 0 | Khung repo, Compose, migration | Xong, đã chạy thật |
| 1 | Rate limiter (Go + Java) | Xong: test pass, `-race` sạch; benchmark sơ bộ chưa đáng tin (xem `loadtest/results/phase1-ratelimiter/README.md`) |
| 2 | Inventory (Go + Java) | Xong: cùng contract test pass cho 3 chiến lược x 2 ngôn ngữ, 0 oversell; bảng so sánh ở `loadtest/results/phase2-inventory/README.md` |
| 3 | Order service (saga, idempotency, outbox, Resilience4j) | Xong: 19 test, `make e2e` và `make chaos` qua với cả inventory-go và inventory-java |
| 4 | Outbox worker + Notification (Go) | Xong: 3 worker, giết 1 giữa chừng không mất event, mỗi event 1 thông báo (`make e2e-outbox`) |
| 5 | Gateway (Go): JWT, rate limit, circuit breaker, proxy | Xong: chuỗi middleware có test, `make e2e-gateway` qua |

## Yêu cầu
- Docker Desktop (đang chạy), `make`, Git Bash (Windows)

## Chạy nhanh

```bash
make up           # Oracle, Redis, Kafka, Prometheus, Grafana (lần đầu Oracle khởi động vài phút)
make db-migrate   # tạo bảng + dữ liệu mẫu
make db-shell     # sqlplus, thử: SELECT * FROM stock;
make down         # dừng, giữ dữ liệu  (make db-reset để xoá sạch)
```

| Dịch vụ | Địa chỉ |
|---|---|
| Oracle | `localhost:1521/FREEPDB1` (user/mật khẩu trong `.env`) |
| Redis | `localhost:6380` (đổi bằng `REDIS_HOST_PORT`) |
| Kafka (từ host) | `localhost:29092` |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 |

Mật khẩu dev nằm trong `.env.example`; `make up` tự tạo `.env` từ đó. Không commit `.env`.

## Cấu trúc

```
api/ db/migrations/ services/ contract-tests/ loadtest/
deploy/compose/ deploy/k8s/ ansible/ labs/concurrency-lab/ docs/
```

Sơ đồ kiến trúc: xem mục 2 của spec; sẽ vẽ lại trong `docs/architecture.md` khi có service đầu tiên.

## Rate limiter (Phase 1)

```bash
# Go (cổng 8081) và Java (cổng 8082), cùng contract
cd services/ratelimiter-go   && REDIS_ADDR=localhost:6380 go run ./cmd/ratelimiter
cd services/ratelimiter-java && ./mvnw spring-boot:run

curl -X POST localhost:8081/v1/check -H 'Content-Type: application/json' \
  -d '{"key":"user:42","limit":5,"windowSec":60}'
# {"allowed":true,"remaining":4,"retryAfterMs":0}
```

Cấu hình bằng env: `RATELIMIT_ALGORITHM` (`token_bucket` | `sliding_window` | `fixed_window`), `REDIS_ADDR` (Go) hoặc `REDIS_HOST`/`REDIS_PORT` (Java), `PORT`, `CHECK_TIMEOUT_MS`. Metrics ở `/metrics`: `ratelimit_allowed_total`, `ratelimit_rejected_total`, `ratelimit_check_duration_seconds`.

Test: `make test` (Testcontainers), `make test-go-race` (cần `make up`; chạy `-race` trong container Linux vì Windows không có gcc).

## Inventory (Phase 2)

```bash
make up && make db-migrate
export DB_PASSWORD=dev_app_password     # mật khẩu trong .env (APP_USER_PASSWORD)
cd services/inventory-go   && STOCK_STRATEGY=atomic go run ./cmd/inventory       # cổng 8083
cd services/inventory-java && STOCK_STRATEGY=atomic ./mvnw spring-boot:run       # cổng 8084

curl -X POST localhost:8083/v1/inventory/reserve -H 'Content-Type: application/json' \
  -d '{"orderId":"o-1","sku":"SKU-IPHONE","qty":2}'
```

API: `GET /v1/inventory/{sku}`, `POST /v1/inventory/{reserve|release|confirm}` (idempotent), `/healthz`, `/readyz`, `/metrics`.
Env: `STOCK_STRATEGY` (`atomic` | `pessimistic` | `optimistic`), `DB_*`, `DB_POOL_MAX`, `REDIS_ADDR` (Go) / `REDIS_HOST`+`REDIS_PORT` (Java), `CACHE_ENABLED`, `CACHE_TTL_MS`.

Test: `make test` (unit), `make contract-test` (cùng một bộ test chạy vào cả hai service x 3 chiến lược, gồm test 2000 người mua tranh 100 hàng). Thiết kế: `docs/adr/0003-*.md`, `0004-*.md`.

## Order (Phase 3)

```bash
make up && make db-migrate
make up-apps INVENTORY_IMPL=go        # hoặc java: build + chạy order và inventory trong Docker
make e2e                              # tạo đơn, replay, hết hàng + bù trừ, outbox
make chaos                            # tắt inventory: breaker mở, order vẫn nhanh, tự hồi phục

curl -X POST localhost:8085/v1/orders -H 'Content-Type: application/json' \
  -H 'X-User-Id: u1' -H 'Idempotency-Key: abc-123' \
  -d '{"items":[{"sku":"SKU-IPHONE","qty":1}]}'
```

`POST /v1/orders` (header `Idempotency-Key` và `X-User-Id` bắt buộc): 201 đơn mới, 200 replay, 202 saga chưa xong (PENDING),
409 `OUT_OF_STOCK` / `PAYMENT_DECLINED` / `IDEMPOTENCY_KEY_REUSED` / `REQUEST_IN_PROGRESS`, 503 `INVENTORY_UNAVAILABLE`.
`GET /v1/orders/{id}`. Thiết kế: `docs/adr/0005-*.md`. Ghi chú: Order ghi `outbox_events` cùng transaction; Phase 4 sẽ đọc bảng này.

## Outbox worker + Notification (Phase 4)

```bash
make up-apps INVENTORY_IMPL=go   # chạy thêm 3 outbox-worker (OUTBOX_REPLICAS) và notification
make e2e-outbox                  # 150 đơn, 3 worker, giết 1 worker bằng SIGKILL: không mất event, 1 thông báo / đơn
```

Order ghi `outbox_events` cùng transaction; các worker tranh nhau bằng `FOR UPDATE SKIP LOCKED`, gửi lên Kafka topic `flashsale.order-events`
(key = aggregate id nên mỗi đơn nằm trong 1 partition, giữ thứ tự). Notification là consumer idempotent theo event id (bảng `processed_events`).
Env chính: `OUTBOX_WORKERS`, `OUTBOX_BATCH_SIZE`, `OUTBOX_MAX_ATTEMPTS`, `KAFKA_BROKERS`, `KAFKA_TOPIC`. Metrics: `outbox_pending_events` (độ trễ relay), `outbox_published_total`.
Thiết kế và các phát hiện về Oracle (ORA-02014, ROWNUM làm worker "đói"): `docs/adr/0006-*.md`, `0007-*.md`.

## Gateway (Phase 5)

```bash
make up-apps INVENTORY_IMPL=go        # gateway nghe ở cổng 8088 (cần JWT_SECRET, DEMO_PASSWORD: make env-sync tự thêm vào .env)
make e2e-gateway

TOKEN=$(curl -s -X POST localhost:8088/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"alice","password":"<DEMO_PASSWORD trong .env>"}' | jq -r .accessToken)
curl -X POST localhost:8088/api/orders -H "Authorization: Bearer $TOKEN" -H 'Idempotency-Key: k-1' \
  -H 'Content-Type: application/json' -d '{"items":[{"sku":"SKU-IPHONE","qty":1}]}'
```

Chuỗi: request id → log/metrics → rate limit IP → JWT → rate limit user → circuit breaker → reverse proxy. Route công khai: `POST /auth/login`,
`/api/orders[/{id}]` (GET, POST), `GET /api/inventory/{sku}` (reserve/release/confirm là nội bộ). `GET /readyz` tổng hợp sức khoẻ order, inventory, Redis.
Env: `RATELIMIT_MODE` (`embedded` dùng thư viện ratelimiter-go, `remote` gọi service ratelimiter), `RL_USER_LIMIT`, `RL_IP_LIMIT`, `RATELIMIT_FAIL_OPEN`, `TRUST_PROXY`.
Thiết kế: `docs/adr/0008-*.md`.

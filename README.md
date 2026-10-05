# FlashSale Platform

Hệ thống flash sale (bán hàng giới hạn, tải đột biến) dùng để so sánh **Go vs Java** trên cùng một API contract, với Oracle làm source of truth. Spec đầy đủ: [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md). Hướng dẫn làm việc với Claude Code: [CLAUDE_CODE_WORKFLOW.md](CLAUDE_CODE_WORKFLOW.md).

## Trạng thái

| Phase | Nội dung | Trạng thái |
|---|---|---|
| 0 | Khung repo, Compose, migration | Xong, đã chạy thật |
| 1 | Rate limiter (Go + Java) | Xong: test pass, `-race` sạch; benchmark sơ bộ chưa đáng tin (xem `loadtest/results/phase1-ratelimiter/README.md`) |
| 2 | Inventory (Go + Java) | Xong: cùng contract test pass cho 3 chiến lược x 2 ngôn ngữ, 0 oversell; bảng so sánh ở `loadtest/results/phase2-inventory/README.md` |
| 2b | Concurrency lab Go vs Java (10 thí nghiệm) | Xong: `make lab-go`, `make lab-java`, `make lab-report` → [docs/concurrency-comparison.md](docs/concurrency-comparison.md) (bảng, biểu đồ, flame graph, so sánh code) |
| 3 | Order service (saga, idempotency, outbox, Resilience4j) | Xong: 19 test, `make e2e` và `make chaos` qua với cả inventory-go và inventory-java |
| 4 | Outbox worker + Notification (Go) | Xong: 3 worker, giết 1 giữa chừng không mất event, mỗi event 1 thông báo (`make e2e-outbox`) |
| 5 | Gateway (Go): JWT, rate limit, circuit breaker, proxy | Xong: chuỗi middleware có test, `make e2e-gateway` qua |
| 6a | Observability: metrics, Grafana dashboard, tracing (Jaeger) | Xong: dashboard provisioned, 1 trace xuyên gateway-order-inventory (`make trace-check`) |
| 6b | Load test + benchmark Go vs Java | Xong: 6 lượt x 600 VU, reconcile PASS cả 6; tìm và sửa 3 lỗi thật. Báo cáo: [docs/benchmark-report.md](docs/benchmark-report.md) |
| 7 | CI/CD (GitHub Actions) | Workflow ci-go, ci-java, docker (Trivy + GHCR + bảng size), security, ansible (lint + dựng lab thật + kiểm idempotency), loadtest nightly; các bước đã chạy sạch trên máy (`make lint`, `make security-scan`, `make loadtest-smoke`), xem tab Actions cho lần chạy trên GitHub. ADR 0010 |
| 8 | Ansible + k3s (lab 3 node container) | Xong: site.yml dựng toàn bộ từ máy trắng, idempotent (changed=0), đơn hàng CONFIRMED qua gateway trên k3s, rolling update serial: 1 đo được (ADR 0011) |

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

Sơ đồ kiến trúc, luồng một đơn hàng và các đảm bảo đúng đắn (kèm test chứng minh): [docs/architecture.md](docs/architecture.md).

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

## Observability (Phase 6a)

| Công cụ | Địa chỉ |
|---|---|
| Grafana (dashboard **FlashSale overview**, tự nạp) | http://localhost:3000 (admin / mật khẩu trong `.env`) |
| Prometheus | http://localhost:9090 |
| Jaeger (trace) | http://localhost:16686 |

`make trace-check` gửi một đơn qua gateway rồi kiểm tra có đúng một trace đi qua gateway → order → inventory → Oracle. `make dashboard` sinh lại
dashboard JSON và chạy thử mọi query. Thiết kế và giới hạn: `docs/adr/0009-observability.md`.

## Load test và benchmark (Phase 6b)

```bash
make up && make db-migrate
bash loadtest/run-benchmark.sh      # tự chạy make db-tune (redo log 3 x 512 MB); ~35 phút cho 3 vòng x go/java
node loadtest/aggregate.mjs         # bảng summary.md + biểu đồ docs/img/benchmark.svg
bash loadtest/reconcile.sh <sku> <stock>   # đối soát DB sau một lượt (tồn kho, đơn treo, key trùng, event, thông báo)
```

Kết quả (600 VU, cùng giới hạn 2 CPU / 1 GiB): throughput và latency **ngang nhau** (~265 đơn/s, p95 ~33 ms) vì nút
cổ chai là order + Oracle, không phải inventory; khác biệt nằm ở tài nguyên: Go dùng bộ nhớ ít hơn ~10 lần, khởi động
0,6 s so với 5,6 s, image 14 MiB so với 134 MiB. Chi tiết, phương pháp, giới hạn và 3 lỗi load test tìm ra:
[docs/benchmark-report.md](docs/benchmark-report.md).

![benchmark](docs/img/benchmark.svg)

## CI/CD (Phase 7)

| Workflow | Khi nào | Làm gì |
|---|---|---|
| `ci-go` | push/PR sửa code Go | golangci-lint, check-lua, `go test -race` + coverage trên Oracle/Redis/Kafka thật (compose) |
| `ci-java` | push/PR sửa code Java | `./mvnw verify`: test (Testcontainers), Checkstyle, JaCoCo |
| `docker` | push/PR sửa services | build 8 image, kiểm tra non-root, Trivy (chặn CRITICAL có bản vá), push GHCR từ main, bảng size Go vs Java |
| `security` | push, PR, hằng tuần | Trivy dependency Go + cấu hình (Dockerfile, compose) |
| `ansible` | sửa `ansible/`, `deploy/lab`, `deploy/k8s` | ansible-lint, dựng lab 3 node, site.yml, đặt 1 đơn qua gateway k3s, chạy lại phải `changed=0` |
| `loadtest` | hằng đêm, thủ công | cả stack với inventory go và java: e2e + k6 smoke + reconcile |

Chạy lại trên máy: `make lint`, `make security-scan`, `make loadtest-smoke`. Quyết định và giới hạn: `docs/adr/0010-ci-cd.md`.

## Ansible + k3s (Phase 8)

Ba container Ubuntu (systemd + SSH) đóng vai VM, thêm một container controller chạy Ansible (`deploy/lab`, ADR 0011):
`lab-data` (Docker, Oracle, Redis, Kafka, Prometheus, Grafana), `lab-k3s-server`, `lab-k3s-agent` (k3s + ứng dụng).

```bash
make down            # lab cần ~5 GB RAM của Docker
make lab-up          # dựng 3 node + controller, cài khoá SSH bootstrap
make lab-images      # build image, tag theo commit, lưu tar cho Ansible
make lab-site        # ansible-playbook site.yml (chạy lại: changed=0)
make lab-rolling     # rolling-update.yml (serial: 1) + đo gateway mỗi giây
make lab-down        # dừng (make lab-destroy để xoá cả volume)
```

Gateway trên k3s: http://localhost:18088 (mật khẩu demo nằm trong vault: `docker exec -w /repo/ansible lab-controller
ansible-vault view group_vars/all/vault.yml`). Grafana của lab: http://localhost:13000, Prometheus: http://localhost:19090.

| Role | Làm gì |
|---|---|
| common | user `deploy` (sudo, chỉ SSH key), tắt root SSH và mật khẩu, ufw, node_exporter |
| docker | Docker (gói Ubuntu) trên node data |
| oracle | Oracle Free, redo log 512 MB, migration bằng chính `db/migrate.sh` |
| redis_kafka | Redis, Kafka KRaft quảng bá IP node data |
| monitoring | Prometheus (node_exporter mọi node + NodePort ứng dụng), Grafana cùng dashboard |
| k3s | server rồi agent, version cố định, token từ vault, CoreDNS 2 replica + PDB |
| app_deploy | nạp image vào containerd mọi node, Secret từ vault, manifest `deploy/k8s/*.j2`, chờ rollout |

Secret nằm trong `ansible/group_vars/all/vault.yml` (Ansible Vault, mật khẩu từ `FLASHSALE_VAULT_PASSWORD`).
Rolling update đo được: trước khi có PodDisruptionBudget và 2 CoreDNS, mỗi lần drain mất cả nền tảng ~18 s; sau khi sửa
còn tối đa 3 s trên chính node đang restart (load balancer có health check sẽ bỏ qua node đó).

## Concurrency lab (Phase 2b)

Mười thí nghiệm giống hệt nhau viết bằng Go và Java 25 (goroutine vs platform/virtual thread, worker pool, fan-out I/O,
bộ đếm dùng chung, race detector, hàng đợi có giới hạn, huỷ task, deadlock/leak, pipeline, rate limiter), chạy trong
container cùng giới hạn 2 CPU / 3 GiB: `make lab-go`, `make lab-java`, `make lab-report`. Chi tiết và cách chạy riêng
từng thí nghiệm: [labs/concurrency-lab/README.md](labs/concurrency-lab/README.md). Kết quả trung thực, có cả chỗ Java
thắng (CPU-bound, stream, tự phát hiện deadlock): [docs/concurrency-comparison.md](docs/concurrency-comparison.md).

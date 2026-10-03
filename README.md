# FlashSale Platform

Hệ thống flash sale (bán hàng giới hạn, tải đột biến) dùng để so sánh **Go vs Java** trên cùng một API contract, với Oracle làm source of truth. Spec đầy đủ: [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md). Hướng dẫn làm việc với Claude Code: [CLAUDE_CODE_WORKFLOW.md](CLAUDE_CODE_WORKFLOW.md).

## Trạng thái

| Phase | Nội dung | Trạng thái |
|---|---|---|
| 0 | Khung repo, Compose, migration | Xong, đã chạy thật |
| 1 | Rate limiter (Go + Java) | Xong: test pass, `-race` sạch; benchmark sơ bộ chưa đáng tin (xem `loadtest/results/phase1-ratelimiter/README.md`) |
| 2 | Inventory (Go + Java) | Chưa làm |
| 3 | Order service | Chưa làm |

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

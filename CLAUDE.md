# FlashSale Platform

Hệ thống flash sale để so sánh Go vs Java (cùng API contract) trên Oracle. Spec đầy đủ: `docs/PROJECT_SPEC.md` (chỉ đọc mục cần thiết, đừng nạp cả file).

## Quy ước
- Làm đúng một phase mỗi lần. Trước khi code, nói ngắn kế hoạch và liệt kê file sẽ tạo/sửa. Không làm trước phase sau.
- Mọi thay đổi có test. Go chạy `go test -race ./...`.
- Không hard-code secret; dùng env và `.env.example`. Không commit `.env`.
- Mọi truy vấn dùng bind variable, không nối chuỗi SQL.
- Mọi call ra ngoài có timeout; mọi goroutine/thread pool có cách dừng (context/shutdown).
- Log JSON có cấu trúc, kèm request id.
- Giữ Go và Java tương đương về hành vi; nếu khác thì ghi lý do vào ADR (`docs/adr/`).
- Tech stack cố định (spec mục 1), không tự thêm công nghệ. Điểm chưa rõ thì hỏi, không đoán.
- Java: JDK 25 (LTS) + Spring Boot bản ổn định mới nhất; version cụ thể chốt ở Phase 1 và ghi ADR. Go: 1.22+.
- Commit nhỏ, Conventional Commits (`feat:`, `fix:`, `test:`, `chore:`).
- Cuối phase: chạy toàn bộ test, cập nhật README, tóm tắt việc đã làm và còn thiếu.

## Lệnh
- `make up` / `make down` / `make ps` / `make logs SERVICE=oracle`: hạ tầng Compose
- `make db-migrate`: áp dụng `db/migrations` (idempotent); `make db-reset` xoá sạch rồi dựng lại; `make db-tune` tăng redo log (cần trước load test)
- `make db-shell`: sqlplus vào Oracle
- `make test`: check-lua + test Go + test Java (Testcontainers, cần Docker); `make test-go-race`: -race trong container Linux (cần `make up`)
- `make contract-test`: contract test inventory (go + java x 3 chiến lược); cần `make up` + `make db-migrate`
- `make up-apps INVENTORY_IMPL=go|java`: build + chạy order/inventory trong Docker (cổng 8085/8083/8084); `make e2e`, `make chaos`
- `make e2e-outbox`: 3 outbox worker, giết 1 giữa chừng (cần make up-apps). `make test-go` cần make up + db-migrate (outbox-worker/notification test với Oracle + Kafka thật); `make test-go-race` chạy trong mạng compose
- `make e2e-gateway`: login, tạo đơn qua gateway, 401/405/429. `make env-sync` thêm biến mới của .env.example vào .env. Tests Go dừng tạm các container outbox-worker (chúng tranh claim event của test)

## Ghi chú môi trường
- Windows: Makefile dùng `bash` (Git Bash). Script `.sh` phải giữ kết thúc dòng LF.
- Oracle: chuỗi rỗng = NULL, READ COMMITTED mặc định, phân trang `OFFSET ... FETCH NEXT`.
- Kafka: trong mạng compose dùng `kafka:9092`, từ host dùng `localhost:29092`.

## Tiến độ
- Phase 0 (khung repo): xong. `make up`, `make db-migrate` (idempotent), `make db-shell` đã chạy thật.
- Phase 1 (rate limiter Go + Java): xong. Cổng Go 8081, Java 8082. Go 1.26, JDK 25 (Temurin, `JAVA_HOME` đã đặt), Spring Boot 4.1.1 (ADR 0001). Chưa có benchmark đáng tin; làm ở Phase 6 trong container.
- Phase 2 (inventory Go + Java): xong. Cổng Go 8083, Java 8084. Strategy atomic|pessimistic|optimistic, reservation idempotent theo (orderId, sku), cache-aside Redis (ADR 0003/0004). Contract test: `contract-tests/` (Go, qua BASE_URL). inventory-java trên host Windows từ chối burst kết nối mới (Go không bị) -> client test giữ pool 64 kết nối; kiểm tra lại trong container Linux ở Phase 6.
- Phase 3 (order, Java): xong. Cổng 8085. Saga reserve->payment(giả lập)->confirm, payment là điểm pivot, PENDING được resume khi client retry cùng Idempotency-Key (ADR 0005). Migration V3 thêm failure_reason/updated_at. TIMESTAMP đọc bằng LocalDateTime (UTC), KHÔNG dùng java.sql.Timestamp. Metric Prometheus không được kết thúc bằng _created. Dockerfile cho mọi service (non-root), compose: deploy/compose/docker-compose.apps.yml.
- Phase 4 (outbox-worker + notification, Go): xong. Claim bằng SELECT ... FOR UPDATE SKIP LOCKED, đọc đúng N dòng với PREFETCH_ROWS=N (Oracle khoá theo từng fetch; FETCH FIRST + FOR UPDATE bị ORA-02014; ROWNUM làm worker đói) — ADR 0006. At-least-once, consumer idempotent theo event id (ADR 0007). Migration V4. Cổng notification 8087.
- Phase 5 (gateway, Go): xong. Cổng 8088. Chuỗi IP-limit -> JWT -> user-limit -> breaker -> proxy; limiter dùng thư viện ratelimiter-go/pkg/limiter (go.mod replace, Docker context = services/) hoặc service remote; fail-open mặc định (ADR 0008). JWT chỉ HS256, X-User-Id do gateway đặt từ token.
- Phase 6a (observability): xong. Prometheus scrape mọi service (5s), Grafana dashboard sinh bởi scripts/gen-dashboard.mjs, Jaeger 16686, OTel Go (gateway, inventory-go) + OTel Java agent qua JAVA_TOOL_OPTIONS. Tắt tracing khi benchmark: OTEL_ENDPOINT= JAVA_TRACING_OPTS= (rỗng). ADR 0009. Dùng node script phải dùng fileURLToPath (đường dẫn có khoảng trắng).
- Phase 6b (load test + benchmark): xong. `loadtest/run-benchmark.sh` (600 VU, 3 vòng x go/java, giới hạn 2 CPU/1 GiB) -> `node loadtest/aggregate.mjs`; `loadtest/reconcile.sh` đối soát DB sau mỗi lượt. Báo cáo `docs/benchmark-report.md`. Sửa 3 lỗi: breaker tính OUT_OF_STOCK là thành công + gateway không tính 503; job `PendingOrderRecovery` (migration V5); pool Go phải SetMaxIdleConns = MaxOpenConns (ORA-12516). `make db-tune`: redo log Oracle 3 x 512 MB (bản gốc 10 MB làm DB đứng). Không chạy test Java/Go cùng lúc với benchmark (chung Oracle).
- Phase 7 (CI/CD): xong phần code. `.github/workflows/`: ci-go, ci-java, docker (Trivy + GHCR), security, ansible (skip tới Phase 8), loadtest (nightly). Lint: `services/.golangci.yml` (golangci-lint v2), `services/checkstyle.xml` (mvn verify). `make lint`, `make security-scan`, `make loadtest-smoke`. Pin `tomcat.version`/`jackson-bom.version` trong pom vì CVE (bỏ khi Spring Boot cập nhật). `mvnw` và `*.sh` phải có bit thực thi trong git (`git update-index --chmod=+x`). ADR 0010.
- Phase 8 (Ansible + k3s): xong. Lab `deploy/lab`: 3 container Ubuntu systemd+SSH (privileged, `cgroup: private`, volume cho /var/lib/rancher/k3s, /var/lib/docker, /var/lib/containerd, /etc/rancher) + controller Ansible. `make lab-up`, `lab-images`, `lab-site`, `lab-rolling`, `lab-down`. Vault: `ansible/group_vars/all/vault.yml`, mật khẩu `FLASHSALE_VAULT_PASSWORD` (.env). site.yml idempotent (changed=0): đổi thay đổi k8s bằng `kubectl diff`, không tin output của apply (PDB luôn "configured"). Lab cần ~5 GB: `make down` trước. ADR 0011.
- Phase 2b (concurrency lab): xong. `labs/concurrency-lab/{go,java}` cùng CLI `-exp N -variant V -n N`, cùng JSON lines; `run.sh` chạy trong container --cpus 2 --memory 3g, mỗi biến thể 1 process. `make lab-go` (~1 phút), `make lab-java` (~20 phút: 1M platform thread ~15 phút), `make lab-report` sinh `docs/concurrency-comparison.md` từ `report-template.md` (sửa template, không sửa file sinh ra). Flame graph tự vẽ bằng `flame.mjs` (pprof -traces, jfr print). Java cần `--enable-preview` (StructuredTaskScope, JDK 25). JFR in log ra stdout: report bỏ dòng không phải JSON.
- Redis host port là 6380 (`REDIS_HOST_PORT`) vì máy dev có container `redis` khác giữ 6379.
- Git Bash: script gọi `docker exec ... sqlplus /nolog` phải `export MSYS_NO_PATHCONV=1`.

# Compact instructions
Khi nén hội thoại, giữ: phase hiện tại, quyết định kiến trúc đã chốt, danh sách file đã sửa,
test đang fail và nguyên nhân. Bỏ: log dài, khám phá không dẫn tới đâu.

# Không đọc
loadtest/results/raw/, **/target/, **/node_modules/, *.log

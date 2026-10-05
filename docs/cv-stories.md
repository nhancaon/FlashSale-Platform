# FlashSale Platform: ba câu chuyện cho CV và phỏng vấn

Mỗi câu chuyện theo khung **bối cảnh → việc khó → cách làm → kết quả đo được → bài học**, kèm vài gạch đầu dòng tiếng
Anh dùng thẳng cho CV. Mọi con số đều lấy từ repo (đường dẫn trong ngoặc) và tái hiện được bằng lệnh `make`.

---

## 1. Java: đơn hàng đúng tuyệt đối dưới tải (Order service, Oracle, transaction)

**Bối cảnh.** Flash sale: hàng nghìn người cùng mua 100 sản phẩm. Yêu cầu: không bán quá số hàng, người dùng bấm lại
không tạo đơn thứ hai, không mất sự kiện nào, kể cả khi service khác chết giữa chừng.

**Việc khó và cách làm.**
- *Idempotency dưới đồng thời.* Unique `(user_id, idempotency_key)` trong Oracle; request thua cuộc đua bắt
  ORA-00001 rồi trả kết quả đã lưu. Test: 20 request cùng key chạy song song chỉ tạo 1 đơn.
- *Giao dịch phân tán không dùng 2PC.* Saga điều phối reserve → payment → confirm, payment là điểm pivot (đã trừ tiền
  thì chỉ đi tiếp, không hoàn kho). Không giữ connection Oracle khi gọi service khác: 2 transaction ngắn (ADR 0005).
- *Event không mất, không thừa.* Transactional outbox: đơn và event commit cùng transaction.
- *Resilience4j* (timeout, retry có jitter, circuit breaker, bulkhead), chaos test: tắt inventory → 12/12 đơn trả 503
  trong ~90 ms, hệ thống tự hồi phục.
- *Load test tìm ra lỗi thật:* breaker bỏ qua câu trả lời `OUT_OF_STOCK` nên sau khi hết hàng mở mạch nhầm, kéo gateway
  mở theo (~21 000 lỗi 503); đơn PENDING treo mãi khi client bỏ cuộc → thêm job khôi phục, mỗi đơn chỉ một bên "claim"
  bằng `UPDATE ... WHERE updated_at < ...` (test 8 luồng tranh nhau, đúng 1 thắng).

**Kết quả.** 6 lượt x 600 VU: đúng 100/100 đơn bán, 0 oversell, 0 đơn trùng, 0 đơn treo, ~33 000 event/lượt đều gửi,
mỗi đơn đúng 1 thông báo (`docs/benchmark-report.md`, `loadtest/reconcile.sh`).

**Bài học.** Test xanh chưa đủ: chỉ load test + đối soát DB mới lộ lỗi breaker và đơn treo. Ghi TIMESTAMP Oracle
bằng `LocalDateTime` (UTC); `java.sql.Timestamp` lệch giờ làm mọi đơn "cũ" ngay khi tạo.

- Built an idempotent order service (Java 25, Spring Boot 4, Oracle) with an orchestration saga, transactional outbox and
  Resilience4j; verified 0 oversell and 0 duplicate orders across 100 000 orders under 600 concurrent users.
- Found and fixed a circuit-breaker cascade and stuck sagas through load testing with database reconciliation; added a
  recovery job with row-level claiming (regression tests included).

---

## 2. Go: concurrency, worker và benchmark có số liệu

**Bối cảnh.** Viết cùng service bằng Go và Java với cùng contract để so sánh công bằng, và các worker Go chịu tải.

**Việc khó và cách làm.**
- *Outbox relay nhiều worker không xử lý trùng.* `SELECT ... FOR UPDATE SKIP LOCKED` trên Oracle. Phát hiện Oracle
  khoá theo từng lần fetch: `FETCH FIRST` + `FOR UPDATE` lỗi ORA-02014, `ROWNUM` làm worker đói; giải bằng
  `PREFETCH_ROWS = N` đọc đúng N dòng (ADR 0006). Giết 1 trong 3 worker giữa chừng: không mất event.
- *Consumer idempotent* (inbox theo event id) cho at-least-once của Kafka (ADR 0007).
- *Gateway Go:* chuỗi middleware IP limit → JWT → user limit → breaker → reverse proxy; rate limiter Redis + Lua dùng
  chung script với bản Java.
- *Benchmark phải đúng trước khi nhanh:* lần đo đầu Go thua (56% lỗi 503) — nguyên nhân là `SetMaxIdleConns(5)` đóng
  mọi kết nối thứ 6 trở đi, Oracle bị "churn" session tới ORA-12516. Sửa cho tương đương Hikari: 0 lỗi, p95 33 ms.
- *Concurrency lab:* 10 thí nghiệm giống hệt nhau Go vs Java (goroutine vs virtual thread, channel vs BlockingQueue,
  context vs StructuredTaskScope, race detector...), flame graph từ pprof và JFR.

**Kết quả.** inventory-go vs inventory-java cùng 2 CPU: throughput như nhau, Go dùng 26 vs 281 MiB, khởi động 0,6 vs
5,6 s, image 14 vs 134 MiB. Lab: 1 triệu goroutine 1,2 s; channel nhanh hơn ArrayBlockingQueue ~6 lần; Java thắng
CPU-bound (+20%) và stream (2–3 lần) — ghi trung thực (`docs/concurrency-comparison.md`).

**Bài học.** Một cấu hình pool sai làm sai cả kết luận benchmark: kiểm tra "cùng hành vi" trước khi so tốc độ.

- Implemented Go services (outbox relay with Oracle SKIP LOCKED, idempotent Kafka consumer, API gateway with JWT, Redis
  rate limiting and circuit breakers), all tested with the race detector.
- Benchmarked Go vs Java implementations under identical container limits; traced a 56% error rate to connection-pool
  churn (ORA-12516) and fixed it; Go used 10x less memory at equal throughput.
- Built a 10-experiment Go vs Java concurrency lab with reproducible containers, flame graphs and an honest report.

---

## 3. DevOps: CI/CD, Ansible, k3s, observability

**Bối cảnh.** Đưa 8 service lên một cụm Kubernetes dựng từ máy trắng, có CI, quét bảo mật và quan sát được.

**Việc khó và cách làm.**
- *CI GitHub Actions 6 workflow:* lint (golangci-lint v2, Checkstyle), test với Oracle/Kafka thật, coverage, build 8
  image + kiểm tra non-root + Trivy (chặn CRITICAL), push GHCR, bảng so sánh size; Dependabot. Trivy tìm ra Tomcat
  11.0.24 (3 CRITICAL) và Jackson (HIGH) → pin bản vá trước BOM Spring Boot.
- *Ansible + Vault:* 7 role (hardening, Docker, Oracle chạy chính script migration của repo, Redis/Kafka, monitoring,
  k3s server/agent, deploy app). Chạy lại `changed=0` (CI kiểm tra), secret không bao giờ ghi ra file trên node.
- *Lab 3 "VM" bằng container* (máy Windows Home không có VM): tự gỡ 5 vấn đề, ví dụ hai kubelet chung cgroup giết pod
  của nhau, overlayfs lồng nhau, `kubectl apply` báo PDB "configured" mãi (dùng `kubectl diff` để đo thay đổi thật).
- *Rolling update đo được:* bản đầu mỗi lần drain mất cả nền tảng ~18 s (CoreDNS 1 replica, không PDB); sau khi thêm
  PDB + 2 CoreDNS còn tối đa 3 s trên chính node đang restart. HPA theo CPU cho gateway và inventory.
- *Observability:* Prometheus + Grafana (dashboard sinh bằng code), OpenTelemetry tracing Go + Java agent, 1 trace
  xuyên gateway → order → inventory → Oracle.
- *Oracle dưới tải:* redo log mặc định 2 x 10 MB làm DB đứng (`log file switch (checkpoint incomplete)`) → `make db-tune`.

**Kết quả.** `make demo` dựng toàn bộ bằng một lệnh; `site.yml` dựng cụm k3s 3 node từ máy trắng và đặt được đơn
CONFIRMED qua gateway; CI tái hiện được bằng `make lint`, `make security-scan`, `make loadtest-smoke`.

**Bài học.** Đo trước khi tin: "rolling update không downtime" chỉ đúng sau khi đo và sửa CoreDNS/PDB.

- Automated provisioning of a 3-node k3s cluster with Ansible (7 roles, Vault, idempotent, CI-verified) and zero-downtime
  node maintenance (serial: 1 drain with PodDisruptionBudgets; outage cut from ~18 s to ≤3 s, measured).
- Built GitHub Actions CI/CD: lint, tests on real Oracle/Kafka, Trivy-gated image builds pushed to GHCR, nightly k6
  smoke tests with database reconciliation.
- Instrumented services with Prometheus, Grafana and OpenTelemetry; diagnosed an Oracle redo-log stall under load.

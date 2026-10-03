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
- `make db-migrate`: áp dụng `db/migrations` (idempotent); `make db-reset` xoá sạch rồi dựng lại
- `make db-shell`: sqlplus vào Oracle
- `make test`: chạy test (sẽ có từ Phase 1)

## Ghi chú môi trường
- Windows: Makefile dùng `bash` (Git Bash). Script `.sh` phải giữ kết thúc dòng LF.
- Oracle: chuỗi rỗng = NULL, READ COMMITTED mặc định, phân trang `OFFSET ... FETCH NEXT`.
- Kafka: trong mạng compose dùng `kafka:9092`, từ host dùng `localhost:29092`.

## Tiến độ
- Phase 0 (khung repo): xong. `make up`, `make db-migrate` (idempotent), `make db-shell` đã chạy thật.
- Redis host port là 6380 (`REDIS_HOST_PORT`) vì máy dev có container `redis` khác giữ 6379.
- Git Bash: script gọi `docker exec ... sqlplus /nolog` phải `export MSYS_NO_PATHCONV=1`.

# Compact instructions
Khi nén hội thoại, giữ: phase hiện tại, quyết định kiến trúc đã chốt, danh sách file đã sửa,
test đang fail và nguyên nhân. Bỏ: log dài, khám phá không dẫn tới đâu.

# Không đọc
loadtest/results/raw/, **/target/, **/node_modules/, *.log

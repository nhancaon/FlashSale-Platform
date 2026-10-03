# Hướng dẫn học Claude Code qua project FlashSale

Mục tiêu: dùng project này để học các tính năng agent của Claude Code (subagent, plan mode, CLAUDE.md, hooks, MCP, skills) và giữ chi phí token thấp.

> Tính năng Claude Code thay đổi nhanh. Khi lệch với file này, tin tài liệu chính thức: https://code.claude.com/docs

## 1. Lộ trình học (gắn với phase của project)

| Tuần / Phase | Tính năng học | Cách thực hành |
|---|---|---|
| Phase 0 | `CLAUDE.md`, plan mode, `/init`, `/context`, `/usage` | Viết CLAUDE.md ngắn; dùng plan mode để duyệt kế hoạch trước khi sinh code |
| Phase 1–2 | `/clear`, `/compact`, chọn model, prompt có phạm vi rõ | Mỗi service một phiên; xong thì `/clear` |
| Phase 2b, 3 | **Subagent** (reviewer, test-runner) | Tạo agent trong `.claude/agents/`, để nó review code Go/Java/SQL |
| Phase 4–5 | Slash command tự viết (`.claude/commands/`), **hooks** | Hook tự chạy `gofmt`/Spotless sau mỗi lần sửa file |
| Phase 6 | **MCP** (GitHub, có thể DB), chạy song song nhiều subagent | Agent phân tích kết quả benchmark |
| Phase 7–8 | Chế độ không giao diện (`claude -p`) trong CI, skills | Dùng Claude review PR trong GitHub Actions (tuỳ chọn) |

Thứ tự học đáng giá nhất: **CLAUDE.md → plan mode → /clear & /compact → subagent → hooks → MCP**. Làm tốt 4 thứ đầu là đã tiết kiệm token nhiều nhất.

## 2. Subagent là gì và dùng khi nào

Subagent là trợ lý chuyên biệt mà phiên chính giao việc cho. Mỗi subagent chạy trong **context riêng**, có system prompt riêng, giới hạn công cụ riêng và có thể dùng model khác. Hệ quả quan trọng: việc đọc nhiều file, chạy test dài, tìm kiếm... diễn ra ở đó và chỉ **kết quả tóm tắt** quay về phiên chính, nên phiên chính không bị phình context.

Lưu ý:
- Claude Code có sẵn subagent: **Explore** (tìm kiếm, đọc codebase), **Plan** (nghiên cứu khi ở plan mode), **general-purpose**.
- Subagent **không tạo được subagent khác**.
- Subagent được nạp khi **bắt đầu phiên**; tạo file mới xong phải mở lại phiên (hoặc dùng `/agents`).
- Tên agent trùng trong cùng phạm vi thì một cái bị bỏ qua, nên giữ tên duy nhất.
- Project agent nằm ở `.claude/agents/` (commit vào git), user agent ở `~/.claude/agents/`; project ưu tiên hơn user.

Khi nào nên dùng: review code, chạy và tóm tắt test, khám phá codebase, phân tích log/benchmark. Khi nào **không** cần: việc nhỏ vài dòng, hoặc việc cần trao đổi qua lại nhiều với bạn (subagent không hỏi lại tiện như phiên chính).

Mỗi subagent cũng tiêu token riêng, nên dùng cho việc **nặng đọc, nhẹ kết quả**; đừng tạo agent cho mọi thứ.

## 3. Bộ subagent cho project này

Tạo các file sau trong `.claude/agents/` (hoặc gõ `/agents` rồi chọn "Generate with Claude" để tự sinh rồi chỉnh).

**`.claude/agents/go-reviewer.md`**
```markdown
---
name: go-reviewer
description: Review code Go (concurrency, context, lỗi, goroutine leak). Dùng sau khi sửa code Go.
tools: Read, Grep, Glob, Bash
model: sonnet
---
Bạn là reviewer Go cẩn thận. Chỉ ĐỌC và chạy lệnh kiểm tra, không sửa file.
Kiểm tra: goroutine leak, thiếu context/timeout, race condition, đóng channel sai,
xử lý lỗi bị nuốt, bind variable SQL, graceful shutdown.
Chạy `go vet ./...` và `go test -race` ở package liên quan nếu cần.
Trả về: danh sách vấn đề theo mức độ (blocker/major/minor), file:dòng, gợi ý sửa. Ngắn gọn.
```

**`.claude/agents/java-reviewer.md`**
```markdown
---
name: java-reviewer
description: Review code Java/Spring (transaction, pool, virtual thread pinning, Oracle). Dùng sau khi sửa code Java.
tools: Read, Grep, Glob, Bash
model: sonnet
---
Bạn là reviewer Java (JDK 25) / Spring Boot. Chỉ ĐỌC, không sửa file.
Kiểm tra: ranh giới @Transactional, N+1, cấu hình HikariCP, synchronized trong virtual thread
(pinning), ThreadLocal, xử lý ORA-xxxx, idempotency, timeout/circuit breaker.
Trả về: vấn đề theo mức độ, file:dòng, gợi ý sửa. Ngắn gọn.
```

**`.claude/agents/sql-oracle-reviewer.md`**
```markdown
---
name: sql-oracle-reviewer
description: Review SQL/migration Oracle (lock, index, bind variable, isolation). Dùng khi đụng tới db/migrations hoặc truy vấn.
tools: Read, Grep, Glob
model: sonnet
---
Bạn là DBA Oracle. Chỉ đọc. Kiểm tra: bind variable, index thiếu, thứ tự lock gây deadlock,
SKIP LOCKED, chuỗi rỗng = NULL, READ COMMITTED gây lost update, constraint cho idempotency.
Trả về danh sách vấn đề và SQL đề xuất.
```

**`.claude/agents/test-runner.md`** (rẻ, chạy test và tóm tắt)
```markdown
---
name: test-runner
description: Chạy test/lint của service chỉ định và tóm tắt kết quả. Dùng khi cần chạy test dài.
tools: Bash, Read, Grep
model: haiku
---
Chạy đúng lệnh test được yêu cầu (vd `make test-inventory-go`). Không sửa code.
Trả về: pass/fail, danh sách test fail kèm đoạn lỗi quan trọng nhất (tối đa 20 dòng mỗi lỗi),
KHÔNG dán toàn bộ log.
```

**`.claude/agents/benchmark-analyst.md`**
```markdown
---
name: benchmark-analyst
description: Phân tích file kết quả k6/JMH/benchstat trong loadtest/results và labs, rút ra bảng so sánh Go vs Java.
tools: Read, Grep, Glob, Bash
model: sonnet
---
Đọc kết quả benchmark, kiểm tra điều kiện đo công bằng (cùng giới hạn CPU/RAM, warm-up, số lần lặp).
Xuất bảng markdown (throughput, p50/p95/p99, RAM, startup), nêu nghi ngờ về độ tin cậy, và
kết luận trung thực. Không làm đẹp số liệu.
```

Cách gọi: nói thẳng trong prompt, ví dụ `Dùng subagent go-reviewer review thư mục services/outbox-worker` hoặc để Claude tự giao dựa vào `description`. Muốn chạy song song: `Chạy song song go-reviewer và sql-oracle-reviewer cho Phase 4`.

Bài tập học: tạo một agent, cố tình cho nó quá nhiều quyền rồi thu hẹp `tools`; so sánh dung lượng context của phiên chính (`/context`) khi review trực tiếp và khi giao cho subagent.

## 4. Các tính năng đáng học khác

- **Plan mode**: Claude chỉ đọc và đề xuất kế hoạch, bạn duyệt rồi mới cho sửa file. Sửa kế hoạch gần như không tốn gì so với sửa code sai. Dùng cho mọi phase.
- **CLAUDE.md**: tự động nạp ở đầu mỗi phiên. Giữ **ngắn** (luật, lệnh build/test, quy ước), vì nó bị tính token ở mọi phiên. Chi tiết dài để trong `docs/` và chỉ trỏ tới khi cần.
- **Custom slash command** (`.claude/commands/*.md`): đóng gói prompt lặp lại, ví dụ `/phase-check` chạy checklist "Xong khi" của phase hiện tại.
- **Hooks**: chạy lệnh tự động ở các sự kiện (ví dụ sau khi sửa file thì format, trước khi chạy lệnh nguy hiểm thì chặn). Cấu hình trong settings của Claude Code; xem cú pháp hiện hành ở tài liệu hooks trước khi viết.
- **MCP**: nối công cụ ngoài (GitHub, DB, Grafana...). Mỗi MCP server thêm mô tả công cụ vào context, nên chỉ bật cái đang cần.
- **Chế độ không giao diện** `claude -p "..."`: dùng trong script/CI.
- **Skills**: gói hướng dẫn và script cho một loại việc, nạp khi cần. Tốt để tách quy trình dài ra khỏi CLAUDE.md.

## 5. Quản lý token: nguyên tắc và thói quen

Hiểu gốc rễ: mỗi lượt Claude Code gửi lại toàn bộ hội thoại hiện có, nên **context càng dài thì mỗi lượt càng đắt**, và phần lớn chi phí là token đầu vào chứ không phải code Claude viết ra.

Thói quen nên có:

1. **Một nhiệm vụ = một phiên.** Đổi việc thì `/clear`. Sau khoảng 2 lần sửa sai liên tiếp cũng `/clear` rồi viết lại prompt tốt hơn, đừng để context chứa cả đống thử nghiệm hỏng.
2. **`/compact` ở điểm ngắt hợp lý** (xong việc điều tra, trước khi bắt đầu tính năng mới). Có thể kèm hướng dẫn, ví dụ `/compact Giữ lại quyết định thiết kế và test đang fail, bỏ phần khám phá`. Không compact giữa lúc đang làm dở.
3. **Theo dõi**: `/cost` (API), `/usage` (xem phiên và, với gói trả phí, phân rã theo skill/subagent/MCP), `/context` (xem cái gì đang chiếm context).
4. **Prompt cụ thể**: chỉ rõ đường dẫn file, hàm, lệnh test. "Làm cho tốt hơn" khiến Claude tốn nhiều lượt đi khám phá.
5. **Plan mode trước, code sau.**
6. **Giữ CLAUDE.md nhỏ**, đặt thêm mục `# Compact instructions` nói rõ cần giữ gì khi nén.
7. **Chọn model theo việc**: việc nặng suy luận (thiết kế, debug khó) dùng model mạnh; việc cơ học (chạy test, format, đổi tên) dùng Sonnet/Haiku. Đặt `model: haiku` cho subagent đơn giản như trên.
8. **Giao việc nặng-đọc cho subagent** để log và file dài không nằm trong phiên chính.
9. **Chỉ bật MCP cần thiết**; nhiều MCP làm giảm đáng kể phần context còn dùng được.
10. **Ngắt lệnh sớm** (Esc) khi thấy Claude đi sai hướng, đừng để nó chạy hết rồi mới sửa.
11. **Chặn thư mục nặng** (`node_modules`, `target`, `bin`, kết quả benchmark lớn) khỏi tầm đọc bằng `.gitignore` và quy tắc trong CLAUDE.md ("không đọc `loadtest/results/raw/`").
12. **Tránh dán log dài**; chỉ dán đoạn lỗi liên quan hoặc bảo test-runner tóm tắt.
13. Nếu dùng gói Pro/Max thì bạn bị giới hạn theo hạn mức chứ không tính từng token; nếu dùng API thì đặt hạn mức chi tiêu cho workspace.

### Mẫu mục nên có trong CLAUDE.md
```markdown
# Compact instructions
Khi nén hội thoại, giữ: phase hiện tại, quyết định kiến trúc đã chốt, danh sách file đã sửa,
test đang fail và nguyên nhân. Bỏ: log dài, khám phá không dẫn tới đâu.

# Không đọc
loadtest/results/raw/, **/target/, **/node_modules/, *.log
```

## 6. Quy trình làm một phase (mẫu lặp lại)

1. `/clear`, mở plan mode, prompt: *"Làm Phase N theo docs/PROJECT_SPEC.md. Chỉ lập kế hoạch: file sẽ tạo/sửa, thứ tự, rủi ro."*
2. Đọc và sửa kế hoạch. Duyệt.
3. Cho code từng phần nhỏ (một service / một chiến lược mỗi lần), mỗi phần có test.
4. Giao `test-runner` chạy test; giao `go-reviewer`/`java-reviewer`/`sql-oracle-reviewer` review. Bạn tự đọc kết quả review và code quan trọng.
5. Sửa vấn đề, chạy lại test.
6. Cập nhật README/ADR. Commit.
7. `/usage` xem tốn bao nhiêu, ghi vào nhật ký học tập (xem mục 7). `/clear` trước phase kế.

## 7. Nhật ký học tập (làm project thành tài liệu phỏng vấn)

Tạo `docs/ai-workflow-log.md`, mỗi phase ghi 3–5 dòng: prompt hiệu quả nhất, chỗ Claude làm sai và bạn phát hiện thế nào, subagent nào hữu ích, lượng token/thời gian ước tính. Cuối project bạn có thể kể: *"Em dùng Claude Code với subagent reviewer và plan mode, và em kiểm soát chất lượng bằng test, contract test và reconcile sau load test"*. Phần này chứng minh bạn dùng AI có kiểm soát chứ không chỉ copy.

## 8. Việc cần tự làm, không giao hoàn toàn cho AI

- Hiểu và giải thích được từng pattern (outbox, saga, idempotency) bằng lời của mình.
- Tự đọc kết quả benchmark và lý giải vì sao Go/Java khác nhau.
- Tự viết ít nhất vài ADR và đoạn kết luận báo cáo.
- Tự debug ít nhất một lỗi concurrency thật bằng `pprof`/`jstack`/JFR.

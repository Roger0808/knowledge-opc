---
demandId: OPC-INTEGRATION-001
repositoryId: knowledge-opc
document: verification
status: draft
---

# knowledge-opc 验证记录

验证对象：TASK-K01~K05（EXT-001/002 限定范围）。环境：本机 macOS 27.0.1 arm64、go 1.26.5；测试数据库 sqlite（glebarez）+ 真实迁移 SQL；假 knowledge/假平台在 handler 级测试内经 httptest。

## 自动化验证（2026-10-20/21 执行）

| 命令 | 结果 |
| --- | --- |
| `go build ./...` | PASS（exit 0；既有 ld duplicate-libraries 警告） |
| `go test -count=1 ./internal/application/service/ -run SkillRelease` | PASS |
| `go test -count=1 ./internal/handler/ -run SkillRelease` | PASS |
| `go test -count=1 ./internal/application/service/ -run MCPInvocation` | PASS（26 用例） |
| `go test -count=1 ./internal/handler/ -run MCPInvocation` | PASS（12 用例） |
| `go test -count=1 -skip 'TestQueryTemplates\|TestUpdate\|TestDelete\|TestSkillPythonVerifier' ./internal/application/service/` | PASS（51.8s） |
| `go test -count=1 -skip 'TestPutTenantParserConfigAdminPreservesRedactedSecrets' ./internal/handler/` | PASS |
| `go test -count=1 ./internal/router/ ./internal/middleware/ ./internal/mcp/ ./internal/types/ ./internal/application/repository/ ./internal/config/` | PASS（全 6 包） |
| `gofmt -l internal/ cmd/`、`go vet ./internal/...` | PASS（子代理执行，主会话抽验） |

## 规格映射（关键条款 → 证据）

- INT-140/BND-141：发布幂等/版本冲突 409/下架 403/digest 校验 → service+handler 用例（K02）。
- BND-140 manifest：缺字段/非法枚举/digest 不符 422 不落库；zip 越界/symlink/超限拒绝 → K03 用例。
- INT-150 状态机：全路径含 unknown_outcome 三源不重发、cancel 两态、decision 单次绑定 → K05 用例（38 个）。
- BND-150/151：伪造/过期/跨主体 context 403、task actor 提交 decision 403、digest/policy 变更 409 → K05 用例。
- DATA-011 迁移：000129/000130/000131 的 up/down/约束探针由独立 reviewer 在本机一次性 PG16 实例实测通过；service 包测试用 GORM AutoMigrate（本行修正此前"测试内 sqlite 执行真实迁移 SQL"的措辞，审查指出后更正）。

## 差异与存量问题（诚实记录）

- `internal/application/service` 41 个存量失败、`internal/handler` 1 个存量失败：经基线 HEAD 对照 worktree 复跑结果一致（TestQueryTemplates*/TestUpdate*/TestDelete*/TestSkillPythonVerifier、TestPutTenantParserConfigAdminPreservesRedactedSecrets），与本需求无关。
- 前端 type-check（`vue-tsc --build`，NODE_OPTIONS=8GB 堆）：PASS（exit 0）。注：默认堆在本机 node 24 下原生崩溃（Abort trap 6），属内存限制环境问题；本需求未改前端代码。
- 前端测试（`tsx --test`）：**1 个失败，基线存量**——`src/components/GlobalCommandPalette/ResultItem.test.ts`（转义断言），本需求未改任何前端代码（`git status frontend/` 仅本次 pnpm 运行生成的未跟踪 lockfile，已清理），失败与 EXT-001/002 无关；其余全部通过。
- K06 契约失败阻断写入用例：随 X01 契约测试集在跨仓阶段统一执行。

## 工程扩展（Testing/PBT/Security/Resiliency）

- Testing：见上表，全部 PASS。
- PBT/Security/Resiliency：跨仓验证阶段（X01/X03）统一补齐并回填（含 property/case 数/seed 与故障注入证据）。

---
project: work-opc
document: verification
demandId: WORK-RESOURCES-001
repositoryId: knowledge-opc
loop: 1
version: "1.0"
status: draft
updatedAt: 2026-10-22
---

# knowledge-opc 本仓验证证据（TASK-008）

实施分支 `feature/261022_WORK-RESOURCES-001_resource-entry`（worktree，未提交）。本文件只记录本仓命令级证据；跨仓联调（TASK-025/027）不在此重复。

## 环境

- macOS arm64；go1.26.5 darwin/arm64；node v24.19.0。
- 本机 PostgreSQL（homebrew，localhost:5432）用于 000133 SQL 与备份/恢复工具的真实引擎验证，验证后已删除临时库（wr001_pg133 / wr001_migtest）。
- 无 Docker：无法运行 paradedb 镜像，全量 versioned 迁移链（000002 需要 pg_search 扩展）在普通 PG 上基线即失败，与本迁移无关（已在基线 commit 071fd96c 复核同一行为）。

## Go 定向测试（TASK-008 指定四包）

命令：`go test ./internal/handler ./internal/router ./internal/application/service ./internal/application/repository -count=1`

- internal/handler：ok（3.843s）
- internal/router：ok（2.756s）
- internal/application/repository：ok（2.701s）
- internal/application/service：**FAIL（退出码 1），唯一失败 `TestSkillPythonVerifier`**（`a requirement the venv does not carry`、`a pyproject.toml dependency the venv does not carry`）。已在未改动的基线 071fd96c（临时 worktree 验证后已删除）复现同一失败 → 环境性既有失败（本机 python 校验器环境），与本轮回改动无关。同包排除该用例后：`go test ./internal/application/service -skip 'TestSkillPythonVerifier' -count=1` → **ok（42.169s，退出码 0）**。
- 迁移工具与迁移：`go test ./internal/database/ -count=1` → ok（含 SQLite 000047 up/down、备份/恢复往返、MCP 表保留断言）。
- 全仓编译：`go build ./...` → 退出码 0；`gofmt -l internal/ migrations/` 无输出。

## 迁移验证（VAL-217 / BND-011）

SQLite（真实文件库，经 golang-migrate）：

- 空库迁移到 47：skill_release 不存在，mcp_invocations / mcp_invocation_decisions 保留（`TestSkillReleaseBackupEmptyDatabase` PASS）。
- 迁移到 46 → 插入 2 行代表性存量 → 备份 → 迁移 47（表与 4 个专属索引消失，MCP 表与行保留）→ 从备份恢复 2 行 → 恢复后行 checksum 与备份一致、archive_ref 清单一致（`TestSkillReleaseBackupRestoreRoundTrip` PASS）。
- 结构 down 只重建空表（`TestSkillReleaseDownRecreatesEmptyTableOnly` PASS）；恢复中主键冲突整体回滚（`TestSkillReleaseRestoreFailureRollsBack` PASS）；备份篡改被校验拒绝（`TestSkillReleaseBackupTamperEvident` PASS）。
- `TestSQLiteMigrationsCreateVersionedSchema` / `TestSQLiteMigrationsUpgradeV4PreservesData` / `TestSQLiteMigrationsUpgradeV16AddsSessionForkColumns` 全部 PASS（expectedSQLiteMigrationVersion 已更新为 47，skill_release 已从 versionedSQLiteTables 移除）。

PostgreSQL（真实引擎，psql + 门控测试）：

- 000133 up：手工构造含 1 行存量 + 4 索引的 skill_release + 3 张共享表 → `psql -f 000133_drop_skill_release.up.sql`（ON_ERROR_STOP=1）→ skill_release 表/索引均为 0，共享表 3 张保留。退出码 0。
- 000133 down：同库执行 down → 表重建（16 列）、索引 5 个（4 专属 + 主键）、行数 0、共享表保留。退出码 0。
- 备份/恢复工具在真实 PG 上：`WEKNORA_MIGRATION_TEST_POSTGRES_DSN=postgres://…/wr001_pg133 go test ./internal/database/ -run TestSkillRelease` → ok（备份 2 行 → drop → 恢复 → 行 checksum 一致）。临时库已删除。
- 未执行项：golang-migrate 全量 versioned 链在真实 PG 上的端到端 up（000002 依赖 pg_search，本机无 Docker/paradedb，基线即失败）；SQLite combined 000046 的 MCP 表前后不变已由迁移到 47 后的 MCP 表存在性与行数断言覆盖，未单独整体 down 000046（规格明确禁止）。

## Frontend（TASK-005）

- `npm run type-check`（vue-tsc --build）→ 退出码 0。
- `npm run build`（vite build）→ 退出码 0（默认堆内存 OOM 一次，NODE_OPTIONS=--max-old-space-size=8192 后通过；chunk 大小警告为既有）。
- `npm test`（tsx --test 全量）→ pass 1886 / fail 0 / EXIT=0（含本次新增 resourceDeepLink 6 例、loginRedirect 3 例；修复了两个因新增 import/结构变化需要更新的源码契约测试 fixture）。
- `npm run check-i18n`（locale key audit）→ fail 0（6 个语种均补齐 deepLinkNotFound / deepLinkTenantForbidden）。

## PBT（VAL-102 P4，复用规格定义）

- `internal/handler/skill_catalog_archive_pbt_test.go::TestPBT_P4_CatalogArchiveInputsNeverEscape`：**seed=20261025，cases=600**。属性：被接受的 catalog id 永不包含分隔符/dot-dot/换行（对照独立参考实现）；被接受的 expected_sha256 规范化后恒为 64 位小写 hex。PASS。
- `internal/application/service/tenant_skill_catalog_archive_pbt_test.go::TestPBT_P4_DownloadNameNeverEscapes`：**seed=20261025，cases=600**。属性：下载文件名（来自工作区可控的技能名）对照独立参考净化器逐例一致，永不含 `/ \` CR LF、永不含 `..`、长度 ≤64、首尾无 `-/.`。PASS。过程中属性测试抓到并固化了两个真实缺陷修复（截断后重新引入尾部 `-`、连续点号产生 `..`），实现已修复并通过全部 600 例。

## VAL 映射（本仓可证部分）

| 规格 | 证据 |
|---|---|
| VAL-202 未登录/无权 | service 跨空间同名与删除后访问 = 共享 not-found（404 RESOURCE_NOT_ACCESSIBLE 映射测试）；router 测试断言归档路由对 API key 默认拒绝 |
| VAL-204 跨空间同名不按同名替代 | `TestOpenCatalogArchiveSeparatesSameNameCatalogsAcrossTenants`、`TestGetCatalogArchiveNotFoundIsResourceNotAccessible`；frontend resourceDeepLink 恶意 return URL 拒绝 |
| VAL-208 无 release 写入的下载 | `TestOpenCatalogArchiveReturnsOriginalBytes`（标准 SKILL.md、无 opc-client.json、字节逐位一致）；删除目录后旧安装行不复活（`TestOpenCatalogArchiveDoesNotResurrectDeletedCatalogs`） |
| VAL-210 版本竞争 | `TestOpenCatalogArchiveRefusesStaleExpectedSHA`（409 SKILL_VERSION_CHANGED）、`TestOpenCatalogArchiveRacingReRegisterIsAVersionChange`（行移动竞争） |
| VAL-211 坏包/边界 | `TestOpenCatalogArchiveDetectsReplacedBytes`（422 SKILL_HASH_MISMATCH）、存储缺失 409 ARCHIVE_NOT_READY、服务错误不包装成 ZIP（`TestGetCatalogArchiveServiceFailuresAreNotZips`）、P4 属性 |
| VAL-216 旧链路 404 | `TestSkillReleaseRoutesAreGone`（publish/releases 家族 5 路径逐个 404 且无 API key 声明）；`go build ./...` 无 release 残留引用 |
| VAL-217 迁移与恢复 | 上文「迁移验证」SQLite + PG 双引擎；行数/hash/引用清单校验一致 |
| INT-103/105 错误语义 | handler 测试断言各错误码与 Content-Type/X-SHA256/Content-Length |
| BND-003/009 网页定位 | type-check/build/前端测试；未登录→登录返回同 origin 目标（router/Login 契约测试）；VAL-204 恶意 URL 拒绝 |

## 本仓指纹

- 基线：release @ 071fd96c4295d0925244615f7d8a73e9544f26d0（TASK-001 固定，未变）。
- 工作树 HEAD：071fd96c（分支 feature/261022_WORK-RESOURCES-001_resource-entry，全部改动未提交；release 链文件删除已 git rm 暂存，无 commit）。
- 未执行/环境缺失：真实浏览器手测与跨仓联调归 TASK-025/027；TestSkillPythonVerifier 为基线既有环境失败；PG 全量迁移链需 paradedb（无 Docker）。

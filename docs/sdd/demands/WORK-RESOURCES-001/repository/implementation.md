---
project: work-opc
document: implementation
demandId: WORK-RESOURCES-001
repositoryId: knowledge-opc
loop: 1
status: draft
updatedAt: 2026-10-22
---

# knowledge-opc 实施记录

## 基线与分支（TASK-001）

- remote：`https://github.com/Roger0808/OPC-2.0.git`
- 固定基线：release @ `071fd96c4295d0925244615f7d8a73e9544f26d0`（固定 SHA 经 git rev-parse 核验一致；交接时工作树干净）
- 标准分支：`feature/261022_WORK-RESOURCES-001_resource-entry`，从固定基线新建（disposition=created）
- 隔离方式：专用 worktree，从固定 commit 检出
- 任务范围：TASK-003～008；当前迁移最大序号 versioned=000132、SQLite=000046，新清理迁移计划 000133/000047（实施前核对空闲号；SQLite 000046 含 MCP，不能整体 down）

## 实施过程

实施于 2026-10-22 由 Kimi（实施工程师）在隔离 worktree 完成；改动未提交。迁移号实施前核对：versioned 000133、SQLite 000047 均为空闲号（目录最大 000132/000046），按计划采用，无需递增。验证证据见同目录 verification.md。

- **TASK-002（本仓夹具）**：`internal/application/service/catalog_archive_fixture_test.go` — 可区分样例（A/B/A2 式跨空间同名 C1/C2、标准 SKILL.md 真实 zip、h1/h2 由真实字节计算）；可注入：身份切换（tenant id）、目录移动（re-register）、归档替换（replaceObject）、存储缺失（getErr）、超时（blockFor + context deadline）。
- **TASK-003**：新增 `internal/application/service/tenant_skill_catalog_archive.go` — `OpenCatalogArchive` 严格按 tenant+catalog_id 查 `GetCatalog`（绝不走 resolveCatalog 投影/旧安装行/release 回退），核验 expected/current/actual 三 SHA（竞争时重读行区分版本变更与哈希不匹配），返回原始字节；错误哨兵 ErrCatalogArchiveNotReady/VersionChanged/HashMismatch；下载名净化器（PBT 抓出截断尾部 `-`、连续 `..` 两缺陷并修复）。
- **TASK-004**：新增 `internal/handler/skill_catalog_archive.go`（Viewer+ GET `/api/v1/skills/catalog/:id/archive`，expected_sha256 必填并规范化，INT-105 错误信封，Content-Type/X-SHA256/Content-Length，服务错误不包装 ZIP）；`skill_handler.go` 接口加 `OpenCatalogArchive`；`routes_agent.go` 注册（普通 JWT 组，API key 默认拒绝）。Admin 文件预览/上传/云沙箱安装路由未动。
- **TASK-006**：删除 release 全链路 — handler/service/repository 三个 skill_release.go 及专属测试、types 的 SkillRelease* 类型组、container DI（4 处 Provide/参数）、router 注册、`DeleteCatalog` 的 release 账本检查（目录删除不再检查 release）；旧路由 404 测试 `internal/router/skill_release_removal_test.go`。catalog 上传/云沙箱安装/MCP/RBAC 测试全部保留并通过。
- **TASK-007**：新迁移 `migrations/versioned/000133_drop_skill_release.{up,down}.sql`、`migrations/sqlite/000047_drop_skill_release.{up,down}.sql`（只 drop skill_release 及 4 个专属索引，down 重建空表结构；不改任何历史迁移；不动 catalog/tenant_skills/MCP/身份表）。配套工具 `internal/database/skill_release_cleanup.go`（Backup/Write/Read/Verify/RestoreSkillReleaseTable，checksum 清单 + 事务恢复），测试覆盖空库/代表性旧行库/回滚/篡改检测，SQLite 与真实 PG 双引擎验证。
- **TASK-005**：新增 `frontend/src/utils/resourceDeepLink.ts`（catalog_id/tenant_id 校验、safeLoginRedirect 只认可同 origin `/platform` 相对路径）；router 未登录跳 /login 携带 redirect；Login.vue 登录成功回跳（hasValidTenant 前提）；SkillSettings 依 catalog_id 精确定位并滚动、未找到明确报错不按同名替代、tenant_id 经成员校验后切空间；Toolbox 深链优先 skills 页签；6 语种 i18n 补齐。
- **TASK-008**：verification.md 记录四包 go test、frontend type-check/build/全量 test、PBT P4（seed=20261025，两处各 600 case）、VAL 映射与未执行项。

## Change surface

新增：`internal/application/service/tenant_skill_catalog_archive.go`、`tenant_skill_catalog_archive{,_pbt}_test.go`、`catalog_archive_fixture_test.go`、`internal/handler/skill_catalog_archive.go`、`skill_catalog_archive{,_pbt}_test.go`、`internal/router/skill_catalog_archive_route_test.go`、`skill_release_removal_test.go`、`internal/database/skill_release_cleanup{,_test}.go`、`migrations/versioned/000133_drop_skill_release.{up,down}.sql`、`migrations/sqlite/000047_drop_skill_release.{up,down}.sql`、`frontend/src/utils/resourceDeepLink.ts`、`frontend/src/utils/resourceDeepLink.test.mjs`、`frontend/src/router/loginRedirect.test.mjs`。

删除：`internal/handler/skill_release.go`、`internal/handler/skill_release_test.go`、`internal/application/service/skill_release.go`、`internal/application/service/skill_release_test.go`、`internal/application/repository/skill_release.go`。

修改：`internal/handler/skill_handler.go`、`internal/handler/skill_catalog_test.go`、`internal/application/service/tenant_skill_catalog.go`（DeleteCatalog 去 release 检查）、`tenant_skill_service.go`（去 skillReleases）、`tenant_skill_catalog_test.go`（删 release 守卫用例）、`internal/types/tenant_skill.go`（删 SkillRelease 类型组）、`internal/container/container.go`、`internal/router/router.go`、`routes_agent.go`、`operations_test.go`、`router_api_key_capabilities_test.go`、`internal/database/migration_sqlite_versioned_schema_test.go`（版本 47、移除 skill_release 断言）；frontend：`router/index.ts`、`views/auth/Login.vue`、`views/settings/SkillSettings.vue`、`views/toolbox/Toolbox.vue`、`config/toolbox.test.ts`、`views/settings/SkillSettings.filters.test.ts`（新增 import 的 mock）、6 个 locale 文件。

数据/接口/部署：PG/SQLite 新前向迁移（drop skill_release）；新 Viewer+ 接口 GET `/api/v1/skills/catalog/:id/archive`；移除 `/api/v1/skills/catalog/:id/publish` 与 `/api/v1/skills/releases*` 家族。无对象存储清理动作（本轮只盘点/备份工具，不删对象）。

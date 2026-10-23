# WORK-RESOURCES-001 对外契约摘要（knowledge-opc 只读投影）

> 本文件为只读追溯包的一部分，不得在参与仓独立修改；权威版本见主仓。

## 对外接口

- `GET /api/v1/skills/catalog/:id/archive?expected_sha256=...`（INT-103/105）
  - 权限：Viewer+，普通 JWT 组；API key 默认拒绝。
  - 行为：严格按 tenant+catalog_id 查真实未删除目录（不走旧安装投影/同名包/release 回退），核验 expected/current/actual 三 SHA（竞争时重读行区分版本变更与哈希不匹配），返回原始 zip 字节；不重新打包，内部 BundleRef 不外泄；无专用 manifest 的标准包可下载。
  - 响应头：`Content-Type`、`X-SHA256`（实际字节哈希）、`Content-Length`；服务错误不包装 ZIP。
  - `expected_sha256` 必填并规范化。

## 错误信封（INT-105）

- 统一 `{error:{code,message,request_id}}`；message 不含上游 Token、内部 URL、文件路径或正文。
- 归档相关：404 `RESOURCE_NOT_ACCESSIBLE`（目录缺失/不可见）、409 `SKILL_VERSION_CHANGED`（竞争更新）、422 `SKILL_HASH_MISMATCH`（摘要不符）、归档未就绪 `ARCHIVE_NOT_READY`（not_ready 语义）、403 `SPACE_FORBIDDEN`。
- 衔接既有平台码：400 `INVALID_REQUEST`、401 `SESSION_REVOKED`、409 `CONTEXT_CHANGED`、403 `SCOPE_MISMATCH`。

## 旧 release 路由

- 旧技能 release 全链路已删除（handler/service/repository/types/DI/router），旧 release 路由全部 404（VAL-216）。
- 迁移 `migrations/versioned/000133*`、`migrations/sqlite/000047*` 只 drop skill_release 及 4 个专属索引，down 重建空表结构；历史迁移不改写，catalog/tenant_skills/MCP/身份表不动。

## 消费方

- platform-opc 携个人桥 token 调用本路由（platform → knowledge，INT-103 链路）。

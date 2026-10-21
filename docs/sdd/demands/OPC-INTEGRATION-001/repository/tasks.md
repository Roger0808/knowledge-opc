---
demandId: OPC-INTEGRATION-001
repositoryId: knowledge-opc
document: tasks
---

# knowledge-opc 任务（总体 tasks.md 的本仓视图）

基线：release / `628d974dd68736c39e09a2cc8a077047f5d486f4`；分支 `feature/261020_OPC-INTEGRATION-001_opc-integration`。验证：相关 Go 包测试 + frontend type-check/test。

- TASK-K01 技能发布数据与迁移（skill_release）。
- TASK-K02 发布/查询/下载/下架接口 INT-140。
- TASK-K03 manifest 解析与安全校验（opc-client.json）。
- TASK-K04 精确调用数据与迁移（mcp_invocations/mcp_invocation_decisions）。
- TASK-K05 精确调用适配与审批绑定 INT-150。
- TASK-K06 回归与契约。

约束：EXT-001/002 限定范围（ADR-008）；不修改检索/Agent 引擎、不解除 stdio 限制；迁移遵守 golang-migrate 约定。

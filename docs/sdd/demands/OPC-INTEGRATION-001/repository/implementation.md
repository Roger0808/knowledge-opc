---
demandId: OPC-INTEGRATION-001
repositoryId: knowledge-opc
document: implementation
---

# knowledge-opc 实施记录

- 基线：release / `628d974dd68736c39e09a2cc8a077047f5d486f4`（远程固定）。
- 当前分支：`feature/261020_OPC-INTEGRATION-001_opc-integration`（标准分支）。
- 任务范围：TASK-K01~K06。
- 验证命令：相关 Go 包测试、frontend type-check/test。

## 变更日志

- 2026-10-20 TASK-K01~K03 技能发布链（主会话复验 `go build ./...` exit 0、SkillRelease 新测试全过）：
  - K01：`migrations/versioned/000129_skill_release.*`（唯一约束 (catalog_id,version) 与 (bundle_sha256)、idempotency_key 唯一、target_tenants/target_roles/manifest jsonb）。
  - K02/K03：`internal/types/tenant_skill.go`（SkillReleaseEntity + manifest 类型）、`internal/application/repository/skill_release.go`（独立聚合，不动既有 TenantSkillRepository）、`internal/application/service/skill_release.go`（Publish/List/Get/OpenArchive/Delist + manifest 解析校验）、`internal/handler/skill_release.go`、`internal/router/routes_agent.go`（RegisterSkillReleaseRoutes + catalog publish）、`internal/router/router.go`、`internal/container/container.go` 接线。新路由：POST /skills/catalog/:id/publish（Admin+）、GET /skills/releases（Viewer+ 租户命中）、GET /skills/releases/:id（Viewer+ 目标成员，不可见 404 防探测）、GET /skills/releases/:id/archive（Viewer+ + 角色命中，X-SHA256 头）、POST /skills/releases/:id/delist（Admin+ 源租户）。错误码 SKILL_VERSION_CONFLICT 409 / SKILL_DELISTED 403 / SKILL_HASH_MISMATCH 422 / SKILL_MANIFEST_INVALID 422。service 18 用例 + handler 12 用例全过。
  - **实施期语义决定（待对账阶段回写规格措辞）**：manifest.sha256 字面要求"与整包 zip 哈希一致"在数学上不成立（manifest 在 zip 内，自引用无不动点）。实施采用：manifest.sha256 = 包内除 opc-client.json 外全部文件字节的确定性 digest（打包方可预计算），bundle_sha256 列仍存整包实际字节哈希并在下载时重算校验。安全语义不弱化；规格 BND-140 措辞留待 reconciliation 阶段澄清（stage_revalidated）。
  - 存量测试失败：internal/handler 1 个 + internal/application/service 41 个在基线 HEAD 对照 worktree 复跑结果一致，属存量问题，与本改动无关；记入验证阶段差异。
  - 未 commit/push。遗留：List 过滤待下推 JSONB 查询；SQLite 迁移链未加该表；swag docs 未重新生成。
- 2026-10-20 TASK-K04~K05 精确调用 EXT-002（主会话复验 `go build ./...` exit 0、MCPInvocation 新测试全过）：迁移 000130 `mcp_invocations`（idempotency_key 与 context_jti 部分唯一索引、args_digest、args_encrypted、7 天过期）+ 000131 `mcp_invocation_decisions`（单次有效）；`internal/types/mcp_invocation.go`（10 状态 + 终态原因 + canonical JSON digest）、repository（CAS 状态迁移）、service（全状态机：created→policy_checked→waiting_approval/waiting_oauth/executing→succeeded/failed/unknown_outcome/cancelled；惰性过期；panic/写超时/无回执三源 unknown_outcome 且不重发）、context 校验（platform JWT 签名/aud/exp≤120s/jti 双防重放/主体一致/task 绑 task+device）、handler 五路由、`internal/router/routes_integration.go`、config `IntegrationConfig`（env 注入无硬编码密钥）；执行经 `mcp.MCPManager.GetOrCreateClient`（不裸调 CallTool、stdio 限制保持）；策略复用 approval.ToolRisk + policy.Decide（Ask 绝不静默跳过；task actor 写类 Deny、目录只列 read/probe）。新增 38 用例全过；存量失败集与基线一致。未 commit/push。遗留：ArgsKey 未配置时参数明文存储（digest 绑定完整性，spec 列可空；如需强制加密可改无密钥拒 Ask）；决策后执行同步（崩溃语义可测，纯异步队列留后续）。
- 2026-10-21 审查修复（F1~F10，子代理被 429 中断后主会话补齐测试适配、代理恢复后完成；主会话复验 service 62 + handler 26 用例全过 exit 0）：F1 结果分类取反（确定未发送→failed、歧义→unknown_outcome）；F2 发布字节物化到 tenant-skills/releases/<release_id>.zip + DeleteCatalog 引用保护 409；F3 Decide 无条件惰性过期；F4 无 ArgsKey 只存 digest 不落明文；F5 调用上下文必选 403；F6 上下文 args_digest 绑定校验；F7 aud 缺失 fail-closed；F8 幂等重放租户/主体校验；F9 发布方租户解析存储；F10 探测 oracle 顺序统一/状态迁移错误上抛/崩溃惰性清扫 unknown_outcome/信封补 request_id/SQLite 迁移链 000046 三表等价物 + service 夹具改真实迁移 SQL。遗留：无 ArgsKey 时审批后跨进程续跑无正文可恢复（运维以 WEKNORA_INTEGRATION_ARGS_KEY 覆盖）；release 键审计期后清理未实现。未 commit/push。
- 2026-10-21 第二轮审查发现修复（R1/R2，主会话执行并复验）：R1 无 ArgsKey 时进入 waiting_approval/waiting_oauth 前失败关闭（新错误 ErrMCPInvocationArgsKeyRequired → 503 INTEGRATION_ARGS_KEY_REQUIRED；行落 failed），回归测试 TestMCPInvocationApprovalFailsClosedWithoutArgsKey 断言不入等待态且执行计数 0；R2 崩溃清扫按风险分窗：mcp_invocations 加 risk 列（迁移 000132 + sqlite 000046 同列；MarkPolicyChecked 落 risk；legacy 空 risk 走写窗口），读类 30min 读窗口不写窗口误伤，Finish moved=false 记日志并回读最新行不回写；回归 TestMCPInvocationCrashSweepReadWindow（31s 不结算/31min 结算）。复验：service/handler/router/middleware/database/repository/config 全绿（存量失败集不变），MCPInvocation race 抽查绿，gofmt 干净。未 commit/push。

---
demandId: OPC-INTEGRATION-001
repositoryId: knowledge-opc
document: review
status: draft
---

# knowledge-opc 独立审查记录

reviewer 路径：当前 Agent 的隔离只读 subagent（用户 2026-10-21 选择）。

## 第一轮（2026-10-21，指纹：HEAD=628d974d…、变更清单哈希 8de6c04f…；reviewer 复跑 build/相关包测试全过，迁移在一次性 PG16 实例 up/down/约束探针实测通过）

结论：**CHANGES_REQUIRED**。

阻断：
- F1：写调用"结果未知"分类反向——ErrToolCallOutcomeUnknown（已发送未回执）落 failed、ErrNotConnected（确定未发送）落 unknown_outcome，BND-152 安全属性反向。
- F2：发布字节未物化——release 记录 catalog 可变引用（tenant-skills/catalog/<id>.zip 同键覆盖/删除即失效），BND-141 不可变承诺不成立；应复制到 release 自有不可变键并让 DeleteCatalog/重注册感知引用。
- F3：Decide 对 waiting_approval 不做惰性过期，过期审批仍可执行。
规格语义：F4 无 ArgsKey 时参数明文落库（改只存 digest）；F5 平台调用上下文可选（改必选，BND-150 字面条款）；F6 上下文未绑目标操作/参数摘要（校验 context.args_digest==请求 digest）。
低：F7 aud 未配置时 fail-open 改 fail-closed；F8 幂等重放补租户/主体校验；F9 跨租户下载解析发布方存储；F10（探测 oracle 顺序、状态迁移错误忽略、executing 崩溃无清扫、信封缺 request_id、SQLite 迁移链/测试措辞）。
另修正：verification.md 中"测试内 sqlite 执行真实 SQL"措辞与事实不符（service 测试用 AutoMigrate；真实 SQL 证据由 reviewer 在 PG16 补做）——由主会话改正。

处置：修复切片处理 F1~F10；完成后重新固定指纹并重审。

## 修复回写（2026-10-21）

F1~F10 逐条修复完成（明细见 implementation.md 当日条目）；主会话复验 service 62 + handler 26 用例全过；存量失败集经 pristine 对照证实与本改动无关（环境性：本机代理 DNS 触发 SSRF 防护等）。待第二轮差异重审。

## 第二轮（2026-10-21，差异重审，新指纹 d8e4f34b…）

结论：**CHANGES_REQUIRED**。F1~F10 全部修复到位且有针对性回归；但修复引入 **R1（高）**：无 ArgsKey 时审批/OAuth 恢复路径以 nil 参数执行已批准的调用（批准绑定形同虚设）——必修；**R2（中）**：崩溃清扫对慢读调用按 WriteTimeout 过早结算且 Finish 忽略 moved=false 吞回执——建议同轮修；R3 观察项（legacy delisted 行保护口径、防重 500 形态、R1 修复对 OAuth 等待的影响）记注。处置：主会话修 R1/R2 后重审。

## 第三轮修复回写（2026-10-21）

R1/R2 已修（主会话）：审批/OAuth 等待态在无参数密钥时失败关闭（503 INTEGRATION_ARGS_KEY_REQUIRED）；崩溃清扫按风险分窗（risk 列 000132）+ Finish 回执不再静默丢弃。回归测试两条。待第三轮差异重审。

## 第三轮（2026-10-21，差异重审 R1/R2，指纹 7758f64a…）

结论：**READY_FOR_HUMAN_REVIEW**。R1/R2 修复正确且有回归测试；迁移 000132 PG/sqlite 双链实测；OAuth 等待无密钥拒绝的连带语义经核对为唯一自洽选择。观察项 4 条（低危，不阻断）：失败原因列语义粗糙、OAuth 路径无专项测试、日志标签读旧值、risk 列需在 Architecture Reconciliation 记录（主会话处理）。

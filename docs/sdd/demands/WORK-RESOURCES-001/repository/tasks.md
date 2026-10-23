---
project: work-opc
document: repository-tasks
demandId: WORK-RESOURCES-001
repositoryId: knowledge-opc
version: "1.0"
status: confirmed
updatedAt: 2026-10-22
---

# knowledge-opc 本仓任务投影

本文件保存在主仓，只投影本仓 Tasks；唯一总体任务为 [Tasks v1.0](../../loop-1/tasks.md)。生成来源已确认 Tasks SHA-256：`71edce650f713d8898c6a065026e51bfaadb13f17ffa54d48e4ed42e7676519f`。修改总体任务后重新生成投影，不在参与仓独立编辑需求或改变 activeLoop。

范围：目录归档、RBAC、网页定位、旧 release 清理和 knowledge 迁移恢复。基线、规范 remote、标准分支以 [repositories.json](../../repositories.json) 为准。以下任务均未实施或验证通过。

共享前置：TASK-001 隔离、TASK-002 fixture。共享交付责任：TASK-025 联调本仓响应；TASK-026/027 本仓发布、备份与回退；TASK-028/029 本仓 verification/review/receipt 和只读导出；TASK-030 提供本仓固定证据。

规格依据：[Specification v1.0](../../loop-1/specification.md)；当前阶段只维护任务，不预建 implementation/verification/review 或就绪 receipt。

### TASK-003 实现严格目录原始归档读取

- 依赖：TASK-002；引用：REQ-005/006、ADR-005、规格 BND-006、INT-103/105。
- 位置：internal/application/service/tenant_skill_catalog.go、对应接口与归档测试。
- 完成条件：按 tenant+catalog_id 查真实未删除目录，读取其权威 BundleRef，核验 expected/current/actual SHA，返回原始字节。禁止旧安装投影、同名包、沙箱运行文件或 release 回退；没有专用 manifest 的标准包可下载。缺失/坏包/竞争更新返回规格错误，内部 BundleRef 不外泄。
- 验证方法：VAL-208/210/211；真实 catalog 与仅存在旧安装投影分别测试，删除目录后缓存/旧安装均不能复活下载，包字节不被重新打包。

### TASK-004 暴露目录归档路由并保持权限边界

- 依赖：TASK-003；引用：REQ-008、ADR-007、规格 INT-103/105、DATA-105。
- 位置：internal/handler/skill_catalog.go、skill_handler.go、internal/router/routes_agent.go、router/handler 测试。
- 完成条件：新增 Viewer+ 目录归档 GET，校验当前空间成员及资源归属；无权/不存在一致处理，正确 Content-Type/X-SHA256/大小限制。Admin 文件预览、上传和云沙箱安装权限保持，API key 路由能力声明与实际权限一致。
- 验证方法：VAL-202/209/211；Viewer 下载可见目录成功，跨空间、撤权、伪造参数、普通用户访问管理文件接口均拒绝；服务错误不被包装成 ZIP 成功。

### TASK-005 网页详情定位与登录返回

- 依赖：TASK-002；引用：REQ-002/004、ADR-001/007、规格 BND-003/009。
- 位置：frontend/src/router/index.ts、views/toolbox/Toolbox.vue、views/settings/SkillSettings.vue、KB 页与 auth/空间状态相关模块。
- 完成条件：技能 catalog_id 精确打开目标详情，tenant_id 只用于通过成员校验后的空间选择；KB/引用路由同样保留资源目标。未登录后登录可返回同 origin 目标；无权/删除明确失败，不按同名替代。不为上传首页新造上传深链。
- 验证方法：VAL-203/204；网页路由/状态测试及后续实际浏览器手测，包含不同网页账号、跨空间同名资源、恶意 return URL。frontend `npm run type-check`、`npm run build`；定向 `npm test -- <实际测试文件>`。

### TASK-006 删除 knowledge 技能 release 代码链路

- 依赖：TASK-004；引用：REQ-009、ADR-008、规格 BND-010/011。
- 位置：internal/handler/skill_release.go、application/service/skill_release.go、repository/skill_release.go、types 的 release 类型、container/container.go、router 注册及 catalog 删除判断；实际引用清单以实施定位为准。
- 完成条件：删除发布、release 列表/详情/archive/delist、DI 与 release 专属测试；目录删除不再检查 release 账本。保留 catalog 上传/云沙箱安装、通用归档工具、MCP 和原有共享权限测试；旧路由确实 404。历史迁移与旧审计不改写。
- 验证方法：VAL-216；对旧路径逐个断言 404，Go 编译及 catalog/MCP/RBAC 定向回归；源码/DI 搜索没有可执行 release 发布依赖，历史 SQL/归档里的术语不要求删除。

### TASK-007 knowledge 清理迁移与恢复工具

- 依赖：TASK-006；引用：ADR-008/DATA-001、规格 BND-011、VAL-217。
- 位置：migrations/versioned、migrations/sqlite、迁移测试与定向备份/恢复脚本。
- 完成条件：新前向迁移只 drop skill_release 及其专属索引，配套结构 down；不触动 catalog/tenant_skills/MCP/身份表。备份 schema/行/archive_ref 清单、校验恢复；release 专用对象清理有白名单与共享引用保护，默认不在迁移中递归删对象。
- 验证方法：临时 PostgreSQL/SQLite 空库和代表性旧行库分别 up/down/恢复，验证行数/hash/引用、事务失败恢复、共享归档保留；SQLite combined 000046 的 MCP 表前后不变。没有恢复证据不能运行实际目标库清理。

### TASK-008 knowledge 构建与本仓验证证据

- 依赖：TASK-003～007；引用：ACC-001/002/004/005、规格 VAL-202/204/208/210/211/216/217、VAL-102。
- 位置：本仓 Go/前端测试及主仓 repository unit verification.md。
- 完成条件：归档大小/路径边界、权限、目录更新/删除竞争、网页定位、旧链路删除和迁移恢复有逐项证据；测试实际跑过，skip/缺环境单列。包解析属性输入复用规格 P4，记录 seed/case。
- 验证方法：`go test ./internal/handler ./internal/router ./internal/application/service ./internal/application/repository` 按实际依赖运行，必要时先定向 `-run` 定位后完整受影响包；frontend type-check/build/定向 test。记录命令、环境、退出码、未覆盖项及本仓指纹。

## 投影确认

Roger0808 / yj198788@hotmail.com 于 2026-10-22 以“确认，后续交接给 kimi 去开发完成”批准主仓 Tasks v1.0，本仓投影随同确认；任务定义未改，不表示任务已执行。

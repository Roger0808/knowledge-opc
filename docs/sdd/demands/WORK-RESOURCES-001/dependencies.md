# WORK-RESOURCES-001 依赖与发布顺序（knowledge-opc 只读投影）

> 本文件为只读追溯包的一部分，不得在参与仓独立修改；权威版本见主仓。

## 发布顺序

knowledge → platform → work（主仓 release-checklist.md）。knowledge-opc 第 1 步发布。

## 本仓依赖

- platform-opc：消费本仓归档契约 INT-103/105（platform → knowledge `GET /api/v1/skills/catalog/:id/archive`，个人桥 token）；platform 在本仓之后发布。
- 迁移：本仓迁移 000133（versioned）/000047（sqlite）只清理 skill_release，不依赖 platform/work 的迁移。

## 本仓发布前置

- 旧技能 release 写入已停且无外部调用方（规格 BND-011：先停旧写，再清表）。
- 备份先行：用 skill_release_cleanup 工具导出 skill_release 行并校验 SHA-256；迁移后共享表（catalog/tenant_skills/MCP/身份）行数与 hash 不变，恢复演练通过（VAL-217）。

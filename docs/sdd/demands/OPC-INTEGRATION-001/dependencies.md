# OPC-INTEGRATION-001 dependencies（knowledge-opc 只读投影）

> 本文件为主仓权威内容的只读投影，不得在参与仓独立修改。

# 依赖（knowledge-opc）

- 最早上线（迁移 000129~132；SQLite 链 000046）；功能开关灰度前关闭。
- platform-opc：桥配对（WeKnora JWTSecret 与 PLATFORM_BRIDGE_KEY 同源注入）；上游部署后开放 EXT 路由。

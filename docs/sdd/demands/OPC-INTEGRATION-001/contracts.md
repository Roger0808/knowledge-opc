# OPC-INTEGRATION-001 contracts（knowledge-opc 只读投影）

> 本文件为主仓权威内容的只读投影，不得在参与仓独立修改。

# 跨仓契约（knowledge-opc 视角）

- ← platform：桥个人上游 token（ValidateToken 查 auth_tokens 行）；调用上下文 JWT（aud 须等于 WEKNORA_INTEGRATION_CONTEXT_AUDIENCE=platform-integration；sub=上游 token user_id；args_digest 与 MCPInvocationArgsDigest 同算法；jti 一次性；≤120s）。
- EXT-001：发布/下载（Viewer+ 目标租户/角色命中；delisted 403）；manifest opc-client.json（sha256=payload digest；整包字节=bundle_sha256/X-SHA256）。
- EXT-002：策略（enabled/risk/require_approval）；user=confirm 写类 Ask；task=read_only 写类 Deny；决定单次绑定 invocation_id+args_digest+policy_version；未知结果不重发。

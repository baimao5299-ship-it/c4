# 社区实现审查：协议转换与 Chat→Responses 兼容性

日期：2026-08-28（只读审查，无源码修改、无服务重启）

## 结论先说

**Chat Completions 客户端可以调用 Responses 账号，但不是全局自动识别。** C3 的行为是“按分组显式声明转换方向，且只在原生 Chat 路由不可用时补差回退”：

1. 分组 `protocol_convert` 包含 `chat_to_resp`；
2. 先尝试选择支持 Chat 的账号；有可用账号时优先直连（零转换）；
3. 只有 Chat 路由格式不可用或账号全忙/禁用时，才选择同组 Responses 账号并执行 Chat→Responses 请求转换、Responses→Chat 响应转换；
4. 分组没有 `chat_to_resp`，或目标没有 Responses 账号时，不会猜测，分别返回“无此格式”或“无可用账号”。

这套“显式配置 + 原生优先 + 缺口回退”比全局自动猜测安全，避免把不同协议/模型误路由。

## 当前 C3 代码与测试证据

- 路由补差逻辑：`internal/proxy/caller.go:227-259`。默认客户端格式直连；仅在 `scheduler.ErrFormatUnavailable` 或 `ErrNoAvailable` 且分组方向匹配时转到目标协议。
- 转换调用器：`internal/proxy/caller_converted.go:42-187`。流式请求使用 `protoconv.StreamMapper`，非流式使用 `ConvertRequest`/`ConvertResponse`；用量从上游原始 Responses 帧提取，日志按客户端格式记录。
- 请求/响应转换实现：`internal/protoconv/chat_resp.go`；入口 `internal/protoconv/protoconv.go:29-53`。
- 已覆盖的回归测试：
  - `internal/proxy/converted_test.go:194-250`：Chat→Responses 流式与非流式，断言上游 `/v1/responses`、`messages→input`、SSE `[DONE]` 与用量映射。
  - `:403-418`：方向不匹配时不转换。
  - `:420-463`：多方向并存互不干扰，Responses 客户端仍直连。
  - `:465-470` 起：转换失败释放目标账号并发槽。
  - `:598-637`：Chat 账号禁用时回退到 Responses；Chat 账号 active 时直连优先。
  - `:639-715`：Chat 全忙时回退；目标也全忙返回 429 并保留 `Retry-After`。
  - `:717-760`：配置了方向但没有目标格式账号时返回 404 配置错误，不伪装成 429。
- 转换单测 `internal/protoconv/protoconv_test.go` 还覆盖数组内容、工具调用 ID、转义键/值、空/null 体、流式完成/失败事件等边界。

## 最小安全方案（建议保持，不要改成全局自动转换）

1. **配置层**：仅允许枚举方向（`chat_to_resp` 等），同一客户端格式最多一个目标方向；空数组表示关闭。
2. **路由层**：保持“原生格式优先，缺口/全忙才转换”；组不存在不转换；转换目标格式不存在返回 404。
3. **能力校验**：转换前再次确认目标模板支持 Responses、模型白名单命中、账号 `active` 且有并发槽；失败释放已占用槽位。
4. **协议校验**：请求先 `json.Valid`，严格校验 `stream/model/service_tier` 类型；转换失败只返回 400，不发上游请求。
5. **流式计费**：始终从目标协议原始帧取 usage；客户端断开记 499/abort，不触发切号；上游已开始输出后不要重放。
6. **可观测性**：日志同时保留 `client_format`、`route_format`、`conversion_direction`、`account_id`、attempt 序号；错误要区分“格式无账号(404)”“账号全忙(429)”“转换失败(400)”。不记录凭据或完整请求体。
7. **上线门槛**：每次协议转换改动至少运行 `go test ./internal/protoconv ./internal/proxy`，并用 fake Responses 上游验证流式、工具调用、usage、断连和并发槽释放。

## 社区成熟做法（仅作参考，不直接移植）

- **Sub2API（独立维护分支，v1.2.0，commit `a36754c5e1691107e7724e1b876b0173ebe3f642`）**：同样强调协议/请求构造与 retry、failover 分层；其错误策略将“上游事实、客户端展示、账号健康、切号预算”分离，直接返回类错误不进入 failover。C3 应继续保持显式转换和边界测试，不照搬其更大的账号/渠道模型。
  - https://github.com/is7Qin/sub2api/blob/local/ARCHITECTURE_PRINCIPLES.md
  - https://github.com/is7Qin/sub2api/blob/local/backend/internal/handler/failover_loop.go
  - https://github.com/is7Qin/sub2api/blob/local/backend/internal/service/upstream_error_policy.go
- **LiteLLM**：官方 README 将 Router 的 retry/fallback、负载均衡、成本追踪作为独立能力。可借鉴“策略与协议适配分离”，但不要把 fallback 当作协议自动转换。
  - https://github.com/BerriAI/litellm

## 不应照搬的做法/风险

- 不要根据请求路径或模型名“猜”目标协议；必须由分组配置声明，否则容易把 Chat 请求发到仅支持 Responses 的账号并产生 400/503。
- 不要把所有 4xx/模型不存在都当账号故障切换；确定性请求错误应直接返回，避免误伤健康账号。
- 不要为了“看起来成功”把转换失败、目标无账号、目标全忙都统一成 200/503；保留 400/404/429 语义便于定位。
- 不要引入无真实数据来源的余额/质量数字；未配置余额端点应显示未配置或过期，而不是 0。
- 不要复制社区项目的大规模数据库迁移、后台 worker 或缓存策略到 C3；C3 当前是 Beta、配置读取一次、无自动迁移，任何跨模型改动都应单独评审。

## 对 C3 的可执行建议（只建议，不代表已修改）

- 控制台显示“协议转换开关、当前命中方向、原生/转换回退原因”三项，减少误判。
- 增加一个只读诊断接口/页面：按分组列出客户端格式→目标格式、可用账号数、最近转换成功率与最近一次失败分类；不触发真实请求。
- 将现有转换回归测试纳入发布门禁，并补一条“同组原生 Chat 与 Responses 账号混合、Chat 原生账号失败后才转换”的端到端测试。
- 保持当前 `protocol_convert` API 兼容，不新增隐式默认值；配置变更需重启并在启动日志打印已加载方向（不打印密钥）。

# 中转站余额、故障切换与成本能力对比

日期：2026-08-27

## C3API 当前能力

### 上游余额/额度

- Codex OAuth/PAT 账号：`GET /api/admin/accounts/usage` 会通过 `internal/sdkbridge` 拉取并缓存上游快照，当前返回主窗口使用率与重置时间、credits balance、spend control。
- 静态 API Key 中转站：没有统一的余额协议。C3API 目前只返回网关侧用量，`upstream` 为空；除非该中转站提供明确的余额 API，否则不能从一个通用接口推断真实余额。
- 快照有 5 分钟成功缓存和 60 秒失败冷却，批量拉取并发上限为 8，避免把余额查询本身变成上游压力。

### 成本与消耗

- 网关侧已经记录请求、总 token、原始成本和倍率后的计费成本。
- 控制台的仪表盘、统计页和账号用量详情可查看 `raw_cost_usd` 与 `cost_usd`。
- 当前成本是按本地价格表和倍率计算的“网关成本”，不等于中转站账单；价格表错误时，统计也会跟着偏差。
- 明细、错误日志和聚合统计分开保存，保留周期分别受 `usage` 配置控制。

### 自动衔接

- 同一分组内的账号按权重和并发上限选号。
- 请求遇到网络错误、429 或 5xx 时，按 `proxy.failover_attempts` 继续尝试其他账号；默认总尝试次数为 3。
- 失败账号进入规则驱动的冷却/不健康状态，后续请求避开它。
- 4xx 等确定性请求错误不重放；流式响应已经开始后也不盲目重放，避免重复扣费和重复生成。
- 因此这是“有限重试 + 状态冷却”，不是无限循环，也不能保证上游已经处理但响应丢失时绝对不重复计费。

## 值得学习的成熟项目

| 项目 | 可借鉴部分 | 官方来源 |
|---|---|---|
| LiteLLM | 虚拟 Key、预算/速率限制、按模型和用户统计成本、负载均衡、fallback/retry、管理面 | https://github.com/BerriAI/litellm · https://docs.litellm.ai/docs/proxy/cost_tracking |
| One API | 渠道池、渠道余额/额度展示、权重负载均衡、失败重试、渠道分组和倍率 | https://github.com/songquanpeng/one-api |
| New API | One API 兼容数据模型、现代控制台、缓存计费、渠道健康与加权路由 | https://github.com/QuantumNous/new-api |
| Portkey Gateway | 条件路由、fallback、指数退避重试、统一观测面 | https://github.com/Portkey-AI/gateway |
| Bifrost | Go 网关的多供应商/多 Key 负载均衡、自动 fallback、预算控制、Prometheus/日志观测；适合参考模块拆分和控制面设计 | https://github.com/maximhq/bifrost · https://docs.getbifrost.ai/features/retries-and-fallbacks |

## 建议移植顺序

1. 为中转站账号增加“余额适配器”接口：每个供应商声明余额 URL、鉴权方式、响应字段映射；未配置适配器时显示“未提供余额接口”，不猜测。
2. 增加每个账号的健康、最近成功率、最近 429/5xx、余额更新时间，并把这些指标纳入调度权重，而不是只看静态权重。
3. 给每次请求增加 attempt 记录和最终选中账号，区分“请求失败后切换”和“上游已接受但响应丢失”，成本统计保留原始 token 与缓存 token 分类。
4. 增加余额阈值和低余额自动降权/停用；余额查询失败保持旧值并标记过期，避免一次网络抖动把账号误判为余额为零。

## 结论

C3API 已经具备本地成本统计和有限故障切换；只有 Codex 账号具备现成的上游额度查询。普通中转站余额必须按供应商单独适配，不能靠通用 OpenAI 兼容接口得到。最适合优先借鉴 LiteLLM 的成本/预算/路由设计，以及 One API/New API 的渠道余额与渠道池模型。

## 本轮落地

- `internal/billing/provider_balance.go` 提供可复用的余额适配器与缓存基础：支持
  Bearer、`X-API-Key`、无鉴权、JSON 路径映射、精确金额比较、TTL、stale-if-error
  和同账号并发合并。未配置余额接口时返回 `unconfigured`，不会伪造 0；刷新失败
  时最多保留有界的旧值，并标记 `stale` 与错误类别。
- 故障切换现在按请求记录已尝试账号，下一轮选号排除这些账号。规则事件仍然
  异步收敛，但同一请求不会在规则落地前重复命中刚失败的账号。
- 导入解析在 ZIP 解压前检查条目数与原始展开总量，并限制为严格 UTF-8 文本；
  Markdown 标题/列表前缀会被识别，不再把展示文字误当账号，也不会静默跳过列表
  中的 JSON/token。

## GitHub 源码复核（2026-08-28）

本次直接查看了以下公开仓库的路由实现，而不是只看项目宣传页：

- [LiteLLM Router](https://github.com/BerriAI/litellm/blob/main/litellm/router.py) 与
  [simple_shuffle](https://github.com/BerriAI/litellm/blob/main/litellm/router_strategy/simple_shuffle.py)：
  把 provider deployment 当成独立目标；健康目标先过滤，再按 `weight`/`rpm`/`tpm`
  加权选择。其 [least_busy](https://github.com/BerriAI/litellm/blob/main/litellm/router_strategy/least_busy.py)
  通过缓存记录每个 deployment 的在途数，选择最空闲目标。
- LiteLLM 的
  [cooldown_handlers](https://github.com/BerriAI/litellm/blob/main/litellm/router_utils/cooldown_handlers.py)：
  冷却键是 deployment，而不是整个 model group；429、401、404、408 和 5xx 的处理
  分开，错误阈值和冷却时间可按 deployment 覆盖，避免一次 429 冻结整组。
- [One API Channel](https://github.com/songquanpeng/one-api/blob/master/model/channel.go)：
  真实数据模型同时保存 `priority`、`weight`、`response_time`、`balance`、`models`
  和 `group`；多 Key 模式用独立状态表/索引轮询，只从启用 Key 中选，全部禁用时明确
  返回“无可用 Key”。
- [New API Channel](https://github.com/QuantumNous/new-api/blob/main/model/channel.go)：
  延续渠道级优先级、权重、模型映射、余额和多 Key 状态，并在更新 Key 数量时清理
  越界状态，防止旧索引误选。
- [Portkey Gateway](https://github.com/Portkey-AI/gateway)：把 retries、fallback、
  load balancing、虚拟 Key、RBAC 和可观测性作为独立能力；重试配置明确放在请求配置
  中，不让协议转换和故障切换混成一个隐式规则。

### 对本项目的可迁移结论

1. 采用 `routing_mode=accounts|upstreams` 双模式，旧账号组默认不变。
2. 上游成员必须有独立的目标 ID、并发槽、冷却和错误计数，不能借用账号 ID。
3. 先按优先级过滤，再在同优先级内做权重/最少在途选择；没有足够样本时不要生成“稳定性评分”。
4. 重试只针对可重试错误，并维护当前请求的已尝试目标集合；流式首帧后不 fallback。
5. 余额、上游成本倍率、用户收费倍率和请求用量分开保存，避免把管理展示值直接乘进现有账单。
6. 只移植这些边界清晰的机制，不复制其他项目的整套数据库、缓存或后台接管逻辑。

## 仍需按供应商配置

余额适配器目前是独立可接线模块，C3API 不会凭空猜测普通中转站的余额接口。要在
控制台显示某个供应商的真实余额，需要为该供应商提供 endpoint、鉴权方式和 JSON
字段映射；没有这些信息时应继续显示“未配置余额接口”。网络超时后上游已经接收的
请求仍可能产生重复生成，若供应商支持幂等键，后续应把客户端幂等键纳入上游请求
契约再开启自动重放。

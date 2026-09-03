# spec.md — 模型名归一化与分组可见性修复（交给 Codex 执行）

## 0. 你的角色与硬约束

诊断已经完成，根因已确定。**不要重新调查为什么分组会消失。** 本文件给的是结论，
你的任务是：让它编译通过、让测试通过、把表驱动用例铺开。

硬约束（违反任何一条就停下来报告，不要自作主张）：

1. **不许改测试来让测试通过。** 如果断言失败，说明实现有问题，或者断言写错了 ——
   两种情况都停下来报回来，附上失败输出原文。不要放宽断言、不要删用例、不要改期望值。
2. **编译错误原样报回来。** 不要发明绕法（不要加 `interface{}`、不要加类型断言、
   不要为了绕过类型检查而改签名）。贴原始 `go build` 输出。
3. **不确定就停下来问。** 特别是遇到"这两个模型名到底算不算同一个"这类语义判断，
   一律停手问，不要猜。
4. **不许顺手重构。** 只改第 3 节列出的文件，只做第 2 节列出的事。
   看到别处代码写得不好也不要动，diff 要能审。

## 1. 已确定的两个根因（结论，不是线索）

### 根因 A：`upstreamGroupHasRoute` 缺 `ModelFormats` 回落

`internal/service/group_availability.go` 的可用性判定只看 `u.Models`，
而调度器 `internal/scheduler/upstream_pool.go:457` 的 `upstreamSupportsModel`
在 `u.Models` 未命中时**还会回落检查 `u.ModelFormats`**（手工探测会先写
`ModelFormats`，目录刷新滞后）。

后果：上游实际能路由，但 `ensureGroupRoutable`（`internal/service/assignment.go:140`）
判定不可路由 → `continue` 静默丢弃 → 分组在用户端消失、建 key 返回 409。
这就是线上"17 个分组只看见 12 个""新建分组看不见"的机制。

**修法已实施**（见第 2 节 "已完成"）：加 `upstreamHasModel` 镜像调度器行为，
allowlist 分支和 legacy 分支都要回落。

### 根因 B：模型名去重折叠 `. _ -` 导致不同价模型被误合并

`modelMatchKey` 把大小写、`.`、`_`、`-` 全部折叠。原先 `uniquePublicModels`
拿它当去重键直接丢弃后来者，**没有任何价格校验**。

后果：`Claude-Fable-5.1` 与 `claude-fable-5-1`（确实是同一个，该合并）和
`deepseek-v3.2` 与 `deepseek.v3.2`（可能是不同产品，不该合并）在字符串上
无法区分，后者会被静默隐藏，且可能按前者价格展示。

**决策（我定的，不要改）：** 仅当"折叠后同名"**且**"解析价与官方价完全相同"时才合并。
价格不同就都保留 —— 多显示一行远好过隐藏一个模型或按错价展示。
这与仓库既有的 `modelLookupKeyCoalesced` + `priceEntriesEquivalent` 模式一致。

**修法已实施**：`distinctConfiguredModels`（只去空白和精确重复）+
`coalescePricedModelRows`（价格证明后合并），前端 `modelRows` 镜像同一闸门。

## 2. 当前状态与你要做的事

### 已完成（我已写入工作区，未提交）

- `internal/service/group_availability.go`：新增 `upstreamHasModel`、
  `upstreamFormatsContainModel`；allowlist 分支改用 `upstreamHasModel`；
  legacy 分支的 `confirmed` 集合并入 `u.ModelFormats` 的 key。
- `internal/service/channel_stats.go`：`uniquePublicModels` 替换为
  `distinctConfiguredModels`；新增 `coalescePricedModelRows`、
  `publicModelRowHasPrice`、`publicModelPricesEqual`；`UserChannelMetrics`
  循环改为「先按精确拼写建行并解析价格 → 再合并 → 再由合并结果回填
  `groupView.AllowedModels`」。
- `internal/service/user_flow_upstream_test.go`：加 `probed` helper 和 4 个
  `ModelFormats` 回落用例。
- `internal/service/channel_stats_test.go`：加
  `TestUserChannelMetricsKeepsSeparatelyPricedPunctuationVariants`。
- `web/src/pages/user/models.tsx`：`modelDisplayKey` 改名 `modelMatchKey`，
  `modelRows` 改为精确匹配优先 + 仅同价才合并。

**这些代码从未编译过**（我的环境没有 Go 工具链）。预期有编译错误。

### 你的任务

**任务 1：让它编译并通过测试。**

```
go build ./...
go test ./internal/service/... -count=1
```

已知风险点，优先检查：

- `int64PointersEqual` 定义在 `internal/service/model_alias.go:574`，
  同包，`channel_stats.go` 直接调用应该没问题 —— 确认一下。
- `channel_stats.go` 是否还 import 了 `strings`（`distinctConfiguredModels` 用到）。
- `PublicChannelModelPrice` 的字段名逐个核对，我是照 `channel_stats.go:47-75`
  抄的，可能有笔误。
- `domain.RequestFormat`、`domain.FormatOpenAIChat` 在 `internal/service` 包内
  是否可见（`group_availability.go` 已 import `domain`）。
- `user_flow_upstream_test.go` 里 `probed` helper 的 `models []string` 传 `nil`
  是否与 `upstream(...)` 的可变参数签名兼容。

**任务 2：核对我写的两个测试的期望值是否与实现一致。**

`TestUserChannelMetricsDeduplicatesLegacyModelSpellings` 断言
`AllowedModels == ["Claude-Fable-5.1", "gpt-5"]`。这个用例里三个 Claude 拼写
**都没有价格**（fake store 没 seed），按 `coalescePricedModelRows` 的规则
应该合并成一行。如果实际不是这样，**停下来报给我**，不要改断言 ——
可能是我的合并逻辑对"全部无价"这种情况处理不对。

**任务 3：铺开表驱动用例。** 给 `coalescePricedModelRows` 写单元测试
（新文件 `internal/service/channel_stats_coalesce_test.go`），用例表如下，
每行一个 case，`want` 是合并后剩下的 `Model` 序列：

| name | 输入行（Model + 价格） | want |
|---|---|---|
| 空输入 | 无 | 空 |
| 单行有价 | `a`(in=1) | `[a]` |
| 单行无价 | `a`(nil) | `[a]` |
| 同名同价合并 | `A.b`(in=1), `a-b`(in=1) | `[A.b]` |
| 同名不同价保留 | `A.b`(in=1), `a-b`(in=2) | `[A.b, a-b]` |
| 同名首行无价后行有价 | `A.b`(nil), `a-b`(in=1) | `[A.b]` 且 in==1 |
| 同名首行有价后行无价 | `A.b`(in=1), `a-b`(nil) | `[A.b]` 且 in==1 |
| 同名全无价 | `A.b`(nil), `a-b`(nil) | `[A.b]` |
| 三拼写两价 | `a.b`(in=1), `a-b`(in=1), `a_b`(in=2) | `[a.b, a_b]` |
| 三拼写、首行独价（已知局限） | `a.b`(in=1), `a-b`(in=2), `a_b`(in=2) | `[a.b, a-b, a_b]` |
| Mode 不同不合并 | `a.b`(mode=token,in=1), `a-b`(mode=call,in=1) | `[a.b, a-b]` |
| 官方价不同不合并 | `a.b`(off_in=1), `a-b`(off_in=2) | `[a.b, a-b]` |
| 不同模型不相干 | `a`(in=1), `b`(in=2) | `[a, b]` |
| 空 Model 原样保留 | `""`(nil), `a`(in=1) | `[, a]` |

用 `require` 断言，风格照 `channel_stats_test.go` 现有测试。

**关于倒数第二行"已知局限"** —— 这不是 bug，不要去"修"。`index` 只记录每个
折叠键的首行位置，所以当首行价格与后续行都不同时，后续两行即便彼此同价也不会
互相合并，结果多出一行。这个方向是**故意保守的**：宁可多显示一行，也不冒险
把两个可能不同的产品合并。真要处理需要按"折叠键 + 价格指纹"双重分组，
复杂度不值得。把它写成测试固化住现有行为就行。

**任务 4：前端编译。**

```
cd web && npm run build
```

`tsc -b` 会报类型错误。已知风险：`PRICE_FIELDS` 用了 `as const` 做索引访问，
`row[field]` 的类型可能过不了 —— 如果 TS 报错，**报回来给我定修法**，
不要用 `any` 或 `@ts-ignore` 绕过。

前端没有测试框架（`web/src` 下无 `*.test.*`），所以验证只到编译为止。
不要新建测试框架。

## 3. 允许改 / 禁止碰

**允许改：**

- `internal/service/group_availability.go`
- `internal/service/channel_stats.go`
- `internal/service/user_flow_upstream_test.go`
- `internal/service/channel_stats_test.go`
- `internal/service/channel_stats_coalesce_test.go`（新建）
- `web/src/pages/user/models.tsx`
- 只在编译确实要求时：`internal/service/model_alias.go`（仅补 helper，不改现有逻辑）

**禁止碰（改了就是越界，会被打回）：**

- `internal/billing/**` —— 计费逻辑，本次完全不动
- `internal/proxy/**` —— 转发与用量提取，本次完全不动
- `internal/repository/**`
- `internal/scheduler/**` —— 调度器是本次的**参照基准**，要让 service 对齐它，
  而不是反过来改它
- `internal/domain/**`
- `openapi/**` 和 `web/src/lib/api/schema.d.ts` —— 本次不改 API 契约
- `artifacts/**` —— 里面有旧代码副本，不要动也不要参照
- 任何 `go.mod` / `go.sum` / `package.json` 变更

## 4. 验收命令

按顺序全绿才算完成：

```
go build ./...
go test ./internal/service/... -count=1
go test ./internal/service/... -run 'TestUpstreamGroupHasRoute|TestUserKeyFlow' -count=1 -v
go test ./internal/service/... -run 'TestUserChannelMetrics' -count=1 -v
go vet ./internal/service/...
cd web && npm run build
```

回归护栏（确认没被我的改动弄坏）：

```
go test ./internal/... ./pkg/... ./cmd/... -count=1
```

## 5. 交回给我什么

1. 上面每条命令的输出（失败的贴原文，成功的贴最后一行就行）
2. `git diff` 全文
3. 你**做了但本 spec 没要求**的任何改动，单独列出来说明原因
4. 你停下来没做的事，以及原因

不要 commit，不要 push。我要先审 diff。

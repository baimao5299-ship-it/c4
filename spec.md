# spec.md — 模型名归一化与分组可见性修复（交给 Codex 执行）

## 第七轮（当前任务，只读这节）

HEAD 现在是 `660499a`。第六轮的改动加上这轮的前端改动一起验证。

### 这轮新增的改动（`660499a`）

创建分组对话框原先只能编辑备注、成员、模型：名称是只读的自动生成值，
可见性/协议/倍率是硬编码常量（public / auto / x1）。想建私有分组或非 ×1 倍率，
必须先建再打开编辑对话框改——而且分组会以「公开 + ×1 + 临时名字」的状态先存在
一段时间。倍率那一项属于真实的计费暴露窗口。

四项现在都可编辑，默认值不变。路由模式仍固定为上游池（保留原有约束：新分组不能
建成空的却显示可用）。

同时退掉了 `autoNameLabel`/`autoNameValue` 两个文案 key，新增 `autoNameHint`，
并重写了 `createDefaultsHint`（原文案让用户「创建后编辑」，现在不需要了）。

### 要跑的命令

```
go test ./internal/... -count=1
go vet ./internal/...
cd web && npm run build
```

### 重点关注

**`npm run build` 是这轮的核心。** 我改了 `groups.tsx` 和两个 locale JSON，
手工加删了文案 key，可能的问题：

- JSON 语法错误（我手工编辑的）
- 删掉的 `autoNameLabel`/`autoNameValue` 如果还有别处引用，会是运行时缺失而非
  编译错误——我搜过没有引用，但如果 build 有相关告警请贴回
- `ProtocolConvertCheckboxes` 和 `Select` 在创建对话框里的用法是照编辑对话框
  抄的，props 应该匹配
- 中英两个 locale 的 key 必须完全对齐，缺一个会导致该语言下显示原始 key

Go 侧这轮没改，跑 `./internal/...` 只是确认第六轮的改动仍然全绿。

### 硬约束不变

测试失败原样贴回，不要改测试期望值、不要改生产代码、不要给测试桩加特殊分支。
`TEST_DATABASE_URL` 跑不了就明确说跳过。

---

## 第六轮（已完成，存档）

第五轮你按要求在第一条命令失败时立刻停下并原样贴回了断言，这是正确处理。
那条失败我判定为**测试断言错误、生产代码正确**，已在 `771a101` 改断言。
HEAD 现在是 `ac3cd85`。

### 为什么改的是测试而不是代码

`TestTestUpstreamWithModelProbesExplicitModelMissingFromCatalogue` 第 83 行
原本断言 `ModelsCheckedAt` 非 nil。但这条测试自己第 82 行的注释写着
「a successful explicit probe must be routable after reload」，而盖章恰好破坏
这一点：路由在 `ModelsCheckedAt == nil` 时放行任何模型，一旦盖章就只认
`Models` 列表。该用例的上游从未验证过，`Models` 里只有手工探测的
`hidden-alias`，所以旧行为让目录里的 `listed-model` **变成不可路由**。
这就是分组消失那个 bug 的缩小版。断言已改为 `require.Nil`。

### 这轮还包含前端改动

用户反馈「设了优先级没效果」。查明两个原因，都在 `web/src/pages/groups.tsx`：

1. 创建分组对话框传了 `showMemberOptions={false}`，把优先级/权重/并发三个输入框
   隐藏了（那三个框在第 193-201 行受此 flag 控制），所以新建的成员一律是
   `defaultUpstreamMember` 的 `priority: '0'`，全挤在同一层。已去掉该 flag。
2. 优先级的真实语义是**严格故障转移**而非按比例分流
   （`scheduler/upstream_pool.go:283-312` 按 priority 分桶，只有最小数字那层做
   主序列，其余进 fallback；`:598-603` 主层耗尽才碰 fallback）。已加提示文案
   `groups.memberOptionsHint`（中英双语）说明这一点。

后端链路我查过，**没有问题**：handler、service 校验、repository 写入、调度器
读取全都正确处理了 priority。这轮不需要改后端。

### 要跑的命令

```
go test ./internal/... -count=1
go test ./internal/service/... -run 'TestTestUpstreamWithModel|TestListUpstreamModels|TestUpstream' -count=1 -v
go vet ./internal/...
cd web && npm run build
```

前端改了三个文件（`groups.tsx` 和两个 locale JSON），`npm run build` 必须跑。

### 重点关注

- `TestTestUpstreamWithModelProbesExplicitModelMissingFromCatalogue` 现在应该
  通过。如果它还失败，把新的失败原因贴回来。
- `TestTestUpstreamWithModelDoesNotDependOnCatalogueAvailabilityWhenExplicit`
  （同文件第 86 行）没有断言 `ModelsCheckedAt`，不该受影响。
- `npm run build` 里注意 locale JSON 的语法——我手工加了 key，如果 JSON
  格式错了 `tsc` 或 vite 会报错。

### 硬约束不变

测试失败就原样贴回，不要改测试期望值、不要改生产代码、不要给测试桩加特殊分支。
`TEST_DATABASE_URL` 跑不了就明确说跳过。

---

## 第五轮（已完成，存档）

第四轮验收通过。HEAD 现在是 `590a30e`。

这轮改动面比前几轮大，**动了接口签名**，涉及 4 个生产文件 + 3 个测试桩。

### 改动内容

第三轮（`f3df30d`）只修了 service 层的创建/更新路径，但仓库层
`recordUpstreamModelCapabilities` 仍然对任何非 nil 模型列表都盖
`ModelsCheckedAt`，所以后台点一次「重新探测」就能把残缺快照重新标记为权威，
把修复成果作废。

`UpstreamModelCapabilityStore.RecordUpstreamModelCapabilities` 末尾加了
`complete bool` 参数，只有完整运行才盖时间戳。不完整运行仍然写入已确认的模型
和警告码——不盖章是安全的，因为 service 层在调用前已经合并了保留快照，所以
记录的列表不会在这里变短。

`RecordUpstreamModels`（legacy 接口）行为不变，它表达不了「不完整」这个概念。

`recordExplicitUpstreamModel` 现在传 `complete=false`：手工探测单个模型按定义
不是目录验证，在那里盖章会把一个从未验证过的上游钉死成只有那一个模型可路由。

### 要跑的命令

```
go test ./internal/... -count=1
go test ./internal/service/... -run 'TestListUpstreamModels|TestUpstream|TestValidateAll|TestCreateUpstream|TestUpdateUpstream' -count=1 -v
go vet ./internal/...
cd web && npm run build
```

第一条跑整个 `./internal/...`，因为改了接口签名，要确认没有其它实现方漏改。

### 我预期不会失败的既有测试（失败就停下报我）

- `TestListUpstreamModelsRetainsPreviousSnapshotOnPartialRun`
  —— 这条测的是「部分运行仍保留旧快照的模型」，我没改那个合并逻辑
- `upstream_test.go` 第 740、1138、1159 行三处 `require.NotNil(ModelsCheckedAt)`
  —— 它们都在 `ValidationComplete=true` 的场景里（紧邻处都有
  `require.True(t, result.ValidationComplete)`），应该不受影响
- `TestValidateAllUpstreamsPersistsCanceledDiagnosticWithoutStorageError`
  —— 只是桩签名跟着改了，语义没动

新增测试是 `upstream_incomplete_stamp_test.go`，两条：不完整运行不盖章、
完整运行照常盖章。

### 如果有测试失败

原样贴回失败测试名、完整 `-v` 输出、相关断言代码。**不要改测试期望值，不要改
生产代码，不要给测试桩加特殊分支让它通过。** 这次改的是路由可用性的核心判断，
改错的方向有两种：一是不可用的上游被当作可用（用户拿到上游报错），二是可用的
上游被当作不可用（分组消失，就是我们一直在修的问题）。我需要自己判断是哪种。

### 关于 `TEST_DATABASE_URL`

仓库层的真实 SQL 行为测试（`pg_upstream_test.go`）在没配这个环境变量时会跳过。
如果你那边能起一个临时 PostgreSQL，跑一下会很有价值——这轮改的正是那个文件里
的 SQL 构造逻辑。**跑不了就明确说跳过了，不要假装跑过。**

---

## 第四轮（已完成，存档）

第三轮验收通过，输出完整、没有擅自改代码。HEAD 现在是 `536682d`。

这轮验证协议转换的修复：`internal/protoconv/chat_resp.go`。

### 改动内容

`appendChatInputItems` 把 user 和 assistant 放在同一个 case 里，共用
`appendContentParts`，而后者对所有文本部件硬编码 `"input_text"`。但 Responses
协议只允许 assistant 消息携带 `output_text`/`refusal`，所以任何带 assistant
历史的多轮对话经 Chat→Responses 转换后都会被上游整体拒绝：

```
invalid_value: Invalid value: 'input_text'.
Supported values are: 'output_text' and 'refusal'.  param: input[2].content[0]
```

线上分组 31（上游 coco企业015，模型 gpt-5.6-sol）就是这个错误。它从第二轮对话
起才失败，单轮请求没有 assistant 项、转换正常，所以看起来像是间歇性故障。

改动是把文本部件类型作为参数从调用方传入，按 role 选择。`appendContentParts`
只有一个调用点。developer 分支（`chat_resp.go:165`）保持 `input_text` 不变，
那对 Responses 是正确的。

新增测试 `chat_resp_assistant_role_test.go`，覆盖两种 role × 两种 content 形态
（字符串和数组部件）。

### 要跑的命令

```
go test ./internal/protoconv/... -count=1 -v
go test ./internal/proxy/... ./internal/sdkbridge/... -count=1
go vet ./internal/protoconv/... ./internal/proxy/...
```

### 重点关注

`protoconv_test.go` 里 `TestConvertRequestChatToResp` 的 `input[1]`（user，
第 77 行断言 `input_text`）和 `TestConvertRequestChatToRespLegacyMaxTokensAndImages`
（第 111 行同样断言 `input_text`）都是 user 消息，**不应该受影响**。如果这两条
失败，说明我把 user 分支也改坏了，停下报我。

`input[2]` 是 assistant 项，原先没有任何断言，所以旧行为没被钉住。

`internal/proxy` 和 `internal/sdkbridge` 里有 Codex/Responses 相关的集成测试，
它们可能间接依赖转换输出，一并跑一下。

### 如果有测试失败

原样贴回失败测试名、完整 `-v` 输出、相关断言代码。不要改测试期望值，也不要改
`chat_resp.go`。协议转换错了会让整类请求失败，我要自己判断。

---

## 第三轮（已完成，存档）

第二轮验收通过，感谢你如实报告 `go build ./...` 的 `artifacts/gen-check*` 问题
而不是擅自删除——那个目录确实在禁止范围内，且已证实不影响构建。

HEAD 现在是 `f3df30d`，我改了 `internal/service/upstream.go` 两处，请验证。

### 改动内容

`CreateUpstreamWithModelValidation` 和 `UpdateUpstreamWithModelValidation` 原先
在 `result.OK` 成立时**无条件**设置 `u.ModelsCheckedAt`。但 `OK` 的定义只是
`len(result.Models) > 0`（一个模型探测成功即可），而探测是刻意串行的
（`upstreamModelValidationConcurrency = 1`，每模型 12s），所以大目录常在探测
几个模型后超时。结果是一份残缺快照被标记为「已确认」，而所有路由检查
（`upstreamHasModel`、scheduler 的 `upstreamSupportsModel`）都把已标记的快照
当作**穷尽列表**，导致未探测到的模型全部不可路由，依赖它们的分组从用户 API
消失且无法建 key。

改动是把标记改为以 `result.ValidationComplete` 为条件。该字段已存在，批量探测
路径本来就在用它。

### 要跑的命令

```
go test ./internal/service/... -count=1
go test ./internal/service/... -run 'TestUpstream|TestUpdateUpstream|TestCreateUpstream|TestValidateModel|TestUserChannelMetrics|TestUpstreamGroupHasRoute' -count=1 -v
go vet ./internal/service/...
```

### 我预期不会失败的既有测试（如果失败请停下报我）

- `TestUpdateUpstreamWithModelValidationPersistsVerifiedSnapshot`
  —— 该场景 1 个模型探测成功、属完整运行，`ValidationComplete=true`，
  `ModelsCheckedAt` 仍应非 nil。**这条如果失败说明我的判断错了，立刻停下报我，
  不要改断言、不要改我的代码去迁就测试。**
- `TestUpdateUpstreamWithoutModelChangeKeepsLegacyWritePath`
  —— 该场景不触发探测，`ModelsCheckedAt` 本就应为 nil。
- `upstream_partial_snapshot_test.go` 里的全部用例
  —— 它们走批量探测路径（`recordUpstreamModels`），我没动那条路径。

### 如果有测试失败

把**失败测试名 + 完整 `-v` 输出 + 相关断言那几行代码**原样贴回。不要修改
测试期望值，也不要改 `upstream.go`。这两处改动涉及路由可用性判断，改错会让
不可用的上游被当作可用，我需要自己判断。

### 已知遗留（不要在这轮修）

`internal/repository/upstream_repo.go` 的 `recordUpstreamModelCapabilities`
（约 330-354 行）也在 `models != nil` 时无条件 `SetModelsCheckedAt`，所以批量
重探测路径仍可能发布不完整快照。修它需要把 `complete` 穿过
`UpstreamModelCapabilityStore` 接口，改动面更大，留到下一轮。

---

## 第二轮（已完成，存档）

第一轮你交回了 `internal/service/channel_stats_coalesce_test.go`，14 个用例和
第 2 节任务 3 的表格逐条对应，包括那条"已知局限"也如实固化了，**已验收并提交**。

但你**没有交回任何验收命令的输出**（第 5 节第 1 项）。仓库里除了那个新测试文件
没有其他改动，所以我无法区分两种情况：

- (a) 代码一次编译通过、测试全绿，确实无须改动
- (b) 任务 1（修编译错误）、任务 2（核对期望值）、任务 4（前端编译）没有执行

**第二轮只要一件事：把下面这些命令跑一遍，把输出原文交回来。**

```
go build ./...
go test ./internal/service/... -count=1
go test ./internal/service/... -run 'TestCoalescePricedModelRows|TestUserChannelMetrics|TestUpstreamGroupHasRoute|TestUserKeyFlow' -count=1 -v
go vet ./internal/service/...
cd web && npm run build
```

我要在 `-v` 输出里看到这两个测试名出现并 PASS：

- `TestUserChannelMetricsDeduplicatesLegacyModelSpellings` —— 管"该合并的合并了"
- `TestUserChannelMetricsKeepsSeparatelyPricedPunctuationVariants` —— 管"不该合并的没被合并"

前者是我最没把握的一处：那个用例里三个 Claude 拼写全都无价，走的是
`publicModelPricesEqual` 全 nil 相等那条路径。**如果它失败，停下来报给我，
不要改断言。**

注意 HEAD 已经前进到 `837e311`，我在第二个提交里修了 `web/src/pages/user/models.tsx`
的两个空格处理 bug（价格行会被误判为未定价）。所以前端必须重新编译验证。

第 4 节任务 4 里原本标注的 TS 风险（`PRICE_FIELDS` 索引访问）我已核实站得住：
那 16 个键全部存在于 `ChannelModelPrice` 类型上，值类型统一为
`number | null | undefined`，`as const` 下能推出联合类型。如果 `tsc` 仍然报错，
把错误原文交回来，**不要用 `any` 或 `@ts-ignore` 绕过**。

如果全绿，就只交命令输出，不要改任何代码。

---

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

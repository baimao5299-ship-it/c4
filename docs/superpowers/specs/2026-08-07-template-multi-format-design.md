# 模板多格式支持设计

## 状态

已批准（用户确认：多格式列表废弃默认、模型级按格式组织 {格式: [模型]}、未配置=全部模型、严格匹配拒绝、不向后兼容直接删、仅后端本轮）。

## 背景

模板当前只有单一 `default_format`（模板级）+ `model_formats`（模型级单格式覆盖）。需求：

1. **一个模板支持多个格式**——请求格式不在支持列表内直接拒绝；
2. **按格式组织模型**——每个格式下可限制"只支持某些模型"；
3. **不向后兼容**：`default_format` 全链路删除，无迁移逻辑，现有数据不处理。

## 数据结构变更

### domain（internal/domain/types.go）

```go
type Template struct {
	ID             int64
	Name           string
	BaseURL        string
	SupportedFormats []RequestFormat          // 模板支持的格式列表（非空、去重）
	Models         []string                   // 可服务模型集合（不变）
	FormatModels   map[RequestFormat][]string // 格式 → 该格式支持的模型列表；未配置的格式 = 全部 Models
	ModelMapping   map[string]string          // 不变
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
```

删除：`DefaultFormat`、`ModelFormats`。

### 方法重构

```go
// FormatsFor 模板支持的格式列表（= SupportedFormats）。
func (t *Template) FormatsFor() []RequestFormat

// Serves 模型是否可服务：models ∪ format_models 全部列表 ∪ mapping keys（不变语义，遍历新结构）。
func (t *Template) Serves(m string) bool

// FormatSupports 格式 f 是否支持模型 m：
//   - f 不在 SupportedFormats → false
//   - FormatModels[f] 配置了 → m ∈ 列表
//   - 未配置 → Serves(m)
func (t *Template) FormatSupports(f RequestFormat, m string) bool
```

`FormatFor(m)`（返回单格式）删除——格式由请求端决定，不再"推导"。

### ent schema（internal/ent/schema/template.go）

```go
field.JSON("supported_formats", []string{}),     // 替代 default_format（enum 单值 → JSON 数组）
field.JSON("format_models", map[string][]string{}), // 替代 model_formats（model→format 反转为 format→models）
```

删除 `default_format` 枚举字段与 `model_formats`。**不迁移**：现有 `default_format`/`model_formats` 数据列保留在 DB（ent 不自动删列），新代码不读不写。

## 契约变更（openapi/openapi.yaml）

- `TemplateCreate` / `Template`：删 `default_format`；`supported_formats`（必填，数组，minItems 1，枚举项）；`model_formats` → `format_models`（object，additionalProperties 为 string 数组）；
- 生成：oapi-codegen 重新生成 api.gen.go + openapi-typescript 重新生成 schema.d.ts（契约先行，前端下轮适配）。

## 匹配与拒绝逻辑

请求到达（chat/responses/anthropic 三端点，格式由端点决定）：

1. 请求格式 `f` ∉ 模板 `SupportedFormats` → 拒绝（400，复用 ErrFormatUnavailable 语义，消息含格式名）；
2. 请求模型 `m` ∉ 模板 `Serves(m)` → 拒绝（既有 no_account/不可服务语义）；
3. `FormatModels[f]` 配置了且 `m` ∉ 列表 → 拒绝（400，消息"format f does not support model m"）；
4. 通过 → 按 (f, m) 调度（现有预生成加权序列路径不变，键不变）。

## 校验规则（service.validateTemplate）

- `SupportedFormats` 非空、每项合法枚举、去重 → 否则 ErrInvalidInput；
- `FormatModels`：key 必须 ∈ SupportedFormats；每个列表非空、模型必须 ∈ `Models ∪ ModelMapping keys`（子集校验）→ 否则 ErrInvalidInput。

## 影响面

| 文件 | 变更 |
|---|---|
| `internal/domain/types.go` | Template 结构 + FormatsFor/Serves/FormatSupports |
| `internal/ent/schema/template.go` | 字段替换 + `go generate`（ent） |
| `internal/repository/mapping.go`、`template_repo.go` | ent ↔ domain 转换 + Create/Update Set 链 |
| `internal/service/service.go` | validateTemplate 重写 |
| `internal/scheduler/scheduler.go:263` | 模板格式匹配 `DefaultFormat != format` → `!Contains(SupportedFormats, format)` |
| `internal/scheduler/state.go` | buildRoutes 的 (format, model) 组合生成：SupportedFormats × Models 笛卡尔积，FormatModels[f] 裁剪 |
| `internal/handler/convert.go`、`template.go` | 转换 + Create/Put body 适配 |
| `internal/handler/api.gen.go` | 生成 |
| `web/src/lib/api/schema.d.ts` | 生成（仅契约，前端下轮） |
| `docs/admin-api.md` | 模板节更新 |

调度器快照键（format, model）不变——`buildRoutes` 从"模板一个格式"变"模板每 (f, m) 组合"生成路由桶；无账号可服务某组合时该桶空（与现状同）。

usage 日志 `format` 单值不变（记录实际请求格式）。

### 实现回填（Task 2）

- 拒绝语义：格式不支持 / `FormatModels[f]` 排除模型 / 无可用账号 均折叠为既有的 `ErrFormatUnavailable`，由 proxy 映射为 `404`「no account supports this request format」（复用既有语义，未做 400/专属消息）；上表「400 + 消息含格式名」为设计初稿措辞，以本行为准。
- 校验路径（admin 创建/更新）严格 `400`（`ErrInvalidInput`），e2e 已验。
- 调度回退桶 `(format, "")` 按 `SupportedFormats` 包含（tier2 语义，无模型可服务时为空）。

## 测试

- domain：FormatsFor/Serves/FormatSupports 各分支（未配置=全部、配置子集、f 不在 supported、m 不在 Serves）；
- service：校验（空列表/非法枚举/重复/format_models key 越界/模型非子集）；
- scheduler：模板多格式快照生成（笛卡尔积 + 裁剪）、按格式匹配（scheduler.go:263 路径）；
- repo：Create/Update 多格式往返（JSON 数组/嵌套 map）；
- handler：Create/Put body（supported_formats/format_models 绑定）、契约回归；
- 契约：生成后 go test ./... 全绿。

## 范围

- **本轮仅后端**（契约 + domain/ent + 调度 + 校验 + 测试）；前端模板页（supported_formats 多选 + format_models 编辑 UI）下轮适配。
- **不向后兼容**：无迁移逻辑；现有 default_format 数据不处理；文档注明破坏性变更。

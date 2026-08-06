# 模板多格式支持实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 模板从单一 `default_format` 改为多格式支持：`supported_formats`（模板级格式数组）+ `format_models`（按格式组织的模型限制，未配置=全部模型）；请求格式不匹配直接拒绝。**不向后兼容，直接删**，无迁移。

**Architecture:** domain/ent 字段体系替换（default_format/model_formats → supported_formats/format_models JSON），调度器 buildRoutes 按 (format, model) 组合生成桶（tier1 = Serves，tier2 = 不可服务），匹配严格化。

**Tech Stack:** Go 1.26 / ent v0.14.6 / chi / oapi-codegen v2.4.1 / pgxmock / testify

## Global Constraints

- **不向后兼容**：`default_format`/`model_formats` 全链路删除（契约/domain/ent/repo/service/handler/scheduler/proxy），无迁移逻辑，DB 旧列不处理
- **测试同样全新定义**：所有测试按新语义直接重写，不留旧字段名、旧测试（如 TestBuildRoutesModelFormatsOverride 直接删，换新名新语义）、不留"旧行为对照/兼容性"注释
- 契约唯一权威 `openapi/openapi.yaml`；生成：`cd internal/handler && go generate`（oapi-codegen）+ `cd internal/ent && go generate`（ent）+ `cd web && pnpm_config_verify_deps_before_run=false pnpm run gen:api`（schema.d.ts 仅契约先行，前端下轮适配，**不跑 web build**）
- 响应大写 Go 风格；请求体 snake_case；枚举 format：openai-chat/openai-responses/anthropic
- `supported_formats` 非空、每项合法枚举、去重；`format_models` key ∈ supported_formats、列表非空、模型 ∈ `Models ∪ ModelMapping keys`（**不含 format_models 自身，防自引用循环**）
- 匹配：请求格式 ∉ supported_formats → 400（ErrFormatUnavailable 语义）；模型级 format_models[f] 配置且 m ∉ 列表 → 400；通过后按 (format, model) 调度（桶键不变）
- 错误响应统一 `{"error": "..."}`；既有单资源断言除模板字段外不变
- 本轮仅后端；schema.d.ts 生成但不验证 web 编译

---

### Task 1: 全链路字段体系替换（契约 + 数据层 + 调度 + 校验 + 测试）

**Files:**
- Modify: `openapi/openapi.yaml`（Template/TemplateCreate schema）
- Modify: `internal/ent/schema/template.go` + 重新生成 `internal/ent/`
- Modify: `internal/domain/types.go`、`internal/domain/types_test.go`
- Modify: `internal/repository/mapping.go`、`internal/repository/template_repo.go`、`internal/repository/repository_test.go`
- Modify: `internal/service/service.go`、`internal/service/service_test.go`
- Modify: `internal/scheduler/scheduler.go`（buildRoutes + 回退桶）、`internal/scheduler/scheduler_test.go`、`internal/scheduler/bench_test.go`
- Modify: `internal/handler/convert.go`、`internal/handler/template.go`、`internal/handler/handler_test.go`、`internal/handler/api.gen.go`（生成）
- Modify: `internal/proxy/proxy_test.go`、`internal/proxy/forward_ext_test.go`（测试模板构造 helper）
- Modify: `web/src/lib/api/schema.d.ts`（生成，仅契约）

**Interfaces:**
- 生产：`domain.Template{SupportedFormats []RequestFormat; FormatModels map[RequestFormat][]string}`（删 DefaultFormat/ModelFormats）；`t.FormatSupports(f, m) bool`、`t.FormatsFor() []RequestFormat`、`t.Serves(m) bool`（Serves 遍历 FormatModels 值）；生成类型 `TemplateCreate{SupportedFormats []RequestFormat; FormatModels *map[string][]string}`、`Template{SupportedFormats []RequestFormat; FormatModels map[string][]string}`
- 消费：Task 2 回归验证

- [ ] **Step 1: 改契约（openapi.yaml）**

`components/schemas` 的 `TemplateCreate` 与 `Template` 替换（删 default_format/model_formats，加 supported_formats/format_models）：

```yaml
    TemplateCreate:
      type: object
      required: [name, base_url, supported_formats]
      properties:
        name: { type: string }
        base_url: { type: string }
        supported_formats:
          type: array
          minItems: 1
          items: { type: string, enum: [openai-chat, openai-responses, anthropic] }
        models:
          type: array
          items: { type: string }
        format_models:
          type: object
          additionalProperties:
            type: array
            items: { type: string }
        model_mapping:
          type: object
          additionalProperties: { type: string }
    Template:
      type: object
      required: [ID, Name, BaseURL, SupportedFormats, CreatedAt, UpdatedAt]
      properties:
        ID: { type: integer, format: int64 }
        Name: { type: string }
        BaseURL: { type: string }
        SupportedFormats:
          type: array
          items: { type: string, enum: [openai-chat, openai-responses, anthropic] }
        Models:
          type: array
          items: { type: string }
        FormatModels:
          type: object
          additionalProperties:
            type: array
            items: { type: string }
        ModelMapping:
          type: object
          additionalProperties: { type: string }
        CreatedAt: { type: string, format: date-time }
        UpdatedAt: { type: string, format: date-time }
```

（先 Read openapi.yaml 现有 TemplateCreate/Template 段，确认无其他引用 default_format 的地方；`/templates` 相关端点不动。）

- [ ] **Step 2: 生成 Go 契约并核对**

Run: `cd internal/handler && go generate`
Expected: api.gen.go 的 TemplateCreate/Template 更新。此时 `go build ./...` 失败（domain 未改）——正常，继续。

- [ ] **Step 3: 改 ent schema + 重新生成**

`internal/ent/schema/template.go` 字段替换：

```go
field.JSON("supported_formats", []string{}),       // 替代 default_format（枚举数组）
field.JSON("format_models", map[string][]string{}), // 替代 model_formats（格式 → 模型列表）
```

删除 `default_format` 与 `model_formats` 两行。然后：

Run: `cd internal/ent && go generate`
Expected: ent 重新生成（template.go/template_create.go 等 6 个文件更新，SetDefaultFormat/SetModelFormats 消失，SetSupportedFormats/SetFormatModels 出现）。

- [ ] **Step 4: 改 domain 类型与方法 + 重写测试**

`internal/domain/types.go`（删 DefaultFormat/ModelFormats/FormatFor，加新结构）：

```go
type Template struct {
	ID               int64
	Name             string
	BaseURL          string
	SupportedFormats []RequestFormat            // 模板支持的格式（非空、去重）
	Models           []string                   // 可服务模型集合
	FormatModels     map[RequestFormat][]string // 格式 → 该格式支持的模型列表；未配置 = 全部 Models
	ModelMapping     map[string]string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// FormatsFor 模板支持的格式列表。
func (t *Template) FormatsFor() []RequestFormat { return t.SupportedFormats }

// FormatSupports 格式 f 是否支持模型 m（模型级限制）：
// f 不在 SupportedFormats → false；FormatModels[f] 配置了 → m ∈ 列表；未配置 → true。
func (t *Template) FormatSupports(f RequestFormat, m string) bool {
	if !slices.Contains(t.SupportedFormats, f) {
		return false
	}
	if list, ok := t.FormatModels[f]; ok {
		return slices.Contains(list, m)
	}
	return true
}

// Serves 模型是否在可服务集合（models ∪ format_models 全部列表 ∪ mapping keys）内。
func (t *Template) Serves(m string) bool {
	if slices.Contains(t.Models, m) {
		return true
	}
	for _, list := range t.FormatModels {
		if slices.Contains(list, m) {
			return true
		}
	}
	if _, ok := t.ModelMapping[m]; ok {
		return true
	}
	return false
}
```

`internal/domain/types_test.go`：删 `TestTemplateFormatFor`，写：

```go
func TestTemplateFormatSupports(t *testing.T) {
	tpl := &Template{
		SupportedFormats: []RequestFormat{FormatOpenAIChat, FormatAnthropic},
		Models:           []string{"gpt-4o", "claude-3"},
		FormatModels:     map[RequestFormat][]string{FormatOpenAIChat: {"gpt-4o"}},
	}
	require.True(t, tpl.FormatSupports(FormatOpenAIChat, "gpt-4o"))
	require.False(t, tpl.FormatSupports(FormatOpenAIChat, "claude-3"), "配置了格式 → 仅列表内模型")
	require.True(t, tpl.FormatSupports(FormatAnthropic, "gpt-4o"), "未配置格式 → 全部模型")
	require.False(t, tpl.FormatSupports(FormatOpenAIResponses, "gpt-4o"), "格式不在 supported")
	require.True(t, tpl.Serves("gpt-4o"))
	require.False(t, tpl.Serves("nonexistent"))
	require.Equal(t, []RequestFormat{FormatOpenAIChat, FormatAnthropic}, tpl.FormatsFor())
}
```

- [ ] **Step 5: 改 repo 转换与读写**

`internal/repository/mapping.go`（toDomainTemplate）：

```go
func toDomainTemplate(t *ent.Template) *domain.Template {
	formats := make([]domain.RequestFormat, 0, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		formats = append(formats, domain.RequestFormat(f))
	}
	fm := make(map[domain.RequestFormat][]string, len(t.FormatModels))
	for k, v := range t.FormatModels {
		fm[domain.RequestFormat(k)] = v
	}
	return &domain.Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL,
		SupportedFormats: formats, Models: t.Models,
		FormatModels: fm, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}
```

`internal/repository/template_repo.go`（Create/Update 的 Set 链替换 + helpers）：

```go
// formatsToStrings 领域格式数组 → ent 字符串数组；formatModelsToStrings 同理（map 键转 string）。
func formatsToStrings(fs []domain.RequestFormat) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

func formatModelsToStrings(m map[domain.RequestFormat][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}
// Create: SetSupportedFormats(formatsToStrings(t.SupportedFormats)).SetFormatModels(formatModelsToStrings(t.FormatModels))
// Update: 同
```

- [ ] **Step 6: 改 service 校验**

`internal/service/service.go` validateTemplate 替换：

```go
func validateTemplate(t *domain.Template) error {
	if t.Name == "" {
		return ErrInvalidInput
	}
	u, err := url.Parse(t.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ErrInvalidInput
	}
	if len(t.SupportedFormats) == 0 {
		return ErrInvalidInput
	}
	seen := make(map[domain.RequestFormat]bool, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		if !f.Valid() || seen[f] {
			return ErrInvalidInput
		}
		seen[f] = true
	}
	for f, models := range t.FormatModels {
		if !seen[f] || len(models) == 0 {
			return ErrInvalidInput
		}
		for _, m := range models {
			// 模型必须在可服务集合（排除 format_models 自身，防自引用循环）
			if !slices.Contains(t.Models, m) {
				if _, ok := t.ModelMapping[m]; !ok {
					return ErrInvalidInput
				}
			}
		}
	}
	return nil
}
```

（service.go 需 import "slices"。）

- [ ] **Step 7: 改调度器 buildRoutes + 回退桶**

`internal/scheduler/scheduler.go` buildRoutes 两处：

```go
// 模型桶：模板 FormatSupports(format, model) 才进桶；Serves 分 tier1/tier2
for model := range modelSet(accs) {
	for _, format := range formats {
		var t1, t2 []*accountSnapshot
		for _, a := range accs {
			if a.tpl == nil || !a.tpl.FormatSupports(format, model) {
				continue
			}
			if a.tpl.Serves(model) {
				t1 = append(t1, a)
			} else {
				t2 = append(t2, a)
			}
		}
		if len(t1) == 0 && len(t2) == 0 {
			continue
		}
		rt := &route{}
		if len(t1) > 0 {
			rt.tier1 = newWeightedSeq(t1)
		}
		if len(t2) > 0 {
			rt.tier2 = newWeightedSeq(t2)
		}
		routes[routeKey{format, model}] = rt
	}
}
// 回退桶：模板 supported_formats 含 format 的全部账号（scheduler.go:263 处）
for _, format := range formats {
	var t2 []*accountSnapshot
	for _, a := range accs {
		if a.tpl == nil || !slices.Contains(a.tpl.SupportedFormats, format) {
			continue
		}
		t2 = append(t2, a)
	}
	if len(t2) == 0 {
		continue
	}
	routes[routeKey{format, ""}] = &route{tier2: newWeightedSeq(t2)}
}
```

（scheduler.go 需 import "slices"。`a.tpl.FormatFor` 调用删除。）

- [ ] **Step 8: 改 handler 转换与 body**

`internal/handler/convert.go` toAPITemplate：

```go
func toAPITemplate(t *domain.Template) Template {
	formats := make([]RequestFormat, 0, len(t.SupportedFormats))
	for _, f := range t.SupportedFormats {
		formats = append(formats, RequestFormat(f))
	}
	fm := make(map[string][]string, len(t.FormatModels))
	for k, v := range t.FormatModels {
		fm[string(k)] = v
	}
	return Template{
		ID: t.ID, Name: t.Name, BaseURL: t.BaseURL,
		SupportedFormats: formats, Models: t.Models,
		FormatModels: fm, ModelMapping: t.ModelMapping,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}
```

`internal/handler/template.go`（PostTemplates/PutTemplatesId 的 body 转换替换 + helpers）：

```go
func formatsFromBody(in []RequestFormat) []domain.RequestFormat {
	out := make([]domain.RequestFormat, 0, len(in))
	for _, f := range in {
		out = append(out, domain.RequestFormat(f))
	}
	return out
}

func formatModelsFromBody(m *map[string][]string) map[domain.RequestFormat][]string {
	if m == nil {
		return nil
	}
	out := make(map[domain.RequestFormat][]string, len(*m))
	for k, v := range *m {
		out[domain.RequestFormat(k)] = v
	}
	return out
}
// PostTemplates / PutTemplatesId 内：
SupportedFormats: formatsFromBody(in.SupportedFormats),
FormatModels:     formatModelsFromBody(in.FormatModels),
```

- [ ] **Step 9: 适配全部测试**

按引用清单逐一改（先 `grep -rn "DefaultFormat\|ModelFormats\|FormatFor" internal/ --include="*.go"` 找全）：

1. `internal/scheduler/scheduler_test.go`：
   - :61 helper `mkTemplate` → `&domain.Template{ID: id, BaseURL: "...", SupportedFormats: []domain.RequestFormat{format}, Models: models}`
   - :390 同（SupportedFormats 单元素）
   - `TestBuildRoutesModelFormatsOverride`（:446）重写为多格式语义：
     ```go
     func TestBuildRoutesFormatModelsLimit(t *testing.T) {
     	tpl := &domain.Template{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
     		Models: []string{"gpt-4o", "special"}, FormatModels: map[domain.RequestFormat][]string{domain.FormatAnthropic: {"special"}}}
     	// 断言：routes[{anthropic, special}] 存在且 tier1 非空；routes[{anthropic, gpt-4o}] 不存在（special 仅限）…
     	// 具体断言以现有测试结构为准（Read 后按既有 require 风格写）
     }
     ```
   - :528/:545 模板构造加 SupportedFormats
   - 其他 mkTemplate 调用点同步
2. `internal/scheduler/bench_test.go`：模板构造 helper 同步
3. `internal/repository/repository_test.go`：
   - :199/:213/:261 SQL mock 参数（ent JSON 字段序列化顺序——重新生成后 Set 链字段顺序变化，**跑测试看实际 SQL 断言失败信息调整**；JSON 数组/嵌套 map 参数用 gjson/JSONEq 风格或按 mock 的实际参数列表写）
   - :236-246 模板构造 + 断言（DefaultFormat → SupportedFormats；FormatFor("o3") → FormatSupports）
   - :332 模板构造
4. `internal/service/service_test.go`：:17 validateTemplate 用例改（SupportedFormats: []domain.RequestFormat{domain.RequestFormat("nope")} → 非法枚举；另加空列表/重复/format_models 越界/模型非子集用例）
5. `internal/handler/handler_test.go`：:69/:148/:149 断言改（FormatFor → FormatSupports / SupportedFormats round-trip）
6. `internal/proxy/proxy_test.go`、`internal/proxy/forward_ext_test.go`：模板构造 helper 加 `SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}`（format 变量处：forward_ext_test.go:172 的 `DefaultFormat: format` → `SupportedFormats: []domain.RequestFormat{format}`）

- [ ] **Step 10: 全量测试**

Run: `go test ./...`
Expected: 全绿（如 SQL mock 参数不匹配，按失败信息修正期望参数——ent 生成字段顺序以实际为准）

Run: `go vet ./... && golangci-lint run ./...`
Expected: 干净

- [ ] **Step 11: 重新生成 TS 类型（契约先行）**

Run: `cd web && pnpm_config_verify_deps_before_run=false pnpm run gen:api`
Expected: schema.d.ts 更新（SupportedFormats/FormatModels）。**不跑 web build**（前端下轮适配）。

- [ ] **Step 12: Commit**

```bash
git add openapi/openapi.yaml internal/ent internal/domain internal/repository internal/service internal/scheduler internal/handler internal/proxy web/src/lib/api/schema.d.ts
git commit -m "feat: template multi-format support (supported_formats + format_models)"
```

---

### Task 2: 回归 + e2e + 文档

**Files:**
- Modify: `docs/admin-api.md`（模板节：字段表/请求示例/枚举说明）
- Modify: `docs/superpowers/specs/2026-08-07-template-multi-format-design.md`（如实现偏差回填）

**Interfaces:**
- 消费：Task 1 全部产出

- [ ] **Step 1: 全量回归**

Run: `go test ./... && go vet ./... && golangci-lint run ./...`
Expected: 全绿

- [ ] **Step 2: e2e curl 冒烟（临时数据）**

本地起实例（docker PG + config，或复用控制器环境；token dev-admin-token）：

```bash
TOKEN=dev-admin-token
# 创建多格式模板
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"multi-fmt-test","base_url":"https://api.openai.com/v1","supported_formats":["openai-chat","anthropic"],"models":["gpt-4o","claude-3"],"format_models":{"anthropic":["claude-3"]}}' \
  "http://127.0.0.1:18080/admin/templates"
#   → 200，响应含 SupportedFormats: [...] 与 FormatModels: {...}
# 缺 supported_formats → 400
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"name":"bad","base_url":"https://api.openai.com/v1"}' \
  "http://127.0.0.1:18080/admin/templates"
#   → 400 {"error": ...}
# format_models 越界（key 不在 supported）→ 400；模型非子集 → 400（各一例）
# 单模板 GET → SupportedFormats round-trip；测完删除临时模板
```

（e2e 用临时创建的数据，测完清理；不破坏既有演示数据。）

- [ ] **Step 3: 文档同步**

docs/admin-api.md 模板节：字段表删 default_format/model_formats，加 supported_formats（必填数组）/format_models（{格式: [模型]}，未配置=全部模型）；请求示例更新；注明破坏性变更。

- [ ] **Step 4: Commit**

```bash
git add docs/admin-api.md docs/superpowers/specs
git commit -m "docs: template multi-format API reference"
```

---

## Self-Review 记录

- **Spec 覆盖**：supported_formats 废弃 default（Task 1 Step 1/3/4）✓；format_models 按格式组织（Task 1）✓；未配置=全部模型（FormatSupports）✓；严格匹配 400（buildRoutes 桶生成 + FormatSupports）✓；校验（service validateTemplate + 防自引用）✓；不迁移不兼容（无迁移代码）✓；仅后端（schema.d.ts 仅生成）✓
- **占位符扫描**：无 TBD/TODO；测试适配步骤标注"Read 后按既有风格写"处为测试细节（scheduler_test 具体断言、SQL mock 参数），步骤本身给出替换方向与代码骨架——可执行
- **类型一致性**：`SupportedFormats []RequestFormat`/`FormatModels map[RequestFormat][]string` 在 domain/ent 字符串数组（转换层）/repo/生成类型（[]RequestFormat + map[string][]string）间转换边界全部列出；`FormatSupports(f, m)`/`Serves(m)`/`FormatsFor()` 命名统一；buildRoutes 键不变
- **已知取舍**：scheduler tier2 模型桶语义保留（格式支持但模型不可服务）；回退桶 (format, "") 按 SupportedFormats 包含

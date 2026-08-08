// Package pricing 实现 litellm 模型价格同步：官方价格表拉取（fetcher）+ 定期
// 同步 worker（gronx cron 调度）。同步链路：fetch → UpsertFromLiteLLM（仓库
// 500/批、manual 行级互斥）→ service 快照 Reload。失败保留旧价格，下个周期
// 重试（不重试风暴）。
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"go-proxy-mini/internal/domain"
)

// FetchTimeout 单次拉取 HTTP 超时。
const FetchTimeout = 30 * time.Second

// maxPriceTableBytes 拉取体上限（官方表 ~3MB，64MB 防御上限防异常源打爆内存）。
const maxPriceTableBytes = 64 << 20

// Fetcher 拉取 litellm 官方价格表（GitHub raw JSON，URL 由 settings 配置可换）。
type Fetcher interface {
	Fetch(ctx context.Context, sourceURL string) (*FetchResult, error)
}

// FetchResult 一次拉取的解析结果。
type FetchResult struct {
	Rows    []*domain.Pricing // 数值有效的模型价格行（source=litellm）
	Skipped int               // 无效行跳过数（缺价/非正数/NaN/字段类型非法）
}

// NewFetcher 构造 HTTP fetcher（client 复用调用方连接池；nil → http.DefaultClient；
// 超时由 Fetch 内 context 保证，独立于 client.Timeout）。
func NewFetcher(client *http.Client) Fetcher {
	if client == nil {
		client = http.DefaultClient
	}
	return &httpFetcher{client: client}
}

type httpFetcher struct {
	client *http.Client
}

// Fetch GET 拉取并解析。状态非 200 / 体超限 / JSON 顶层解析失败 → 错误；单行
// 非法（字段类型错/缺价/NaN/非正数）→ 跳过该模型，不影响其余行。
func (f *httpFetcher) Fetch(ctx context.Context, sourceURL string) (*FetchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: %w", sourceURL, err)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: fetch %s: status %d", sourceURL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPriceTableBytes))
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch %s: read body: %w", sourceURL, err)
	}
	return Parse(data)
}

// litellmEntry litellm 价格表单模型行的目标字段（input/output_cost_per_token 与
// cache_read/creation_input_token_cost 为 USD/token；max_*_tokens 为上下文窗口；
// provider/mode/supports_prompt_caching 为显式元数据；其余字段（字符/音频价格、
// rpm/supports_vision 等）仅经 raw 完整镜像保留）。指针字段：null/缺失 → nil。
type litellmEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token"`
	CacheReadInputTokenCost     *float64 `json:"cache_read_input_token_cost"`
	CacheCreationInputTokenCost *float64 `json:"cache_creation_input_token_cost"`
	MaxInputTokens              *float64 `json:"max_input_tokens"`
	MaxOutputTokens             *float64 `json:"max_output_tokens"`
	Provider                    *string  `json:"litellm_provider"`
	Mode                        *string  `json:"mode"`
	SupportsPromptCaching       *bool    `json:"supports_prompt_caching"`
}

// Parse 解析 litellm 价格表 JSON：顶层 map model_name → 行。换算：per-token USD
// × 1e11 四舍五入取整 → 毫分/1M tokens（1 USD = 100,000 毫分 = 10⁻⁵ USD 精度；
// ×1e6 tokens × 1e5 毫分）。只保留数值有效行：input/output 价格均存在、有限且
// > 0（NaN/缺失/非正数/字段类型非法 → 跳过）；max_tokens/cache 价/元数据非法
// 或缺失 → nil（unknown），不参与有效性判定；raw 对通过判定的行无条件保存
// （整个原始条目 JSON 完整镜像，含未映射字段，如 rpm/supports_vision）。
func Parse(data []byte) (*FetchResult, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pricing: parse price table: %w", err)
	}
	res := &FetchResult{Rows: make([]*domain.Pricing, 0, len(raw))}
	for model, entry := range raw {
		p, err := parseEntry(model, entry)
		if err != nil {
			res.Skipped++
			continue
		}
		res.Rows = append(res.Rows, p)
	}
	return res, nil
}

func parseEntry(model string, raw json.RawMessage) (*domain.Pricing, error) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, err // 字段类型非法（如价格为字符串）→ 整行跳过
	}
	if !validCost(e.InputCostPerToken) || !validCost(e.OutputCostPerToken) {
		return nil, errors.New("pricing: missing or non-positive cost")
	}
	p := &domain.Pricing{
		Model:                     model,
		PromptPricePerMillion:     toMilliCentsPerMillion(*e.InputCostPerToken),
		CompletionPricePerMillion: toMilliCentsPerMillion(*e.OutputCostPerToken),
		Source:                    domain.PricingSourceLitellm,
		// raw 完整镜像：整个原始条目原样保存（含未映射字段；manual 行恒为 nil）。
		Raw: raw,
	}
	if t := windowTokens(e.MaxInputTokens); t > 0 {
		p.MaxInputTokens = &t
	}
	if t := windowTokens(e.MaxOutputTokens); t > 0 {
		p.MaxOutputTokens = &t
	}
	// cache 价与 litellm 表一致可缺失/为 0（很多模型无缓存价）→ nil；有效（非
	// nil、有限、> 0）→ ×1e11 毫分取指针。nil 不参与行有效性判定。
	if v := cacheCost(e.CacheReadInputTokenCost); v != nil {
		p.CacheReadPricePerMillion = v
	}
	if v := cacheCost(e.CacheCreationInputTokenCost); v != nil {
		p.CacheCreationPricePerMillion = v
	}
	// 显式元数据：缺失/非法 → nil（不影响行有效性；raw 已完整保留）。
	p.Provider = e.Provider
	p.Mode = e.Mode
	p.SupportsPromptCaching = e.SupportsPromptCaching
	return p, nil
}

// cacheCost 缓存价换算：缺失/非正/NaN/Inf → nil；有效 → 毫分/1M tokens 指针。
func cacheCost(v *float64) *int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return nil
	}
	m := toMilliCentsPerMillion(*v)
	return &m
}

// validCost 价格有效性：存在、有限、正数（litellm 表含 0/负/缺失占位行）。
func validCost(v *float64) bool {
	return v != nil && !math.IsNaN(*v) && !math.IsInf(*v, 0) && *v > 0
}

// toMilliCentsPerMillion 毫分/1M tokens = per-token USD × 1e11（浮点运算后四舍
// 五入取整；覆盖 litellm 全价格区间至 $0.00001/1M）。
func toMilliCentsPerMillion(perTokenUSD float64) int64 {
	return int64(math.Round(perTokenUSD * 1e11))
}

// windowTokens 上下文窗口：非法/非正 → 0（调用方转 nil）。
func windowTokens(v *float64) int64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
		return 0
	}
	if *v > float64(math.MaxInt64) {
		return 0
	}
	return int64(math.Round(*v))
}

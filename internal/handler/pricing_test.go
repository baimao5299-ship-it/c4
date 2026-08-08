package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/pricing"
	"go-proxy-mini/internal/repository"
)

// manualReq 构造 PricingManual（可选 cache 价；矩阵字段走显式设置）。
func manualReq(model string, prompt, completion int64, cache ...*int64) *repository.PricingManual {
	m := &repository.PricingManual{
		Model: model, PromptPricePerMillion: prompt, CompletionPricePerMillion: completion,
	}
	if len(cache) >= 1 {
		m.CacheReadPricePerMillion = cache[0]
	}
	if len(cache) >= 2 {
		m.CacheCreationPricePerMillion = cache[1]
	}
	return m
}

// fakePriceFetcher 测试用价格拉取器（返回注入结果/错误）。
type fakePriceFetcher struct {
	res *pricing.FetchResult
	err error
}

func (f *fakePriceFetcher) Fetch(ctx context.Context, sourceURL string) (*pricing.FetchResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

// newPricingRouter 价格面测试接线：admin token 中间件 + 契约路由 + 可选 fetcher
// 注入（sync 端点测试用）。
func newPricingRouter(t *testing.T, f pricing.Fetcher) (*AdminAPI, func(method, path, body string) *httptest.ResponseRecorder) {
	t.Helper()
	h, _, do := newListTestRouter(t)
	if f != nil {
		h.svc.SetPriceFetcher(f)
	}
	return h, do
}

// seedPricing 通过 service 公开路径造数：litellm 行走 sync（fake fetcher），
// manual 行走手动设价——与真实数据路径一致。
func seedPricing(t *testing.T, h *AdminAPI, f pricing.Fetcher) {
	t.Helper()
	h.svc.SetPriceFetcher(f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err, "seed sync")
	_, err = h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o", 100, 300))
	require.NoError(t, err)
	_, err = h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o-mini", 50, 150))
	require.NoError(t, err)
}

// TestPricingList 列表：分页（page/page_size）+ source/model 筛选 + 排序 +
// 非法参数 400。
func TestPricingList(t *testing.T) {
	f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
		{Model: "claude-3-5-sonnet", PromptPricePerMillion: 300000, CompletionPricePerMillion: 1500000, Source: domain.PricingSourceLitellm},
		{Model: "claude-3-opus", PromptPricePerMillion: 1500000, CompletionPricePerMillion: 7500000, Source: domain.PricingSourceLitellm},
	}, Skipped: 2}}
	h, do := newPricingRouter(t, f)
	seedPricing(t, h, f)

	// 全部：manual 2 + litellm 2
	rec := do(http.MethodGet, "/admin/pricing", "")
	require.Equal(t, 200, rec.Code, "list all: %s", rec.Body.String())
	var list PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(4), list.Total)
	require.Len(t, list.Rows, 4)

	// source 筛选
	rec = do(http.MethodGet, "/admin/pricing?source=manual", "")
	require.Equal(t, 200, rec.Code, "filter manual: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	for _, p := range list.Rows {
		require.Equal(t, PricingSource("manual"), p.Source)
	}
	rec = do(http.MethodGet, "/admin/pricing?source=litellm", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	for _, p := range list.Rows {
		require.Equal(t, PricingSource("litellm"), p.Source)
	}

	// model 模糊搜索（大小写不敏感）
	rec = do(http.MethodGet, "/admin/pricing?model=GPT-4O", "")
	require.Equal(t, 200, rec.Code, "model search: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total, "gpt-4o + gpt-4o-mini")

	// 排序 + 分页
	rec = do(http.MethodGet, "/admin/pricing?sort=model&order=desc", "")
	require.Equal(t, 200, rec.Code, "sort: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, "gpt-4o-mini", list.Rows[0].Model, "model desc 首行")
	require.Equal(t, "claude-3-5-sonnet", list.Rows[len(list.Rows)-1].Model, "model desc 末行")

	rec = do(http.MethodGet, "/admin/pricing?page=2&page_size=2", "")
	require.Equal(t, 200, rec.Code, "pagination: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(4), list.Total)
	require.Len(t, list.Rows, 2, "第二页两行")

	// 非法参数 400
	for _, tc := range []struct{ name, query string }{
		{"source 非法", "?source=bogus"},
		{"sort 非法", "?sort=bogus"},
		{"page_size 越界", "?page_size=101"},
		{"page_size 非数字", "?page_size=abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(http.MethodGet, "/admin/pricing"+tc.query, "")
			require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
			require.NotEmpty(t, errMsg(t, rec), "must be ErrorResponse JSON")
		})
	}
}

// TestPutPricing 手动设价：新模型成功 / 接管 litellm 行（source → manual）/
// 负数与非法 JSON 400。
func TestPutPricing(t *testing.T) {
	f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
		{Model: "gpt-4o", PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
	}}}
	h, do := newPricingRouter(t, f)

	// 新模型设价成功
	rec := do(http.MethodPut, "/admin/pricing/claude-3-5-sonnet",
		`{"prompt_price_per_million":300000,"completion_price_per_million":1500000}`)
	require.Equal(t, 200, rec.Code, "put: %s", rec.Body.String())
	var p Pricing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "claude-3-5-sonnet", p.Model)
	require.Equal(t, int64(300000), p.PromptPricePerMillion)
	require.Equal(t, int64(1500000), p.CompletionPricePerMillion)
	require.Equal(t, PricingSource("manual"), p.Source)

	// 接管 litellm 行：先同步入库再手动设价
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	rec = do(http.MethodPut, "/admin/pricing/gpt-4o", `{"prompt_price_per_million":999,"completion_price_per_million":888}`)
	require.Equal(t, 200, rec.Code, "takeover: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, PricingSource("manual"), p.Source, "手动设价接管 litellm 行")
	rec = do(http.MethodGet, "/admin/pricing?model=gpt-4o&source=manual", "")
	var list PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "接管后行 source=manual")

	// 带 cache 价设价：响应与列表 roundtrip
	rec = do(http.MethodPut, "/admin/pricing/m-cache",
		`{"prompt_price_per_million":10,"completion_price_per_million":20,"cache_read_price_per_million":30,"cache_creation_price_per_million":40}`)
	require.Equal(t, 200, rec.Code, "put with cache: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, int64(30), *p.CacheReadPricePerMillion, "响应含 cache_read")
	require.Equal(t, int64(40), *p.CacheCreationPricePerMillion, "响应含 cache_creation")
	rec = do(http.MethodGet, "/admin/pricing?model=m-cache", "")
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.Equal(t, int64(30), *list.Rows[0].CacheReadPricePerMillion, "列表 roundtrip cache_read")
	require.Equal(t, int64(40), *list.Rows[0].CacheCreationPricePerMillion)

	// 缺省 cache 字段 → nil（不设缓存价）
	rec = do(http.MethodPut, "/admin/pricing/m-nocache", `{"prompt_price_per_million":1,"completion_price_per_million":2}`)
	require.Equal(t, 200, rec.Code, "put without cache: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Nil(t, p.CacheReadPricePerMillion, "缺省 → nil")
	require.Nil(t, p.CacheCreationPricePerMillion)

	// 负数 400（含 cache 价负数）
	rec = do(http.MethodPut, "/admin/pricing/m-neg", `{"prompt_price_per_million":-1,"completion_price_per_million":10}`)
	require.Equal(t, 400, rec.Code, "negative: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/pricing/m-neg", `{"prompt_price_per_million":10,"completion_price_per_million":-1}`)
	require.Equal(t, 400, rec.Code, "negative completion: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/pricing/m-neg",
		`{"prompt_price_per_million":10,"completion_price_per_million":10,"cache_read_price_per_million":-1}`)
	require.Equal(t, 400, rec.Code, "negative cache_read: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/pricing/m-neg",
		`{"prompt_price_per_million":10,"completion_price_per_million":10,"cache_creation_price_per_million":-1}`)
	require.Equal(t, 400, rec.Code, "negative cache_creation: %s", rec.Body.String())

	// 非法 JSON 400
	rec = do(http.MethodPut, "/admin/pricing/m-neg", `{`)
	require.Equal(t, 400, rec.Code, "bad json: %s", rec.Body.String())
}

// TestDeletePricing 删除手动价：成功 200；litellm 行 → 409；不存在 → 404。
func TestDeletePricing(t *testing.T) {
	f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
		{Model: "claude-3-5-sonnet", PromptPricePerMillion: 300000, CompletionPricePerMillion: 1500000, Source: domain.PricingSourceLitellm},
	}}}
	h, do := newPricingRouter(t, f)
	_, err := h.svc.SyncPricingNow(context.Background())
	require.NoError(t, err)
	_, err = h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o", 100, 300))
	require.NoError(t, err)

	// 删除手动行成功
	rec := do(http.MethodDelete, "/admin/pricing/gpt-4o", "")
	require.Equal(t, 200, rec.Code, "delete manual: %s", rec.Body.String())
	var del DeletedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.True(t, del.Deleted)

	// litellm 行 → 409
	rec = do(http.MethodDelete, "/admin/pricing/claude-3-5-sonnet", "")
	require.Equal(t, 409, rec.Code, "delete litellm row must 409: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "claude-3-5-sonnet", "409 消息含 model")

	// 不存在 → 404
	rec = do(http.MethodDelete, "/admin/pricing/no-such-model", "")
	require.Equal(t, 404, rec.Code, "delete missing must 404: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "no-such-model", "404 消息含 model")
}

// TestPricingSync 手动触发同步：成功 200 返回拉取统计（+ 快照/库可见）；
// 拉取失败 → 502；url 未配置 → 400；manual 行不被覆盖。
func TestPricingSync(t *testing.T) {
	t.Run("success with stats", func(t *testing.T) {
		f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
			{Model: "claude-3-5-sonnet", PromptPricePerMillion: 300000, CompletionPricePerMillion: 1500000, Source: domain.PricingSourceLitellm},
			{Model: "claude-3-opus", PromptPricePerMillion: 1500000, CompletionPricePerMillion: 7500000, Source: domain.PricingSourceLitellm},
		}, Skipped: 3}}
		_, do := newPricingRouter(t, f)

		rec := do(http.MethodPost, "/admin/pricing/sync", "")
		require.Equal(t, 200, rec.Code, "sync: %s", rec.Body.String())
		var stats PricingSyncResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats))
		require.Equal(t, 2, stats.Rows)
		require.Equal(t, 3, stats.Skipped)
		require.Equal(t, 2, stats.Updated)

		// 拉取行入库且快照可见
		rec = do(http.MethodGet, "/admin/pricing", "")
		var list PricingListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Equal(t, int64(2), list.Total)
		require.Equal(t, PricingSource("litellm"), list.Rows[0].Source)
	})

	t.Run("fetch failure -> 502", func(t *testing.T) {
		_, do := newPricingRouter(t, &fakePriceFetcher{err: errors.New("upstream unreachable")})
		rec := do(http.MethodPost, "/admin/pricing/sync", "")
		require.Equal(t, http.StatusBadGateway, rec.Code, "fetch fail must 502: %s", rec.Body.String())
	})

	t.Run("url not set -> 400", func(t *testing.T) {
		h, do := newPricingRouter(t, &fakePriceFetcher{res: &pricing.FetchResult{}})
		_, err := h.svc.UpdateSetting(context.Background(), "price_source_url", "")
		require.NoError(t, err)
		rec := do(http.MethodPost, "/admin/pricing/sync", "")
		require.Equal(t, 400, rec.Code, "url not set must 400: %s", rec.Body.String())
	})

	t.Run("manual row not overwritten", func(t *testing.T) {
		f := &fakePriceFetcher{res: &pricing.FetchResult{Rows: []*domain.Pricing{
			{Model: "gpt-4o", PromptPricePerMillion: 250000, CompletionPricePerMillion: 1000000, Source: domain.PricingSourceLitellm},
		}}}
		h, do := newPricingRouter(t, f)
		_, err := h.svc.UpsertManualPricing(context.Background(), manualReq("gpt-4o", 100, 300))
		require.NoError(t, err)

		rec := do(http.MethodPost, "/admin/pricing/sync", "")
		require.Equal(t, 200, rec.Code, "sync: %s", rec.Body.String())
		var stats PricingSyncResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &stats))
		require.Equal(t, 0, stats.Updated, "manual 行不计入 updated")

		rec = do(http.MethodGet, "/admin/pricing?model=gpt-4o", "")
		var list PricingListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
		require.Len(t, list.Rows, 1)
		require.Equal(t, int64(100), list.Rows[0].PromptPricePerMillion, "manual 价不被拉取覆盖")
		require.Equal(t, PricingSource("manual"), list.Rows[0].Source)
	})
}

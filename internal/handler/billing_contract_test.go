package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	userapi "go-proxy-mini/internal/handler/user"
)

// 本文件覆盖 Phase 5 T4 契约扩展：pricing 矩阵 22 字段（PUT 解码 + 响应/列表
// 回显）、用户余额 USD 换算与 price_multiplier（null = 清除）、组倍率、
// service_tier_policy 设置校验、usagelog/stat 计费字段回显。

// TestPutPricingMatrixFields 手动设价矩阵 22 字段：全设 → 响应与列表 roundtrip；
// 缺省 → nil；负数/超界 fast_multiplier → 400。
func TestPutPricingMatrixFields(t *testing.T) {
	h, do := newPricingRouter(t, nil)

	// 全矩阵设价（priority 4 + flex 4 + above 13 + fast 1 = 22）
	body := `{
		"prompt_price_per_million":1000,"completion_price_per_million":2000,
		"priority_prompt_price_per_million":1100,"priority_completion_price_per_million":2100,
		"priority_cache_read_price_per_million":1110,"priority_cache_creation_price_per_million":2110,
		"flex_prompt_price_per_million":1200,"flex_completion_price_per_million":2200,
		"flex_cache_read_price_per_million":1210,"flex_cache_creation_price_per_million":2210,
		"above_threshold":128000,
		"above_prompt_price_per_million":1300,"above_completion_price_per_million":2300,
		"above_cache_read_price_per_million":1310,"above_cache_creation_price_per_million":2310,
		"above_priority_prompt_price_per_million":1400,"above_priority_completion_price_per_million":2400,
		"above_priority_cache_read_price_per_million":1410,"above_priority_cache_creation_price_per_million":2410,
		"above_flex_prompt_price_per_million":1500,"above_flex_completion_price_per_million":2500,
		"above_flex_cache_read_price_per_million":1510,"above_flex_cache_creation_price_per_million":2510,
		"fast_multiplier":20000
	}`
	rec := do(http.MethodPut, "/admin/pricing/matrix-model", body)
	require.Equal(t, 200, rec.Code, "put matrix: %s", rec.Body.String())
	var p Pricing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, int64(1100), *p.PriorityPromptPricePerMillion, "priority_prompt 回显")
	require.Equal(t, int64(2110), *p.PriorityCacheCreationPricePerMillion)
	require.Equal(t, int64(2200), *p.FlexCompletionPricePerMillion)
	require.Equal(t, int64(128000), *p.AboveThreshold, "above_threshold 回显")
	require.Equal(t, int64(2300), *p.AboveCompletionPricePerMillion)
	require.Equal(t, int64(1400), *p.AbovePriorityPromptPricePerMillion)
	require.Equal(t, int64(2410), *p.AbovePriorityCacheCreationPricePerMillion)
	require.Equal(t, int64(1500), *p.AboveFlexPromptPricePerMillion)
	require.Equal(t, int64(2510), *p.AboveFlexCacheCreationPricePerMillion)
	require.Equal(t, int64(20000), *p.FastMultiplier, "fast_multiplier 回显")

	// 列表 roundtrip
	rec = do(http.MethodGet, "/admin/pricing?model=matrix-model", "")
	require.Equal(t, 200, rec.Code, "list: %s", rec.Body.String())
	var list PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.Equal(t, int64(1300), *list.Rows[0].AbovePromptPricePerMillion, "列表 roundtrip above_prompt")
	require.Equal(t, int64(20000), *list.Rows[0].FastMultiplier)

	// 部分设价覆盖：其余矩阵字段清空（PUT 全量替换，nil = 清空）
	rec = do(http.MethodPut, "/admin/pricing/matrix-model",
		`{"prompt_price_per_million":1,"completion_price_per_million":2,"above_threshold":1000}`)
	require.Equal(t, 200, rec.Code, "partial overwrite: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, int64(1000), *p.AboveThreshold, "保留显式 above_threshold")
	require.Nil(t, p.PriorityPromptPricePerMillion, "未提供矩阵价 → nil（清空）")
	require.Nil(t, p.AbovePromptPricePerMillion)
	require.Nil(t, p.FastMultiplier)

	// 矩阵字段负数 → 400（service 校验）
	for _, tc := range []struct{ name, field string }{
		{"priority_prompt", `"priority_prompt_price_per_million":-1`},
		{"flex_completion", `"flex_completion_price_per_million":-1`},
		{"above_threshold", `"above_threshold":-1`},
		{"above_priority_cache_creation", `"above_priority_cache_creation_price_per_million":-1`},
		{"above_flex_prompt", `"above_flex_prompt_price_per_million":-1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(http.MethodPut, "/admin/pricing/matrix-model",
				fmt.Sprintf(`{"prompt_price_per_million":1,"completion_price_per_million":2,%s}`, tc.field))
			require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
		})
	}

	// fast_multiplier 越界（0 与 >100000）→ 400
	rec = do(http.MethodPut, "/admin/pricing/matrix-model",
		`{"prompt_price_per_million":1,"completion_price_per_million":2,"fast_multiplier":0}`)
	require.Equal(t, 400, rec.Code, "fast 0: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/pricing/matrix-model",
		`{"prompt_price_per_million":1,"completion_price_per_million":2,"fast_multiplier":100001}`)
	require.Equal(t, 400, rec.Code, "fast >100000: %s", rec.Body.String())

	// 幂等：svc 快照重载后 GetPrice 读到矩阵价（计费读路径）
	got, err := h.svc.GetPrice("matrix-model")
	require.NoError(t, err)
	require.Equal(t, int64(1000), *got.AboveThreshold, "快照读矩阵价")
}

// TestAdminUserBalanceMultiplier 管理面用户：balance USD 换算（创建/更新/列表/
// 详情回显）、price_multiplier 设置与 null = 清除。
func TestAdminUserBalanceMultiplier(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// 创建：balance 1.5 USD → 150000 毫分；price_multiplier 5000（×0.5）
	rec := doAdmin(http.MethodPost, "/admin/users",
		`{"email":"bill@example.com","password":"s3cret-pass","balance":1.5,"price_multiplier":5000}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
	var created User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, 1.5, *created.Balance, "创建响应 Balance 回显 USD")
	require.Equal(t, 5000, *created.PriceMultiplier, "创建响应倍率回显")

	// 列表回显 USD
	rec = doAdmin(http.MethodGet, "/admin/users?email=bill", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list UserListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, 1.5, *list.Rows[0].Balance, "列表 Balance USD")
	require.Equal(t, 5000, *list.Rows[0].PriceMultiplier)

	// 用户面 /user/auth/me 同语义 USD
	token, _ := loginUser(t, doUser, "bill@example.com")
	rec = doUser(http.MethodGet, "/user/auth/me", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "me: %s", rec.Body.String())
	var me struct {
		Balance *float64 `json:"Balance"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, 1.5, *me.Balance, "/user/auth/me Balance USD")

	// 更新 balance 0.25 USD（毫分边界 25000）；倍率改 20000
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID),
		`{"balance":0.25,"price_multiplier":20000}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "update: %s", rec.Body.String())
	var updated User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, 0.25, *updated.Balance, "更新后 Balance USD")
	require.Equal(t, 20000, *updated.PriceMultiplier)

	// 倍率 null = 清除为未设置（回退组倍率语义）
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"price_multiplier":null}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear multiplier: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Nil(t, updated.PriceMultiplier, "null → nil（未设置）")
	require.Equal(t, 0.25, *updated.Balance, "清除倍率不影响余额")

	// 缺省 = 不变：不传倍率，已有值保持
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"balance":0.5}`, "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Nil(t, updated.PriceMultiplier, "缺省字段保持原值")
	require.Equal(t, 0.5, *updated.Balance)

	// 非法：倍率超界 / 负余额 → 400
	rec = doAdmin(http.MethodPost, "/admin/users",
		`{"email":"x@example.com","password":"s3cret-pass","price_multiplier":100001}`, "")
	require.Equal(t, 400, rec.Code, "multiplier too large: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"price_multiplier":-5}`, "")
	require.Equal(t, 400, rec.Code, "multiplier negative: %s", rec.Body.String())
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"balance":-1}`, "")
	require.Equal(t, 400, rec.Code, "negative balance: %s", rec.Body.String())
}

// loginUser 登录拿 JWT（handler 测试 helper）。
func loginUser(t *testing.T, doUser func(method, path, body, token string) *httptest.ResponseRecorder, email string) (string, int64) {
	t.Helper()
	rec := doUser(http.MethodPost, "/user/auth/login",
		`{"email":"`+email+`","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "login: %s", rec.Body.String())
	var resp struct {
		Token string `json:"token"`
		User  User   `json:"user"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Token, *resp.User.ID
}

// TestGroupMultiplier 组倍率：POST 缺省 → 组默认 10000；POST 显式 → 回显；
// PUT 0 = 免费；PUT 超界 → 400；用户面 /user/groups 回显。
func TestGroupMultiplier(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// POST 缺省 → repo 语义 0 = 未指定 → 10000（fake 与真实 repo 同规则）
	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-default"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 10000, *g.PriceMultiplier, "缺省 → 组默认 ×1")

	// POST 显式倍率 20000
	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-mult","price_multiplier":20000}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 20000, *g.PriceMultiplier)

	// PUT 0 = 免费
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID), `{"name":"g-mult","price_multiplier":0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put free: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 0, *g.PriceMultiplier, "PUT 显式 0 = 免费")

	// PUT 超界 → 400
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID), `{"name":"g-mult","price_multiplier":100001}`, "")
	require.Equal(t, 400, rec.Code, "multiplier too large: %s", rec.Body.String())
	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-neg","price_multiplier":-1}`, "")
	require.Equal(t, 400, rec.Code, "multiplier negative: %s", rec.Body.String())

	// 用户面 /user/groups 回显倍率
	token, _ := registerAndGet(t, doUser, "gm@example.com")
	rec = doUser(http.MethodGet, "/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "user groups: %s", rec.Body.String())
	var groups []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.NotEmpty(t, groups)
	for _, gr := range groups {
		require.NotNil(t, gr.PriceMultiplier, "用户面组回显倍率")
	}
}

// TestServiceTierPolicySettings service_tier_policy_priority/flex 两个 key：
// 默认 passthrough；三值可设；非法 → 400。
func TestServiceTierPolicySettings(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// GET 默认值包含两个 policy key
	rec := do(http.MethodGet, "/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var settings []Setting
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
	seen := map[string]string{}
	for _, s := range settings {
		seen[*s.Key] = *s.Value
	}
	require.Equal(t, "passthrough", seen["service_tier_policy_priority"], "priority 默认 passthrough")
	require.Equal(t, "passthrough", seen["service_tier_policy_flex"], "flex 默认 passthrough")

	// 三值均可设
	for _, v := range []string{"passthrough", "strip", "reject"} {
		rec := do(http.MethodPut, "/admin/settings", `{"key":"service_tier_policy_priority","value":"`+v+`"}`)
		require.Equal(t, 200, rec.Code, "set %s: %s", v, rec.Body.String())
	}

	// 非法值 → 400
	rec = do(http.MethodPut, "/admin/settings", `{"key":"service_tier_policy_flex","value":"bogus"}`)
	require.Equal(t, 400, rec.Code, "invalid policy value: %s", rec.Body.String())
}

// TestLogsStatsBillingFields usagelog cost/billing_tier/above_hit/overdraft 与
// StatBucket cost 经管理面/用户面端点回显。
func TestLogsStatsBillingFields(t *testing.T) {
	doAdmin, doUser, store := newSharedRouters(t)
	token, userID := registerAndGet(t, doUser, "lb@example.com")

	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{
			ID: 1, UserID: userID, RequestID: "r-bill", Model: "gpt-4o",
			Format: domain.FormatOpenAIChat, StatusCode: 200,
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
			Cost: 500, BillingTier: "fast", AboveHit: true, Overdraft: false,
		},
	}
	store.stats = []*domain.StatBucket{
		{UserID: userID, Model: "gpt-4o", RequestCount: 1, Cost: 500, TotalTokens: 30},
	}
	store.mu.Unlock()

	// 管理面 /admin/logs
	rec := doAdmin(http.MethodGet, "/admin/logs", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var logs LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &logs))
	require.Len(t, logs.Rows, 1)
	r := logs.Rows[0]
	require.Equal(t, int64(500), *r.Cost, "log cost 回显（毫分）")
	require.Equal(t, "fast", *r.BillingTier, "log billing_tier 回显")
	require.True(t, *r.AboveHit)
	require.False(t, *r.Overdraft)

	// 用户面 /user/logs 同字段
	rec = doUser(http.MethodGet, "/user/logs", "", token)
	require.Equal(t, http.StatusOK, rec.Code)
	var ul userapi.LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ul))
	require.Len(t, ul.Rows, 1)
	require.Equal(t, int64(500), *ul.Rows[0].Cost, "user log cost 回显")

	// 管理面 /admin/stats
	rec = doAdmin(http.MethodGet, "/admin/stats", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var buckets []StatBucket
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &buckets))
	require.Len(t, buckets, 1)
	require.Equal(t, int64(500), *buckets[0].Cost, "stat bucket cost 回显")
}

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

func TestCreateTemplateValidates(t *testing.T) {
	svc := &Service{store: newFakeStore(), invalidate: func() {}, log: nil}
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "", BaseURL: "not-a-url", DefaultFormat: domain.RequestFormat("nope"),
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestCreateGroupRotateKeyFlow(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, invalidate: func() {}, log: nil}
	g, raw, err := svc.CreateGroup(context.Background(), "g1")
	require.NoError(t, err)
	require.NotEmpty(t, g.KeyHash)
	require.NotEmpty(t, raw, "key must be generated")
	raw2, err := svc.RotateGroupKey(context.Background(), g.ID)
	require.NoError(t, err)
	require.NotEqual(t, raw, raw2, "rotated key must differ")
	g2, err := svc.GetGroup(context.Background(), g.ID)
	require.NoError(t, err)
	require.NotEqual(t, g.KeyHash, g2.KeyHash, "hash must change")
}

func TestQueryStatsGranularity(t *testing.T) {
	fs := newFakeStore()
	fs.stats = []*domain.StatBucket{
		{BucketTime: mustTime("2026-08-01T10:00:00Z"), GroupID: 1, Model: "m", RequestCount: 10, TotalTokens: 100},
		{BucketTime: mustTime("2026-08-01T11:00:00Z"), GroupID: 1, Model: "m", RequestCount: 5, TotalTokens: 50},
	}
	svc := &Service{store: fs}
	rows, err := svc.QueryStats(context.Background(), repository.StatQuery{}, "day")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(15), rows[0].RequestCount, "day aggregation sums requests")
	require.Equal(t, int64(150), rows[0].TotalTokens)
}

// TestListQueryValidation service 层 sort/order 白名单校验：非法值 → ErrInvalidInput
// （handler 依赖此 400；fake store 不校验，故校验必须在 service 层前置）。
func TestListQueryValidation(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, invalidate: func() {}}

	_, _, err := svc.ListTemplates(context.Background(), repository.ListQuery{Order: "sideways"})
	require.ErrorIs(t, err, ErrInvalidInput, "非法 order")
	_, _, err = svc.ListTemplates(context.Background(), repository.ListQuery{Sort: "bogus"})
	require.ErrorIs(t, err, ErrInvalidInput, "非法 sort")
	_, _, err = svc.ListGroups(context.Background(), repository.ListQuery{Sort: "weight"})
	require.ErrorIs(t, err, ErrInvalidInput, "账号专属 sort 对分组无效")
	_, _, err = svc.ListAccountViews(context.Background(), repository.ListQuery{Sort: "bogus"})
	require.ErrorIs(t, err, ErrInvalidInput, "ListAccountViews 同样校验")

	rows, total, err := svc.ListTemplates(context.Background(), repository.ListQuery{Sort: "name", Order: "asc"})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, rows)
	views, total, err := svc.ListAccountViews(context.Background(), repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, total)
	require.Empty(t, views)
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// --- Task 4a: 批量删除/更新 ---

func seedTemplate(t *testing.T, svc *Service, name string) *domain.Template {
	t.Helper()
	created, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: "https://" + name + ".example.com", DefaultFormat: domain.FormatOpenAIChat,
	})
	require.NoError(t, err)
	return created
}

func seedAccount(t *testing.T, svc *Service, tplID int64, name string) *domain.Account {
	t.Helper()
	created, err := svc.CreateAccount(context.Background(), &domain.Account{
		Name: name, UpstreamKey: "k-" + name, TemplateID: tplID, MaxConcurrency: 4,
	})
	require.NoError(t, err)
	return created
}

func TestBatchDeleteTemplates(t *testing.T) {
	fs := newFakeStore()
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, log: nil}
	ctx := context.Background()
	t1 := seedTemplate(t, svc, "a")
	t2 := seedTemplate(t, svc, "b")
	before := invalidated
	require.NoError(t, svc.DeleteTemplatesBatch(ctx, []int64{t1.ID, t2.ID}))
	require.Greater(t, invalidated, before, "批量删除成功后必须 invalidate")
	_, err := svc.GetTemplate(ctx, t1.ID)
	require.ErrorIs(t, err, ErrNotFound, "批量删除后模板必须消失")
}

func TestBatchUpdateTemplates(t *testing.T) {
	fs := newFakeStore()
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, log: nil}
	ctx := context.Background()
	t1 := seedTemplate(t, svc, "a")
	name := "renamed"
	before := invalidated
	require.NoError(t, svc.UpdateTemplatesBatch(ctx, []int64{t1.ID}, repository.TemplatePatch{Name: &name}))
	require.Greater(t, invalidated, before, "批量更新成功后必须 invalidate")
	got, err := svc.GetTemplate(ctx, t1.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
}

func TestBatchDeleteAccounts(t *testing.T) {
	fs := newFakeStore()
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, log: nil}
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "t")
	a1 := seedAccount(t, svc, tpl.ID, "a1")
	a2 := seedAccount(t, svc, tpl.ID, "a2")
	before := invalidated
	require.NoError(t, svc.DeleteAccountsBatch(ctx, []int64{a1.ID, a2.ID}))
	require.Greater(t, invalidated, before, "批量删除成功后必须 invalidate")
	_, err := svc.GetAccount(ctx, a1.ID)
	require.ErrorIs(t, err, ErrNotFound, "批量删除后账号必须消失")
}

func TestBatchUpdateAccounts(t *testing.T) {
	fs := newFakeStore()
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, log: nil}
	ctx := context.Background()
	tpl := seedTemplate(t, svc, "t")
	a := seedAccount(t, svc, tpl.ID, "a1")
	st := domain.StatusDisabled
	before := invalidated
	require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{a.ID}, repository.AccountPatch{Status: &st}))
	require.Greater(t, invalidated, before, "批量更新成功后必须 invalidate")
	got, err := svc.GetAccount(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusDisabled, got.Status)
}

type fakeKeyRegistrar struct {
	deleted []string
}

func (k *fakeKeyRegistrar) Upsert(hash string, groupID int64) {}

func (k *fakeKeyRegistrar) Delete(hash string) { k.deleted = append(k.deleted, hash) }

func TestBatchDeleteGroupsKeyCleanup(t *testing.T) {
	fs := newFakeStore()
	keys := &fakeKeyRegistrar{}
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, keys: keys, log: nil}
	ctx := context.Background()
	g1, _, err := svc.CreateGroup(ctx, "g1")
	require.NoError(t, err)
	g2, _, err := svc.CreateGroup(ctx, "g2")
	require.NoError(t, err)
	before := invalidated
	require.NoError(t, svc.DeleteGroupsBatch(ctx, []int64{g1.ID, g2.ID}))
	require.Greater(t, invalidated, before, "批量删除成功后必须 invalidate")
	require.ElementsMatch(t, []string{g1.KeyHash, g2.KeyHash}, keys.deleted, "分组 key 必须全部清理")
	_, err = svc.GetGroup(ctx, g1.ID)
	require.ErrorIs(t, err, ErrNotFound, "批量删除后分组必须消失")
}

func TestBatchUpdateGroups(t *testing.T) {
	fs := newFakeStore()
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, log: nil}
	ctx := context.Background()
	g, _, err := svc.CreateGroup(ctx, "g1")
	require.NoError(t, err)
	name := "renamed"
	before := invalidated
	require.NoError(t, svc.UpdateGroupsBatch(ctx, []int64{g.ID}, repository.GroupPatch{Name: &name}))
	require.Greater(t, invalidated, before, "批量更新成功后必须 invalidate")
	got, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)
}

// TestBatchDeleteInvalidIDs 空/超长/重复 ids → ErrInvalidInput（三资源同型）。
func TestBatchDeleteInvalidIDs(t *testing.T) {
	svc := &Service{store: newFakeStore(), invalidate: func() {}, log: nil}
	ctx := context.Background()
	cases := map[string][]int64{
		"nil ids":           nil,
		"空 ids":             {},
		"超长 ids（101 个）": make([]int64, 101),
		"重复 ids":           {1, 1},
	}
	for name, ids := range cases {
		err := svc.DeleteTemplatesBatch(ctx, ids)
		require.ErrorIs(t, err, ErrInvalidInput, "templates %s", name)
		err = svc.DeleteAccountsBatch(ctx, ids)
		require.ErrorIs(t, err, ErrInvalidInput, "accounts %s", name)
		err = svc.DeleteGroupsBatch(ctx, ids)
		require.ErrorIs(t, err, ErrInvalidInput, "groups %s", name)
	}
}

// TestBatchUpdatePatchValidation patch 校验失败 → ErrInvalidInput（校验在 store 调用前）。
func TestBatchUpdatePatchValidation(t *testing.T) {
	svc := &Service{store: newFakeStore(), invalidate: func() {}, log: nil}
	ctx := context.Background()
	empty := ""
	badURL := "not-a-url"
	badFormat := domain.RequestFormat("nope")

	t.Run("templates", func(t *testing.T) {
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{Name: &empty}), ErrInvalidInput, "空 name")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{BaseURL: &badURL}), ErrInvalidInput, "非法 BaseURL")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{DefaultFormat: &badFormat}), ErrInvalidInput, "非法 DefaultFormat")
		require.ErrorIs(t, svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{ModelFormats: &map[string]domain.RequestFormat{"m": badFormat}}), ErrInvalidInput, "非法 ModelFormats")
	})
	t.Run("accounts", func(t *testing.T) {
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{Name: &empty}), ErrInvalidInput, "空 name")
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{UpstreamKey: &empty}), ErrInvalidInput, "空 UpstreamKey")
		badTID := int64(0)
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{TemplateID: &badTID}), ErrInvalidInput, "TemplateID <= 0")
		badWeight := -1
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{Weight: &badWeight}), ErrInvalidInput, "Weight < 0")
		badMC := 0
		require.ErrorIs(t, svc.UpdateAccountsBatch(ctx, []int64{1}, repository.AccountPatch{MaxConcurrency: &badMC}), ErrInvalidInput, "MaxConcurrency < 1")
	})
	t.Run("groups", func(t *testing.T) {
		require.ErrorIs(t, svc.UpdateGroupsBatch(ctx, []int64{1}, repository.GroupPatch{Name: &empty}), ErrInvalidInput, "空 name")
	})
	t.Run("合法 patch 不触发校验错误", func(t *testing.T) {
		valid := domain.FormatAnthropic
		err := svc.UpdateTemplatesBatch(ctx, []int64{1}, repository.TemplatePatch{DefaultFormat: &valid})
		require.NotErrorIs(t, err, ErrInvalidInput)
		require.ErrorIs(t, err, ErrNotFound, "fake 缺 id → 404 映射")
	})
}

// TestBatchNotFoundMapping 缺失 id → repo.ErrNotFound → service.ErrNotFound（含缺失 id 详情）。
func TestBatchNotFoundMapping(t *testing.T) {
	svc := &Service{store: newFakeStore(), invalidate: func() {}, log: nil}
	ctx := context.Background()

	err := svc.DeleteTemplatesBatch(ctx, []int64{999})
	require.ErrorIs(t, err, ErrNotFound, "repo.ErrNotFound 映射为 service.ErrNotFound")
	require.Contains(t, err.Error(), "999", "404 消息含缺失 id")

	err = svc.DeleteAccountsBatch(ctx, []int64{999})
	require.ErrorIs(t, err, ErrNotFound)
	require.Contains(t, err.Error(), "999")

	err = svc.UpdateTemplatesBatch(ctx, []int64{999}, repository.TemplatePatch{})
	require.ErrorIs(t, err, ErrNotFound, "批量更新同样映射 404")
	require.Contains(t, err.Error(), "999")

	// 分组缺 id 由 GetGroup 前置检查拦截（直接返回 service.ErrNotFound）
	err = svc.DeleteGroupsBatch(ctx, []int64{999})
	require.ErrorIs(t, err, ErrNotFound)
}

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

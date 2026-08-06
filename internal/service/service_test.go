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
	cases := []struct {
		name string
		tpl  *domain.Template
	}{
		{"name empty", &domain.Template{Name: "", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}},
		{"bad url", &domain.Template{Name: "t", BaseURL: "not-a-url", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}},
		{"empty supported_formats", &domain.Template{Name: "t", BaseURL: "https://u/v1"}},
		{"invalid enum", &domain.Template{Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.RequestFormat("nope")}}},
		{"duplicate formats", &domain.Template{Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIChat}}},
		{"format_models key not in supported", &domain.Template{
			Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels: map[domain.RequestFormat][]string{domain.FormatAnthropic: {"m"}},
		}},
		{"format_models empty list", &domain.Template{
			Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels: map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {}},
		}},
		{"format_models model outside serve set", &domain.Template{
			Name: "t", BaseURL: "https://u/v1", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			FormatModels: map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"gpt-4o"}},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateTemplate(context.Background(), tc.tpl)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
	// 合法：format_models 模型 ∈ Models
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{
		Name: "t", BaseURL: "https://u/v1",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"gpt-4o"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"gpt-4o"}},
	})
	require.NoError(t, err)
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

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

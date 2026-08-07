package scheduler

import (
	"context"
	"testing"
	"time"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/rule"
)

// newTestRuleEngine 空规则引擎（bench 不依赖规则路径，满足 New 的非 nil 要求）。
func newTestRuleEngine(b *testing.B) *rule.RuleEngine {
	b.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	if err := re.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	return re
}

// 5000 账号快照（压测场景复现）：Select 单次耗时对照（O(1) 序列取用）。
func BenchmarkSelect5000Accounts(b *testing.B) {
	s := schedulerWithAccounts(b, 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		if err != nil {
			b.Fatal(err)
		}
		s.Release(sel.AccountID)
	}
}

func schedulerWithAccounts(b *testing.B, n int) *Scheduler {
	b.Helper()
	tpl := &domain.Template{ID: 1, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"}}
	accs := make(map[int64][]*domain.Account)
	for i := int64(1); i <= int64(n); i++ {
		accs[10] = append(accs[10], &domain.Account{
			ID: i, TemplateID: 1, Template: tpl, UpstreamKey: "k",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 100000,
		})
	}
	s := New(Config{DefaultMaxConcurrency: 100000, SyncInterval: time.Hour}, newMemLoader(accs), newTestRuleEngine(b), nil)
	if err := s.InvalidateAllSync(); err != nil {
		b.Fatal(err)
	}
	return s
}

package repository

import (
	"context"
	"encoding/json"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent"
	"go-proxy-mini/internal/ent/rule"
)

// RuleStore 规则存储接口；service 层通过 Repos.Rules 使用。
type RuleStore interface {
	ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) // nil = 全部；priority 升序
	CreateRule(ctx context.Context, r domain.Rule) (int64, error)
	UpdateRule(ctx context.Context, r domain.Rule) error
	DeleteRule(ctx context.Context, id int64) error
	CountRules(ctx context.Context) (int64, error)
}

type RuleRepo struct{ client *ent.Client }

var _ RuleStore = (*RuleRepo)(nil)

func (r *RuleRepo) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	pred := r.client.Rule.Query()
	if enabled != nil {
		pred = pred.Where(rule.Enabled(*enabled))
	}
	rows, err := pred.Order(ent.Asc(rule.FieldPriority)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Rule, 0, len(rows))
	for _, row := range rows {
		out = append(out, *toDomainRule(row))
	}
	return out, nil
}

func (r *RuleRepo) CreateRule(ctx context.Context, rl domain.Rule) (int64, error) {
	row, err := r.client.Rule.Create().
		SetName(rl.Name).SetEnabled(rl.Enabled).SetPriority(rl.Priority).
		SetWhen(whenToMap(rl.When)).SetThen(thenToMap(rl.Then)).
		Save(ctx)
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (r *RuleRepo) UpdateRule(ctx context.Context, rl domain.Rule) error {
	_, err := r.client.Rule.UpdateOneID(rl.ID).
		SetName(rl.Name).SetEnabled(rl.Enabled).SetPriority(rl.Priority).
		SetWhen(whenToMap(rl.When)).SetThen(thenToMap(rl.Then)).
		Save(ctx)
	return err
}

func (r *RuleRepo) DeleteRule(ctx context.Context, id int64) error {
	if err := r.client.Rule.DeleteOneID(id).Exec(ctx); err != nil {
		return errMissingID(err, id)
	}
	return nil
}

func (r *RuleRepo) CountRules(ctx context.Context) (int64, error) {
	n, err := r.client.Rule.Query().Count(ctx)
	if err != nil {
		return 0, err
	}
	return int64(n), nil
}

func toDomainRule(row *ent.Rule) *domain.Rule {
	return &domain.Rule{
		ID: row.ID, Name: row.Name, Enabled: row.Enabled, Priority: row.Priority,
		When: whenFromMap(row.When), Then: thenFromMap(row.Then),
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

// whenToMap/thenToMap 领域 when/then → ent JSON 字段值；whenFromMap/thenFromMap 反向。
// 用 json 标签 round-trip（nil 指针字段自然省略、整数精度无损），避免逐字段手写互转。
func whenToMap(w domain.RuleWhen) map[string]any {
	b, _ := json.Marshal(w)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func whenFromMap(m map[string]any) domain.RuleWhen {
	b, _ := json.Marshal(m)
	var w domain.RuleWhen
	_ = json.Unmarshal(b, &w)
	return w
}

func thenToMap(t domain.RuleThen) map[string]any {
	b, _ := json.Marshal(t)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func thenFromMap(m map[string]any) domain.RuleThen {
	b, _ := json.Marshal(m)
	var t domain.RuleThen
	_ = json.Unmarshal(b, &t)
	return t
}

package rule

import (
	"fmt"
	"time"

	"go-proxy-mini/internal/domain"
)

// ValidateWhen when 语义校验（字段白名单/未知字段由 service 层 JSON 反序列化
// DisallowUnknownFields 挡下；此处查语义）：
//   - kind 必须为 ok/429/error 之一
//   - 计数阈值 ≥ 0、window_seconds ≥ 1
//   - 比例 ∈ [0,1] 且必须配 count_total_ge（比例样本下限）
func ValidateWhen(w domain.RuleWhen) error {
	if w.Kind != nil && kindFromString(*w.Kind) < 0 {
		return fmt.Errorf("when.kind must be ok/429/error, got %q", *w.Kind)
	}
	if w.WindowSeconds != nil && *w.WindowSeconds < 1 {
		return fmt.Errorf("when.window_seconds must be >= 1, got %d", *w.WindowSeconds)
	}
	for name, v := range map[string]*int{
		"when.count_429_ge":   w.Count429GE,
		"when.count_error_ge": w.CountErrorGE,
		"when.count_ok_ge":    w.CountOKGE,
		"when.count_total_ge": w.CountTotalGE,
	} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", name, *v)
		}
	}
	if w.Ratio429GE != nil {
		if w.CountTotalGE == nil {
			return fmt.Errorf("when.ratio_429_ge requires when.count_total_ge")
		}
		if *w.Ratio429GE < 0 || *w.Ratio429GE > 1 {
			return fmt.Errorf("when.ratio_429_ge must be in [0,1], got %v", *w.Ratio429GE)
		}
	}
	if w.RatioErrorGE != nil {
		if w.CountTotalGE == nil {
			return fmt.Errorf("when.ratio_error_ge requires when.count_total_ge")
		}
		if *w.RatioErrorGE < 0 || *w.RatioErrorGE > 1 {
			return fmt.Errorf("when.ratio_error_ge must be in [0,1], got %v", *w.RatioErrorGE)
		}
	}
	return nil
}

// ValidateThen then 动作校验：至少一个动作；status 合法枚举；
// cooldown 可 time.ParseDuration 解析且 > 0；weight ∈ [0,100]。
func ValidateThen(t domain.RuleThen) error {
	if t.Status == nil && t.Cooldown == nil && t.Weight == nil {
		return fmt.Errorf("then must set at least one of status/cooldown/weight")
	}
	if t.Status != nil && !validStatus(*t.Status) {
		return fmt.Errorf("then.status must be active/unhealthy/429/disabled, got %q", *t.Status)
	}
	if t.Cooldown != nil {
		d, err := time.ParseDuration(*t.Cooldown)
		if err != nil {
			return fmt.Errorf("then.cooldown: %w", err)
		}
		if d <= 0 {
			return fmt.Errorf("then.cooldown must be > 0, got %q", *t.Cooldown)
		}
	}
	if t.Weight != nil && (*t.Weight < 0 || *t.Weight > 100) {
		return fmt.Errorf("then.weight must be in [0,100], got %d", *t.Weight)
	}
	return nil
}

func validStatus(s domain.AccountStatus) bool {
	switch s {
	case domain.StatusActive, domain.StatusUnhealthy, domain.Status429, domain.StatusDisabled:
		return true
	}
	return false
}

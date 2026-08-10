package service

import (
	"context"

	"go-proxy-mini/internal/domain"
)

// —— 模板类型化扩展（template_ext 1:1；通用框架：表结构/CRUD 骨架/类型枚举
// 校验。W1 数据层 CRUD + 契约，消费接线 W3/W4/W6；codex 专属类型/列组见
// ext_codex.go，未来 claude oauth 等新类型 → 新增 ext_claude.go 同构） ——

// validateTemplateExt 校验模板 ext 行：credential_type ∈ {responses-special,
// codex-oauth, codex-pat}（api_key 主列类型无 ext 行；类型一致性——ext 行类型
// 必须 == 父模板类型——由 UpsertTemplateExt 校验）。模板是共享配置面：唯一
// 可空列 strip_image_tools（三类型公共能力开关，nil = 未配置 = 关闭）。
func validateTemplateExt(e *domain.TemplateExt) error {
	if e.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if !e.CredentialType.ValidTemplateExt() {
		return ErrInvalidInput
	}
	return nil
}

// GetTemplateExt 模板 ext 行（编辑回显）。模板缺 id → 404。
func (s *Service) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	if _, err := s.store.GetTemplate(ctx, templateID); err != nil {
		return nil, mapRepoErr(err)
	}
	e, err := s.store.GetTemplateExt(ctx, templateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return e, nil
}

// UpsertTemplateExt 幂等写入模板 ext 行（Create/Update 合一；update 全列更新
// 含 NULL 清空）。模板缺 id → 404（FK 由仓库保证）。
// 类型一致性：ext 行 credential_type 必须与父模板的 credential_type 一致
// （api_key 模板无 ext 行；special/oauth/pat 模板只能挂同类型行）——不一致 → 400。
// W1 不接线失效/发布——消费（快照加载/调度）留给 W3/W4/W6。
func (s *Service) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error) {
	tpl, err := s.store.GetTemplate(ctx, e.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != e.CredentialType {
		return nil, ErrInvalidInput // 父模板类型与 ext 行类型必须一致
	}
	if err := validateTemplateExt(e); err != nil {
		return nil, err
	}
	return s.store.UpsertTemplateExt(ctx, e)
}

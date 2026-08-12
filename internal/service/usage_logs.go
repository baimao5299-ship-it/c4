// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// usage_logs / err_logs 查询面（消费面改名裁决：log → usage 语义；错误审计
// err_logs 独立查询面——/usage_logs 与 /err_logs API）。

import (
	"context"

	"github.com/is7qin/c3api/internal/repository"
)

// QueryUsages usage_logs 计费明细分页查询（/usage_logs API；错误行含
// abort/failover 半异常标记——error_type 过滤保留）。keyset 游标分页：
// 返回行可能含 limit+1 探测行，next_cursor 组装在 handler。
func (s *Service) QueryUsages(ctx context.Context, q repository.UsageQuery) ([]any, error) {
	rows, err := s.store.QueryUsages(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return out, nil
}

// QueryErrLogs err_logs 错误明细分页查询（/err_logs API：完整错误面——拒绝 +
// 异常双轨，status_code/error_type 全值）。keyset 游标分页同 QueryUsages。
func (s *Service) QueryErrLogs(ctx context.Context, q repository.ErrLogQuery) ([]any, error) {
	rows, err := s.store.QueryErrLogs(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return out, nil
}

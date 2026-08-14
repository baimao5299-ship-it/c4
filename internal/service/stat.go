// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// QueryStats 拉取小时桶并按 granularity（hour|day）在内存聚合。
func (s *Service) QueryStats(ctx context.Context, q repository.StatQuery, granularity string) ([]*domain.StatBucket, error) {
	rows, err := s.store.ScanStats(ctx, q)
	if err != nil {
		return nil, err
	}
	if granularity == "hour" || len(rows) == 0 {
		return rows, nil
	}
	// day 聚合：按 (日, 各维度) 合并（含 UserID——/user/stats 按用户过滤，
	// 不同用户的同维度桶不可合并）
	merged := make(map[string]*domain.StatBucket)
	for _, b := range rows {
		key := b.BucketTime.Format("2006-01-02") + "|" + itoa(b.GroupID) + "|" + itoa(b.AccountID) + "|" + itoa(b.TemplateID) + "|" + itoa(b.UserID) + "|" + b.Model + "|" + boolStr(b.IsError)
		m, ok := merged[key]
		if !ok {
			day := b.BucketTime.Truncate(24 * time.Hour)
			m = &domain.StatBucket{
				BucketTime: day, GroupID: b.GroupID, AccountID: b.AccountID,
				TemplateID: b.TemplateID, UserID: b.UserID, Model: b.Model, IsError: b.IsError,
			}
			merged[key] = m
		}
		m.RequestCount += b.RequestCount
		m.ErrorCount += b.ErrorCount
		m.InputTokens += b.InputTokens
		m.OutputTokens += b.OutputTokens
		m.TotalTokens += b.TotalTokens
		m.CacheReadTokens += b.CacheReadTokens
		m.CacheCreationTokens += b.CacheCreationTokens
		m.Cost += b.Cost
		m.CallCount += b.CallCount // 按次调用（spec 2026-08-14：入桶与展示）
	}
	out := make([]*domain.StatBucket, 0, len(merged))
	for _, m := range merged {
		out = append(out, m)
	}
	return out, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

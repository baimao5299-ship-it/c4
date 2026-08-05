package service

import (
	"context"

	"go-proxy-mini/internal/repository"
)

func (s *Service) QueryLogs(ctx context.Context, q repository.LogQuery) ([]any, int64, error) {
	rows, total, err := s.store.QueryLogs(ctx, q)
	if err != nil {
		return nil, 0, err
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return out, total, nil
}

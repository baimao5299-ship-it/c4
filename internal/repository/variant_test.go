// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestVariantAllForward(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Hour)
	mk := func(g int64) *domain.StatBucket {
		return &domain.StatBucket{
			BucketTime: base, GroupID: g, AccountID: 0, TemplateID: 0, UserID: 42,
			Model: "gpt-4o", IsError: false,
			RequestCount: 1, ErrorCount: 0, InputTokens: 0, OutputTokens: 0,
			TotalTokens: 10, CacheReadTokens: 0, CacheCreationTokens: 0, Cost: 0, TotalLatencyMS: 0,
		}
	}
	const n = 500
	buckets := make([]*domain.StatBucket, 0, n)
	for g := int64(1); g <= n; g++ {
		buckets = append(buckets, mk(g))
	}
	require.NoError(t, repos.Stats.Upsert(ctx, buckets))
	// 每个 goroutine 用独立副本（避免并发排序同一数组）
	sets := make([][]*domain.StatBucket, 4)
	for i := range sets {
		sets[i] = make([]*domain.StatBucket, n)
		copy(sets[i], buckets)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = repos.Stats.Upsert(ctx, sets[i])
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Logf("goroutine %d err: %v", i, e)
		}
		require.NoError(t, e, "goroutine %d", i)
	}
}

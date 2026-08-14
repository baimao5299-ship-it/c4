// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// TTFT 直方图插值纯 Go 断言（spec 2026-08-14 测试节；公式钉死：桶内线性插值
// low + (rank − cumBelow) / bucketCount × width，rank = ceil(p × N) nearest-rank；
// 顶桶回落 12800）。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTTFTPercentileInterpolation(t *testing.T) {
	// 1000 样本固定分布：<50 ×900、[50,100) ×90、[6400,12800) ×9、顶桶 ×1
	hist := []int64{900, 90, 0, 0, 0, 0, 0, 0, 9, 1}
	n := int64(1000)
	// p50: rank 500 → 桶0 内 → 0 + 500/900×50 ≈ 27
	require.Equal(t, int64(27), ttftPercentileMS(hist, n, 0.50))
	// p90: rank 900 → 桶0 内第 900 个 → 0 + 900/900×50 = 50
	require.Equal(t, int64(50), ttftPercentileMS(hist, n, 0.90))
	// p95: rank 950 → 桶1 内第 50 个（cumBelow=900）→ 50 + 50/90×50 ≈ 77
	require.Equal(t, int64(77), ttftPercentileMS(hist, n, 0.95))
	// p99: rank 990 → 桶1 内第 90 个（cumBelow=900）→ 50 + 90/90×50 = 100（桶边界）
	require.Equal(t, int64(100), ttftPercentileMS(hist, n, 0.99))
	// 空桶跳过 + 桶8 内插值：rank 991 → 桶1 后 cum=990 → 桶8（[6400,12800)）
	// 内第 1 个 → 6400 + 1/9×6400 ≈ 7111
	require.Equal(t, int64(7111), ttftPercentileMS(hist, n, 0.991))
	// 桶8 上界（第 9/9 个）→ 6400 + 6400 = 12800，与顶桶下界重合
	require.Equal(t, int64(12800), ttftPercentileMS(hist, n, 0.999))
	// 顶桶回落：rank 1000 → 桶9（[12800,∞)）→ 下界 12800（顶桶不可插值）
	require.Equal(t, int64(12800), ttftPercentileMS(hist, n, 1.00))
	require.Equal(t, int64(12800), ttftPercentileMS(hist, n, 0.9995))
	// 无样本 → 0
	require.Zero(t, ttftPercentileMS(hist, 0, 0.50))
	require.Zero(t, ttftPercentileMS([]int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0, 0.5))
	// 域外防御：全零直方图 + N>0 → 顶桶下界（hist 丢失不 panic）
	require.Equal(t, int64(12800), ttftPercentileMS(make([]int64, 10), 5, 0.5))
	// 全量命中最后桶边界（N=1000 全在桶 9）→ 顶桶回落
	require.Equal(t, int64(12800), ttftPercentileMS([]int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 1000}, 1000, 0.5))
}

// TestMergeHist 直方图逐元素合并（加法交换序无关）。
func TestMergeHist(t *testing.T) {
	dst := make([]int64, 10)
	mergeHist(dst, []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})
	mergeHist(dst, []int64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1})
	require.Equal(t, []int64{11, 11, 11, 11, 11, 11, 11, 11, 11, 11}, dst)
	// 长度不齐防御（不入 panic）
	mergeHist(dst, []int64{1})
	require.Equal(t, int64(12), dst[0])
	require.Equal(t, int64(11), dst[9])
}

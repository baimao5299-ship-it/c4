// setup 工具轻量单测：区间解析（新参数语义核心）与模型随机挑选行为固定。
package main

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRange(t *testing.T) {
	min, max, ok := parseRange("4-16")
	require.True(t, ok)
	require.Equal(t, int64(4), min)
	require.Equal(t, int64(16), max)
	min, max, ok = parseRange("8") // 单值 = 恒定点
	require.True(t, ok)
	require.Equal(t, int64(8), min)
	require.Equal(t, int64(8), max)
	_, _, ok = parseRange("0") // 0 = 不设置
	require.False(t, ok)
	_, _, ok = parseRange("")
	require.False(t, ok)
}

func TestRandIntRange(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	v, ok := randIntRange(rng, "5-5")
	require.True(t, ok)
	require.Equal(t, int64(5), v)
	_, ok = randIntRange(rng, "0")
	require.False(t, ok)
	_, ok = randIntRange(rng, "0-0")
	require.False(t, ok)
	// 下限 0 的区间：部分实体取到 0 = 不设（"随机挑选填充"语义），部分有值
	var zero, set int
	for i := 0; i < 1000; i++ {
		v, ok := randIntRange(rng, "0-4")
		if !ok {
			zero++
		} else {
			require.InDelta(t, 0, v, 4)
			set++
		}
	}
	require.Greater(t, zero, 0)
	require.Greater(t, set, 0)
}

func TestRandUSD(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	v, ok := randUSD(rng, "2-2")
	require.True(t, ok)
	require.Equal(t, 2.0, v) // 2 位小数
	_, ok = randUSD(rng, "0")
	require.False(t, ok)
}

func TestRandomModels(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	for i := 0; i < 50; i++ {
		models := randomModels(rng)
		require.InDelta(t, 10.5, len(models), 10) // 1-20 个
		seen := map[string]bool{}
		for _, m := range models {
			seen[m] = true
		}
		require.Len(t, seen, len(models)) // 互不重复
	}
}

func TestPickModelsDistinct(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	models := pickModels(rng, 20)
	require.Len(t, models, 20)
	seen := map[string]bool{}
	for _, m := range models {
		seen[m] = true
	}
	require.Len(t, seen, 20)
}

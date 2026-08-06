# Scheduler Pre-Generated Weighted Sequence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the O(n²) weighted random selection in `internal/scheduler` with pre-generated weighted round-robin sequences built at snapshot rebuild time, making `Select` O(1) on the hot path.

**Architecture:** `groupSnapshot` gains a `routes` map keyed by `(format, model)` with a `""` model fallback bucket (unknown-model requests behave as default-format tier2). Sequences are built in `buildSnapshots` (rebuild time): GCD-normalized weight cycles, shuffled once, length-capped at 4096. `Select` looks up the route, walks the sequence with an atomic cursor, checks dynamic state (cooldown/disabled/concurrency) per candidate, CAS-acquires. `weightedOrder` is deleted.

**Tech Stack:** Go 1.26.5, sync/atomic, math/rand/v2, testify.

## Global Constraints

- `Select` hot path: map lookup + atomic cursor + per-candidate atomic state checks; no weight computation, no sorting, no allocation.
- Weighted sequence: GCD-normalized weight cycles (weight 100:50 → [a1,a1,a2]); shuffled once at build; length cap 4096 (scale down proportionally, at least 1 occurrence per account).
- Route buckets: `(format, model)` for every model in the group's model set (union of all account templates' `models ∪ model_formats keys ∪ mapping keys`); plus `(format, "")` default bucket containing accounts with `DefaultFormat == format` (all tier2) for unknown-model requests. Unknown model with no matching default bucket → ErrFormatUnavailable.
- Tier semantics preserved: tier1 sequence scanned first (one full pass), fall back to tier2 only after tier1 yields nothing.
- Dynamic state (disabled / cooldown not expired / concurrency full) checked per candidate at request time; scan limit = one full pass of the sequence (`len(seq)` iterations).
- Error sentinels unchanged: ErrGroupNotFound / ErrFormatUnavailable / ErrNoAvailable. `Selection` struct and proxy interface unchanged.
- `weightedOrder` deleted; `score()` may be deleted if unused elsewhere.
- Semantics doc: weighted random → weighted round-robin (shuffled cycle) in `docs/superpowers/specs/2026-08-05-go-proxy-mini-design.md` and scheduler package comments.
- All existing scheduler tests must pass unchanged (semantics preserved); new tests: sequence build, bucketing, distribution (±5% tolerance over 100k selections), dynamic-state skipping, tier fallback, default bucket, scan limit.
- Verify: `go test ./...`, `go vet ./...`, `golangci-lint run ./...`, `go test -race ./internal/scheduler`.

---

### Task 1: Snapshot structures and sequence building

**Files:**
- Modify: `internal/scheduler/state.go`
- Modify: `internal/scheduler/scheduler.go` (buildSnapshots wiring)
- Modify: `internal/scheduler/scheduler_test.go` (new tests)

**Interfaces:**
- Produces (consumed by Task 2):
  - `type routeKey struct { format domain.RequestFormat; model string }`
  - `type weightedSeq struct { seq []*accountSnapshot; cursor atomic.Uint64 }` with `func newWeightedSeq(pool []*accountSnapshot) *weightedSeq`
  - `type route struct { tier1, tier2 *weightedSeq }`
  - `groupSnapshot` gains `routes map[routeKey]*route`
  - `func buildRoutes(accs []*accountSnapshot) map[routeKey]*route`
  - `const maxSeqLen = 4096`
- Consumes: existing `accountSnapshot` (fields `acc`, `tpl`, `concurrency`, `errRate`, `state`), `domain.Template.FormatFor/Serves`, `domain.RequestFormat` constants.

- [ ] **Step 1: Write failing tests for sequence building**

Add to `internal/scheduler/scheduler_test.go`:

```go
func gcdTest(a, b int) int { for b != 0 { a, b = b, a%b }; return a }

func mkAcc(id int64, weight int, tpl *domain.Template) *accountSnapshot {
	a := &accountSnapshot{acc: domain.Account{ID: id, Weight: weight}, tpl: tpl}
	a.state.Store(&accState{status: domain.StatusActive})
	return a
}

func tplWith(ff domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{DefaultFormat: ff, Models: models}
}

func TestNewWeightedSeqGcdNormalization(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 50, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "weight 100:50 → GCD=50 → 序列长 3")
	count1, count2 := 0, 0
	for _, a := range ws.seq {
		if a.acc.ID == 1 { count1++ } else { count2++ }
	}
	require.Equal(t, 2, count1)
	require.Equal(t, 1, count2)
}

func TestNewWeightedSeqEqualWeights(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 100, tpl), mkAcc(3, 100, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "全同权重 → 每账号 1 次")
	require.ElementsMatch(t, []int64{1, 2, 3}, []int64{ws.seq[0].acc.ID, ws.seq[1].acc.ID, ws.seq[2].acc.ID})
}

func TestNewWeightedSeqLengthCap(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := make([]*accountSnapshot, 0, 50)
	for i := int64(1); i <= 50; i++ {
		pool = append(pool, mkAcc(i, 10000, tpl)) // GCD=10000 → 原始长 50×1=50? 不——weight 10000 全部相等 → GCD=10000 → 每账号 1 次 → 50
	}
	// 反例构造超长：权重 9999 与 1 → GCD=1 → 长 10000 > 4096
	pool2 := []*accountSnapshot{mkAcc(1, 9999, tpl), mkAcc(2, 1, tpl)}
	ws := newWeightedSeq(pool2)
	require.LessOrEqual(t, len(ws.seq), maxSeqLen, "长度上限 4096")
	require.Contains(t, []int64{ws.seq[0].acc.ID, ws.seq[1].acc.ID}, int64(1), "权重高的账号至少出现一次")
}

func TestBuildRoutesBucketsAndDefault(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o", "gpt-4o-mini"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	// 已知模型桶
	rt, ok := routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok)
	require.NotNil(t, rt.tier1, "gpt-4o 在 models 里 → tier1")
	require.Nil(t, rt.tier2)
	// 默认桶（未知模型回落）
	rtD, ok := routes[routeKey{domain.FormatOpenAIChat, ""}]
	require.True(t, ok)
	require.Nil(t, rtD.tier1)
	require.NotNil(t, rtD.tier2, "未知模型 → 默认格式 tier2")
	// 其他格式无桶
	_, ok = routes[routeKey{domain.FormatAnthropic, "gpt-4o"}]
	require.False(t, ok)
}

func TestBuildRoutesModelFormatsOverride(t *testing.T) {
	tpl := &domain.Template{DefaultFormat: domain.FormatOpenAIChat, ModelFormats: map[string]domain.RequestFormat{"special": domain.FormatAnthropic}}
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	rt, ok := routes[routeKey{domain.FormatAnthropic, "special"}]
	require.True(t, ok, "ModelFormats 覆盖 → special 模型走 anthropic 格式")
	require.NotNil(t, rt.tier1, "special ∈ ModelFormats keys → Serves true → tier1")
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./internal/scheduler -run 'Test(NewWeightedSeq|BuildRoutes)' -v`
Expected: FAIL with undefined `newWeightedSeq` / `buildRoutes` / `routeKey`.

- [ ] **Step 3: Implement structures and sequence building**

In `internal/scheduler/state.go` add:

```go
// routeKey 是预生成调度路径的桶键；model == "" 表示默认回退桶
// （请求模型不在任何模板可服务集合内时，行为等价于默认格式的 tier2）。
type routeKey struct {
	format domain.RequestFormat
	model  string
}

// weightedSeq 是按权重预生成的循环段（GCD 归一化 + 一次 shuffle），
// 请求时以原子游标取模取用。重建时生成，Select 热路径零计算。
type weightedSeq struct {
	seq    []*accountSnapshot
	cursor atomic.Uint64
}

// route 是 (format, model) 桶：模型命中（Serves）走 tier1，未命中走 tier2。
type route struct {
	tier1 *weightedSeq
	tier2 *weightedSeq
}

// maxSeqLen 是归一化序列长度上限：超出按比例缩放截断（防极端权重比）。
const maxSeqLen = 4096

func newWeightedSeq(pool []*accountSnapshot) *weightedSeq {
	g := 0
	for _, a := range pool {
		g = gcdInt(g, a.acc.Weight)
	}
	if g <= 0 {
		g = 1
	}
	var total int
	for _, a := range pool {
		total += a.acc.Weight / g
	}
	// 超长缩放：ceil 除法降权，每个账号至少保留 1 次。语义：序列长度 ≈
	// max(账号数, maxSeqLen) 量级——上限防极端权重比（9999:1 → 10000 长）的
	// 膨胀，账号数本身超过上限时不硬截（O(账号数) 可接受）。
	scale := 1
	if total > maxSeqLen {
		scale = (total + maxSeqLen - 1) / maxSeqLen // ceil
	}
	ws := &weightedSeq{}
	ws.seq = make([]*accountSnapshot, 0, total/scale+len(pool))
	for _, a := range pool {
		n := a.acc.Weight / g / scale
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			ws.seq = append(ws.seq, a)
		}
	}
	rand.Shuffle(len(ws.seq), func(i, j int) { ws.seq[i], ws.seq[j] = ws.seq[j], ws.seq[i] })
	return ws
}

func gcdInt(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
```

Add imports `math/rand/v2` and `sync/atomic` to state.go (check current imports; `sync/atomic` already present).

In `internal/scheduler/scheduler.go` add `buildRoutes` (and wire it into `buildSnapshots`):

```go
// modelSet 组内所有账号模板的可服务模型并集（桶 key 的模型空间）。
func modelSet(accs []*accountSnapshot) map[string]struct{} {
	set := make(map[string]struct{})
	for _, a := range accs {
		if a.tpl == nil {
			continue
		}
		for _, m := range a.tpl.Models {
			set[m] = struct{}{}
		}
		for m := range a.tpl.ModelFormats {
			set[m] = struct{}{}
		}
		for m := range a.tpl.ModelMapping {
			set[m] = struct{}{}
		}
	}
	return set
}

// buildRoutes 预生成 (format, model) 调度路径：格式硬过滤（FormatFor）与模型偏好
// （Serves）都是静态信息，可完全在重建时计算。另为每个格式生成默认回退桶
// （model == ""）：请求模型未知时行为等价于默认格式 + tier2（Serves 恒 false）。
func buildRoutes(accs []*accountSnapshot) map[routeKey]*route {
	routes := make(map[routeKey]*route)
	formats := []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatAnthropic}
	for model := range modelSet(accs) {
		for _, format := range formats {
			var t1, t2 []*accountSnapshot
			for _, a := range accs {
				if a.tpl == nil || a.tpl.FormatFor(model) != format {
					continue
				}
				if a.tpl.Serves(model) {
					t1 = append(t1, a)
				} else {
					t2 = append(t2, a)
				}
			}
			if len(t1) == 0 && len(t2) == 0 {
				continue
			}
			rt := &route{}
			if len(t1) > 0 {
				rt.tier1 = newWeightedSeq(t1)
			}
			if len(t2) > 0 {
				rt.tier2 = newWeightedSeq(t2)
			}
			routes[routeKey{format, model}] = rt
		}
	}
	for _, format := range formats {
		var t2 []*accountSnapshot
		for _, a := range accs {
			if a.tpl == nil || a.tpl.DefaultFormat != format {
				continue
			}
			t2 = append(t2, a)
		}
		if len(t2) == 0 {
			continue
		}
		routes[routeKey{format, ""}] = &route{tier2: newWeightedSeq(t2)}
	}
	return routes
}
```

Wire into `buildSnapshots` (scheduler.go:188-205): after building `gs.accounts`, add `gs.routes = buildRoutes(gs.accounts)`. Add `routes map[routeKey]*route` to `groupSnapshot` in state.go.

- [ ] **Step 4: Run tests and verify GREEN**

Run: `go test ./internal/scheduler -run 'Test(NewWeightedSeq|BuildRoutes)' -v`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/state.go internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go
git commit -m "feat: pre-generated weighted sequence structures (routes)"
```

---

### Task 2: Select migration to pre-generated sequences

**Files:**
- Modify: `internal/scheduler/selection.go`
- Modify: `internal/scheduler/scheduler_test.go` (new Select tests + regression)

**Interfaces:**
- Consumes: Task 1's `groupSnapshot.routes`, `routeKey`, `route`, `weightedSeq`.
- Produces: `Select` with identical signature and sentinels; `weightedOrder` removed.

- [ ] **Step 1: Write failing tests for the new Select behavior**

Add to `internal/scheduler/scheduler_test.go`:

```go
// 分布：10 万次选号，频率 vs 权重比例（±5% 容差，shuffle 后的轮询分布）
func TestSelectWeightDistribution(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k2", Status: domain.StatusActive, Weight: 50, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	const n = 100_000
	counts := map[int64]int{}
	for i := 0; i < n; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		counts[sel.AccountID]++
		s.Release(sel.AccountID)
		s.MarkResult(sel.AccountID, ResultOK, nil)
	}
	ratio := float64(counts[1]) / float64(counts[2])
	require.InRange(t, ratio, 1.9, 2.1, "weight 100:50 → 频率比 ≈ 2:1")
}

// 动态状态跳过：冷却中的账号被跳过，选中其他账号
func TestSelectSkipsCooldown(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1 进 429 冷却
	s.MarkResult(1, Result429, nil)
	for i := 0; i < 50; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(2), sel.AccountID, "冷却中的账号 1 必须被跳过")
		s.Release(sel.AccountID)
	}
}

// 全不可用（全冷却）→ ErrNoAvailable，且有限时间内返回
func TestSelectAllCooldownReturnsNoAvailable(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	s.MarkResult(1, Result429, nil)
	done := make(chan error, 1)
	go func() {
		_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNoAvailable)
	case <-time.After(time.Second):
		t.Fatal("全冷却必须有限时间内返回 ErrNoAvailable")
	}
}

// 未知模型回落默认桶：请求 model 不在任何模板可服务集合 → 默认格式 tier2 选中
func TestSelectUnknownModelDefaultBucket(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	sel, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.NoError(t, err, "未知模型走默认回退桶（默认格式 tier2）")
	require.Equal(t, int64(1), sel.AccountID)
}

// tier 回落：tier1 全冷却 → tier2 选中
func TestSelectTierFallback(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 2, Template: &domain.Template{ID: 2, DefaultFormat: domain.FormatOpenAIChat, Models: []string{"other-model"}}, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1（tier1）进冷却 → 请求 gpt-4o 应回落 tier2（账号 2，Serves 为 false）
	s.MarkResult(1, Result429, nil)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "tier1 全不可用 → tier2 回落")
	s.Release(sel.AccountID)
}
```

Note: `newTestScheduler` helper and `groupID 10` convention come from the existing test file — read the existing helpers first and reuse them; adjust `newTestScheduler` to build accounts with the given templates if the existing helper hardcodes them (extend it or construct the scheduler inline per the existing pattern).

- [ ] **Step 2: Run tests and verify RED**

Run: `go test ./internal/scheduler -run 'TestSelect(WeightDistribution|SkipsCooldown|AllCooldownReturnsNoAvailable|UnknownModelDefaultBucket|TierFallback)' -v`
Expected: RED for the new tests (distribution test will fail on O(n²) behavior or unknown-model default bucket missing; at minimum unknown-model and tier-fallback fail because routes do not exist yet).

- [ ] **Step 3: Migrate Select to sequence consumption**

Replace `internal/scheduler/selection.go` `Select` and delete `weightedOrder`:

```go
// Select 按预生成调度路径（格式硬过滤 + 模型偏好 + 加权轮询序列）选号，并占用并发槽。
// 路径在快照重建时生成（buildRoutes），本函数热路径只做 O(1) 桶查找 + 序列游标取用
// + 动态状态检查（冷却/禁用/并发满，atomic 读）+ CAS 抢占。
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	gs, ok := groups[groupID]
	if !ok {
		return nil, ErrGroupNotFound
	}
	rt, ok := gs.routes[routeKey{format, model}]
	if !ok {
		// 未知模型：回落默认桶（默认格式 tier2 语义）
		rt, ok = gs.routes[routeKey{format, ""}]
	}
	if !ok {
		return nil, ErrFormatUnavailable
	}
	now := s.timeNow()
	if rt.tier1 != nil {
		if sel, ok := s.pickFrom(rt.tier1, format, model, now); ok {
			return sel, nil
		}
	}
	if rt.tier2 != nil {
		if sel, ok := s.pickFrom(rt.tier2, format, model, now); ok {
			return sel, nil
		}
	}
	return nil, ErrNoAvailable
}

// pickFrom 沿预生成序列扫描候选：游标取模 + 动态状态检查 + CAS 抢占。
// 扫描上限 = 序列一轮（每候选检查一次）；全不可用/全竞争失败返回 false。
func (s *Scheduler) pickFrom(ws *weightedSeq, format domain.RequestFormat, model string, now time.Time) (*Selection, bool) {
	n := len(ws.seq)
	if n == 0 {
		return nil, false
	}
	for i := 0; i < n; i++ {
		a := ws.seq[int(ws.cursor.Add(1))%n]
		st := a.statePtr()
		if st.status == domain.StatusDisabled {
			continue
		}
		if st.cooldownUntil != nil && !st.cooldownUntil.Before(now) {
			continue
		}
		cur := a.concurrency.Load()
		if cur >= int64(a.acc.MaxConcurrency) {
			continue
		}
		if a.concurrency.CompareAndSwap(cur, cur+1) {
			mapped := model
			if m, ok := a.tpl.ModelMapping[model]; ok {
				mapped = m
			}
			used := s.timeNow()
			st2 := *st
			st2.lastUsedAt = &used
			a.state.Store(&st2)
			return &Selection{
				AccountID: a.acc.ID, TemplateID: a.tpl.ID,
				BaseURL: a.tpl.BaseURL, Format: format,
				UpstreamKey: a.acc.UpstreamKey, Model: mapped,
			}, true
		}
	}
	return nil, false
}
```

Delete `weightedOrder` and the `math/rand/v2` import from selection.go if now unused. `score()` in state.go: check usage — if only `weightedOrder` used it, delete it and its `errRateScale` comment if unused (keep `errRateScale` if `errRate` still uses it — it does, in `score()` and `MarkResult`; if `score()` is deleted, `errRateScale` is still used by `MarkResult` and `Runtime`).

- [ ] **Step 4: Run tests and verify GREEN**

Run: `go test ./internal/scheduler -v`
Expected: all existing + new tests pass.

- [ ] **Step 5: Run repository verification**

Run: `go test ./... && go vet ./... && golangci-lint run ./...`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/scheduler/selection.go internal/scheduler/scheduler_test.go
git commit -m "feat: Select consumes pre-generated weighted sequences (O(1))"
```

---

### Task 3: Semantics docs sync and benchmark

**Files:**
- Modify: `docs/superpowers/specs/2026-08-05-go-proxy-mini-design.md` (weighted random → weighted round-robin)
- Create: `internal/scheduler/bench_test.go`

**Interfaces:**
- Consumes: Task 2's Select.
- Produces: benchmark evidence; doc sync.

- [ ] **Step 1: Sync design doc semantics**

In `docs/superpowers/specs/2026-08-05-go-proxy-mini-design.md`, find the selection description (search "加权随机" / "weight × (1−errRate)" / "weightedOrder") and update to: 选号 = 预生成加权轮询序列（快照重建时按 weight 归一化生成循环段并 shuffle，请求时原子游标取用，O(1)）；动态状态（冷却/禁用/并发满）请求时检查跳过；tier1 优先、全不可用回落 tier2；未知模型回落默认格式桶。Also update the `internal/scheduler/selection.go` package comment if it mentions weighted random.

- [ ] **Step 2: Write Select benchmark**

Create `internal/scheduler/bench_test.go`:

```go
package scheduler

import (
	"testing"

	"go-proxy-mini/internal/domain"
)

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
	tpl := &domain.Template{ID: 1, DefaultFormat: domain.FormatOpenAIChat, Models: []string{"gpt-4o"}}
	accs := make(map[int64][]*domain.Account)
	for i := int64(1); i <= int64(n); i++ {
		accs[10] = append(accs[10], &domain.Account{
			ID: i, TemplateID: 1, Template: tpl, UpstreamKey: "k",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 100000,
		})
	}
	s := New(Config{DefaultMaxConcurrency: 100000, SyncInterval: time.Hour}, noopLoader{accs: accs}, nil)
	if err := s.InvalidateAllSync(); err != nil {
		b.Fatal(err)
	}
	return s
}
```

Check the existing test loader type name in scheduler_test.go (`noopLoader` may differ — reuse the existing test helper pattern; if `schedulerWithAccounts` needs the loader type from the test file, use the existing helper or define inline). Add `time` import if needed.

- [ ] **Step 3: Run benchmark**

Run: `go test ./internal/scheduler -bench BenchmarkSelect5000Accounts -benchmem -run=^$`
Expected: record ns/op and allocs/op (expect sub-µs, 0 allocs).

- [ ] **Step 4: Verify repository and commit**

Run: `go test ./... && go vet ./... && golangci-lint run ./...`
Expected: all green.

```bash
git add docs/superpowers/specs/2026-08-05-go-proxy-mini-design.md internal/scheduler/bench_test.go
git commit -m "docs: weighted round-robin semantics; test: Select 5k-account benchmark"
```

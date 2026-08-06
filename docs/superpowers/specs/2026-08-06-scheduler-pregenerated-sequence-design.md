# 调度器预生成加权序列设计

## 状态

已批准（用户：调度器预先算好调度路径）。替代加权随机不放回抽样（O(n²)），Select 热路径降为 O(1)。

## 背景与目标

50k 并发 × 5,000 账号压测实锤：`weightedOrder`（不放回加权抽样，O(n²)——每轮遍历剩余池重算 total + 逐元素 `score()` + 切片删除）占网关 CPU 51%+，5,000 账号下每 Select ~1,250 万次浮点计算，CPU 打满导致连接停滞（完成率 61 req/s，avg 首字节 162s）。单组 6 账号时是 O(36) 微操作从未暴露。

目标：选号计算从请求时动态计算改为**快照重建时预生成加权序列**，Select 变为 O(1) 取用。worker 结构不变（快照重建本就在 syncLoop/InvalidateGroup 的后台 goroutine 中，序列生成挂在其上，无需新增 worker）。

## 方案

### 数据落点：groupSnapshot 扩展

```go
type groupSnapshot struct {
    accounts []*accountSnapshot       // 保留（byID 重建与 Runtime 用）
    routes   map[routeKey]*route      // 新增：预生成路径
}

type routeKey struct {
    format domain.RequestFormat
    model  string
}

type route struct {
    tier1 *weightedSeq // 模型命中（Serves）序列
    tier2 *weightedSeq // 模型未命中序列
}

type weightedSeq struct {
    seq    []*accountSnapshot // 按权重归一化的循环段
    cursor atomic.Uint64      // 游标（取模循环）
}
```

- `routes` 在 `buildSnapshots` 中构建（重建时，低频）；
- 桶粒度 `(format, model)`：格式硬过滤与模型偏好（tier）都是静态信息（`FormatFor`/`Serves` 不随请求变化），可完全预计算；
- 模型集合 = 组内所有账号模板的 `models ∪ model_formats keys ∪ mapping keys` 并集；
- 序列存**指针引用**（非复制账号对象），组内共享。

### 序列生成（重建时，buildSnapshots）

对每个 `(format, model)`：

1. 遍历组内账号：`tpl.FormatFor(model) == format` 硬过滤 → 通过者按 `tpl.Serves(model)` 分 tier1/tier2；
2. 各 tier 生成加权序列：权重归一化（除以 GCD）得到循环段，如 weight 100:50 → GCD=50 → [a1,a1,a2]（长 3）；全同权重 → 序列 = 账号列表（长 n）；
3. 序列长度上限：归一化后长度 > 4096 时按比例缩放（整数除法取 floor，至少保留 1 次出现）并截断——防极端权重比（999:1）产生超长序列；
4. 生成时对循环段做一次 `math/rand/v2.Shuffle`（保留加权随机分布，避免固定顺序突刺）。

### Select 热路径（O(1)）

```go
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
    gs := groups[groupID]                 // 无锁读
    rt, ok := gs.routes[routeKey{format, model}]
    if !ok { return nil, ErrFormatUnavailable }
    pool := rt.tier1.seq
    if rt.tier1 全部不可用（扫描上限内未命中） { pool = rt.tier2.seq }
    if pool 为空 { return nil, ErrNoAvailable / ErrFormatUnavailable }
    for i := 0; i < scanLimit; i++ {      // scanLimit = max(len(pool), 64)
        a := pool[rt.cursor.Add(1) % len(pool)]
        // 动态检查（atomic 读，O(1)）：disabled / 冷却未过期 / 并发满
        if 不可用 { continue }
        if CAS 抢占成功 {
            return Selection{...}
        }
    }
    return nil, ErrNoAvailable
}
```

- 动态状态（冷却/disabled/并发满）仍在请求时检查——状态每请求变化，不可预计算；
- tier 语义保留：tier1 序列扫描上限内无可用才回落 tier2；
- `found` 语义（区分 ErrFormatUnavailable / ErrNoAvailable）由 route 存在性 + 扫描结果表达；
- 模型映射（ModelMapping → Selection.Model）保留在 CAS 成功后（与现一致）。

### 游标并发

单 `atomic.Uint64` 游标（50k 并发下 Add 竞争可承受——现有 concurrency/errRate 同为 atomic 热路径）。设计记录分片游标为可选优化（per-P sharding），首版不实施。

### 语义变化与兼容

- **加权随机（每请求独立）→ 加权轮询（序列循环）**：长期分布更均匀（无随机突刺），短期分布等价（序列已 shuffle）。规格 §10 与调度器注释同步更新。
- **errRate 权重滞后**：现有 `buildSnapshots` 重建时新快照 errRate 归零（重建清零 EWMA 是现有行为）→ 序列权重 = 纯 weight，两次重建间不随 errRate 变化——**与现状一致，无新语义**。
- 错误哨兵（ErrGroupNotFound/ErrFormatUnavailable/ErrNoAvailable）、Selection 结构、proxy 接口**零变化**。

## 边界

- 扫描上限：`max(len(pool), 64)`——全不可用/极端竞争时防无限循环；
- 序列长度上限：归一化后 >4096 按比例缩放截断；
- route 缺失（组内无账号支持该 format/model）：ErrFormatUnavailable。

## 测试设计

### 单元（scheduler_test.go 扩展）

1. 序列生成：权重比例（2:1 账号序列出现比）、GCD 归一化、全同权重序列长=n、长度上限缩放；
2. 分桶：format/model 组合、tier1/tier2 划分、无账号支持的组合 route 缺失；
3. Select 分布：10 万次选号频率 vs 权重比例（±5% 容差，序列已 shuffle 的轮询分布）；
4. Select 动态状态：冷却/disabled/并发满跳过（序列内继续扫描）；
5. 扫描上限：全不可用 → ErrNoAvailable（有限时间返回）；
6. tier 回落：tier1 全冷却 → tier2 选中；
7. 回归：现有 13 测试全绿（格式硬过滤/模型偏好/并发限制/冷却退避/InvalidateGroup/disabled 守卫/Worker 契约）。

### 性能验证

- `go test -bench`：Select 在 5k 账号快照下单次耗时（O(1) vs 改造前 O(n²) 对照，改造前数值取自压测 pprof 估算）；
- 服务器复测：50k 并发 × 5,000 账号（同拓扑），pprof 确认 Select/weightedOrder 热点消失，完成率与首字节恢复正常量级；
- 结果匿名记录，不写服务器身份。

## 文件改动

- Modify: `internal/scheduler/state.go`（groupSnapshot/route/weightedSeq 结构）
- Modify: `internal/scheduler/scheduler.go`（buildSnapshots 生成序列）
- Modify: `internal/scheduler/selection.go`（Select 改为序列取用；weightedOrder 删除）
- Modify: `internal/scheduler/scheduler_test.go`（新测试 + 回归）
- Modify: `docs/superpowers/specs/2026-08-05-go-proxy-mini-design.md`（加权随机 → 加权轮询语义同步）

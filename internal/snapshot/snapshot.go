// Package snapshot 统一网关各内存快照的生命周期：启动就绪（ReloadAll 全量首刷）
// + NOTIFY 事件分发（按 scope 精确重载）+ 状态可观测（Status）。
//
// 边界（用户拍板 2026-08-11）：
//   - 注册表只做事件驱动（启动 + NOTIFY scope 分发）与状态追踪，不接管各模块
//     周期 ticker（scheduler syncLoop / balances ticker / auth-sync / price cron
//     均留模块自管——避免双 reload 竞争）；
//   - 不缓存/存储快照数据：数据仍在各模块（Auth RWMutex / scheduler store /
//     Balances atomic.Pointer / rulesMu / service pricing snapshot），注册表仅
//     持名称/scope/状态元数据；
//   - 不进入请求热路径：注册表所有锁只在启动/NOTIFY/管理面低频路径，快照读
//     取照旧模块内原子读，Reload 成本与各模块既有 reload 相同（零新增查询）。
//
// scope 语义（脏标记）：Snapshot 注册时声明关心的变更 scope；NOTIFY 变更按
// 类型映射 scope 后 Reload(ctx, scopes...) 只重载命中 scope 的快照（未命中
// 不动——变更标记对应快照集合，触发时只重载这些）。当前接线：settings 变更
// → ScopeSettings（auth gate N 预算即时重算，#36 缺口）。
package snapshot

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Scope 变更 scope 标识（脏标记键）。scope 字符串由装配侧按 NOTIFY 变更类型
// 映射；注册表只按字符串分发，不感知具体类型（保持最小接口）。
type Scope = string

// ScopeSettings settings 快照变更 scope：NOTIFY Change.Settings → 声明本 scope
// 的快照精确重载（当前接线 = auth：gate 预算按新 N 即时重算，补 #36 "NOTIFY
// 不触发 Auth.Reload" 缺口）。settings 自身快照（svc.ReloadSettings）不走注册
// 表，保持既有 ReloadSettings 行为。
const ScopeSettings Scope = "settings"

// Snapshot 最小快照契约（各模块已有 Reload 的包装，不侵入模块内部结构）：
// Name 唯一标识；Scopes 声明的变更 scope（nil = 纯启动/状态快照，不响应任何
// scope 分发）；Reload 全量刷新（须响应 ctx 取消；错误原样返回由注册表收集）。
type Snapshot interface {
	Name() string
	Scopes() []string
	Reload(ctx context.Context) error
}

// Status 单个快照的可观测状态（Status() 快照拷贝，供日志/管理面展示）。
type Status struct {
	Name       string
	Scopes     []string
	LastReload time.Time // 零值 = 尚未触发过
	LastError  error     // 最近一次 reload 错误（nil = 成功）
}

// Registry 快照注册表：名称 → 快照 + scope → 快照名 的元数据索引（O(1) scope
// 分发查找——NOTIFY 低频路径）。触发（ReloadAll/Reload）内部串行执行（execMu，
// 事件不重叠；快照内部并发安全是模块既有保证，此处是注册表层面的事件串行），
// 单次触发内各快照并行 reload + 错误独立收集。注册与触发并发安全。
type Registry struct {
	mu      sync.RWMutex        // 保护元数据（order/byName/byScope/status）
	order   []string            // 注册顺序（Status/错误收集确定性输出）
	byName  map[string]Snapshot // 名称 → 快照
	byScope map[string][]string // scope → 快照名（注册顺序；去重）
	status  map[string]*Status  // 名称 → 状态
	// execMu 触发执行互斥：ReloadAll/Reload 串行。非重入——快照 Reload 内再触
	// 注册表（ReloadAll/Reload）即死锁（sync.Mutex 不可重入），快照必须自持
	// 状态，不得在 Reload 中回调注册表触发。
	execMu sync.Mutex
}

// New 构造空注册表。
func New() *Registry {
	return &Registry{
		byName:  make(map[string]Snapshot),
		byScope: make(map[string][]string),
		status:  make(map[string]*Status),
	}
}

// Register 注册快照（重复名 → 错误；名称为空 → 错误）。注册与触发并发安全：
// 正在执行的触发已快照集合不受影响（本次注册的命中判定从下次触发起生效）。
func (r *Registry) Register(s Snapshot) error {
	if s == nil {
		return fmt.Errorf("snapshot: nil registration")
	}
	name := s.Name()
	if name == "" {
		return fmt.Errorf("snapshot: empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("snapshot: duplicate name %q", name)
	}
	scopes := slices.Clone(s.Scopes())
	r.byName[name] = s
	r.order = append(r.order, name)
	// scope 去重（byScope 注释承诺"去重"）：同一快照声明重复 scope 只入索引
	// 一次。Status.Scopes 同源去重（Status 展示即索引语义）。
	uniq := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		if sc == "" {
			continue
		}
		if _, dup := seen[sc]; dup {
			continue
		}
		seen[sc] = struct{}{}
		uniq = append(uniq, sc)
	}
	for _, sc := range uniq {
		r.byScope[sc] = append(r.byScope[sc], name)
	}
	r.status[name] = &Status{Name: name, Scopes: uniq}
	return nil
}

// ReloadAll 全量并行 reload 所有已注册快照（启动就绪专用）：各快照错误独立
// 收集返回（map：快照名 → 错误，成功者不出现）；返回空 = 全部成功。单次触发
// 内并行执行，不同触发之间串行（execMu）。
func (r *Registry) ReloadAll(ctx context.Context) map[string]error {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	r.mu.RLock()
	names := slices.Clone(r.order)
	r.mu.RUnlock()
	return r.run(ctx, names)
}

// Reload 按 scope 精确重载（NOTIFY 分发）：只 reload 声明了任一给定 scope 的
// 快照（同快照多 scope 命中只执行一次）；空 scopes / 未命中 → no-op 返回空。
// 错误独立收集返回（同 ReloadAll）。O(命中 scope 的快照数) 查找。
func (r *Registry) Reload(ctx context.Context, scopes ...string) map[string]error {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	r.mu.RLock()
	names := make([]string, 0, len(scopes))
	seen := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		for _, n := range r.byScope[sc] {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			names = append(names, n)
		}
	}
	r.mu.RUnlock()
	return r.run(ctx, names)
}

// run 并行执行 names 对应快照的 Reload，收集错误并记录状态。
func (r *Registry) run(ctx context.Context, names []string) map[string]error {
	if len(names) == 0 {
		return nil
	}
	r.mu.RLock()
	snaps := make([]Snapshot, 0, len(names))
	for _, n := range names {
		if s, ok := r.byName[n]; ok {
			snaps = append(snaps, s)
		}
	}
	r.mu.RUnlock()
	errs := make(map[string]error)
	var (
		wg    sync.WaitGroup
		errMu sync.Mutex
	)
	for _, s := range snaps {
		wg.Add(1)
		go func(s Snapshot) {
			defer wg.Done()
			err := s.Reload(ctx)
			r.record(s.Name(), err)
			if err != nil {
				errMu.Lock()
				errs[s.Name()] = err
				errMu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	return errs
}

// record 记录一次 reload 的结果（成功也更新 LastReload；LastError 置 nil 清
// 上次失败——Status 反映最近一次结果）。
func (r *Registry) record(name string, err error) {
	r.mu.Lock()
	if st, ok := r.status[name]; ok {
		st.LastReload = time.Now()
		st.LastError = err
	}
	r.mu.Unlock()
}

// Status 全部快照状态快照（注册顺序；值拷贝，调用方安全持有——Scopes 切片
// 深拷贝（slices.Clone），防调用方改写共享底层数组污染注册表状态）。
func (r *Registry) Status() []Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Status, 0, len(r.order))
	for _, n := range r.order {
		if st, ok := r.status[n]; ok {
			cp := *st
			cp.Scopes = slices.Clone(st.Scopes)
			out = append(out, cp)
		}
	}
	return out
}

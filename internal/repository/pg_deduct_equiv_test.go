package repository_test

// 热点修复 A 扩：DeductAndLog 双路径等价性测试（约束 ① 语义等价的兜底——
// deductCore 单一实现防结构漂移，本测试防行为漂移）。同一输入分别走 ent
// CreateBulk 路径（pool == nil）与 pgx COPY 路径（pool）→ 全列逐字段断言
// usage_logs 行 + 余额/临时额度终态 + 分区路由（跨日 created_at 逐行落区）。

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/ent/tempbalance"
	"go-proxy-mini/internal/repository"
)

// deductState 单次 DeductAndLog 后的可观测终态（双路径对比面）。
type deductState struct {
	balance int64
	od      bool
	// temp 该用户剩余临时额度金额（升序；FEFO 扣减顺序对比）。
	temp []int64
	// logs 该用户 usage_logs 全列（request_id 升序；UserID/ID 归一后对比——
	// 两路径各用独立用户，避免余额互扰）。
	logs []*domain.UsageLog
}

// TestPGDeductCopyPathEquivalent 双路径等价：5 个场景矩阵（条件扣成功 + FEFO
// 临时额度 / 透支 / cost=0 只插日志 / 用户缺失仍插日志 / 大批 4000 行跨分片 +
// 跨日分区路由）逐场景对比终态。
func TestPGDeductCopyPathEquivalent(t *testing.T) {
	reposCopy := newPGRepos(t)      // pool → pgx COPY 路径
	reposEnt := newPGReposNoPool(t) // nil pool → ent CreateBulk 路径（同 schema）
	ctx := context.Background()

	scenarios := []struct {
		name string
		cost int64
		// ghost = 不创建用户（用户缺失仍插日志场景）。
		ghost bool
		seed  func(t *testing.T, repos *repository.Repository, uid int64)
		// logs 用共享基准时间构造（两路径同输入确定性——now 为同一次截断值）。
		logs func(uid int64, now time.Time) []*domain.UsageLog
	}{
		{
			name: "conditional success with FEFO temp balances",
			cost: 400_000,
			seed: func(t *testing.T, repos *repository.Repository, uid int64) {
				require.NoError(t, repos.UpdateUserBalance(ctx, uid, 1_000_000))
				exp1 := time.Now().Add(time.Hour).Truncate(time.Second)
				seedTempBalance(t, repos, uid, 150_000, &exp1)
				seedTempBalance(t, repos, uid, 300_000, nil)
			},
			logs: func(uid int64, now time.Time) []*domain.UsageLog {
				ls := []*domain.UsageLog{
					fullLogFor(uid, "eq-fefo-1"),
					fullLogFor(uid, "eq-fefo-2"),
				}
				for _, l := range ls {
					l.CreatedAt = now // 基准时间统一（fullLogFor 内部 time.Now 覆盖）
				}
				return ls
			},
		},
		{
			name: "overdraft",
			cost: 400_000,
			seed: func(t *testing.T, repos *repository.Repository, uid int64) {
				require.NoError(t, repos.UpdateUserBalance(ctx, uid, 10_000))
			},
			logs: func(uid int64, now time.Time) []*domain.UsageLog {
				l := fullLogFor(uid, "eq-od-1")
				l.CreatedAt = now
				return []*domain.UsageLog{l}
			},
		},
		{
			name: "zero cost only logs",
			cost: 0,
			seed: func(t *testing.T, repos *repository.Repository, uid int64) {
				require.NoError(t, repos.UpdateUserBalance(ctx, uid, 50_000))
				seedTempBalance(t, repos, uid, 30_000, nil)
			},
			logs: func(uid int64, now time.Time) []*domain.UsageLog {
				ls := []*domain.UsageLog{
					fullLogFor(uid, "eq-z-1"),
					fullLogFor(uid, "eq-z-2"),
				}
				for _, l := range ls {
					l.CreatedAt = now
				}
				return ls
			},
		},
		{
			name:  "user missing still logs",
			cost:  400_000,
			ghost: true,
			logs: func(uid int64, now time.Time) []*domain.UsageLog {
				l := fullLogFor(uid, "eq-ghost-1")
				l.CreatedAt = now
				return []*domain.UsageLog{l}
			},
		},
		{
			name: "large batch 4000 rows cross chunks",
			cost: 520_000,
			seed: func(t *testing.T, repos *repository.Repository, uid int64) {
				require.NoError(t, repos.UpdateUserBalance(ctx, uid, 1_000_000))
			},
			logs: func(uid int64, now time.Time) []*domain.UsageLog {
				logs := make([]*domain.UsageLog, 0, 4000)
				for i := 0; i < 4000; i++ {
					l := fullLogFor(uid, fmt.Sprintf("eq-big-%d", i))
					l.CreatedAt = now // 全行统一基准（fullLogFor 内部 time.Now 覆盖）
					// 跨日分区路由：末 3 行 created_at = 明日（今日分区之外）。
					if i >= 3997 {
						l.CreatedAt = now.Add(24 * time.Hour)
					}
					logs = append(logs, l)
				}
				return logs
			},
		},
	}

	// 输入确定性：基准时间测试级一次性计算，两路径同一次截断值。
	base := time.Now().Truncate(time.Second)
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			stCopy, uidCopy := runDeductScenario(t, sc.name, "copy", reposCopy, base, sc.cost, sc.ghost, sc.seed, sc.logs)
			stEnt, uidEnt := runDeductScenario(t, sc.name, "ent", reposEnt, base, sc.cost, sc.ghost, sc.seed, sc.logs)
			require.Equal(t, stEnt, stCopy, "ent 路径与 COPY 路径终态必须逐字段一致")
			if sc.name == "large batch 4000 rows cross chunks" {
				// 分区路由逐路径断言：明日分区各 3 行、今日分区各 3997 行。
				pool := pgTestPool(t)
				tomorrow := time.Now().Add(24 * time.Hour).UTC().Format("20060102")
				require.Equal(t, int64(3), pgCount(t, pool,
					"SELECT count(*) FROM usage_logs_"+tomorrow+" WHERE user_id = $1", uidCopy),
					"COPY 路径跨日行落入明日分区")
				require.Equal(t, int64(3), pgCount(t, pool,
					"SELECT count(*) FROM usage_logs_"+tomorrow+" WHERE user_id = $1", uidEnt),
					"ent 路径跨日行落入明日分区")
			}
		})
	}
}

// runDeductScenario 在指定路径上执行一个场景并采集终态（ghost = 不建用户，用
// 固定不存在 uid；email 带路径 tag 防撞 users_email_key）。返回 (终态, userID)。
func runDeductScenario(t *testing.T, name, tag string, repos *repository.Repository, base time.Time, cost int64, ghost bool,
	seed func(t *testing.T, repos *repository.Repository, uid int64),
	logs func(uid int64, now time.Time) []*domain.UsageLog,
) (deductState, int64) {
	t.Helper()
	ctx := context.Background()
	uid := int64(999999)
	if ghost {
		// 两路径 ghost 用户不同（同一 schema 内各自插日志互不干扰）。
		uid = int64(900000 + len(tag))
	} else {
		u := seedPGUser(t, repos, fmt.Sprintf("equiv-%s-%s@example.com", name, tag))
		uid = u.ID
		seed(t, repos, uid)
	}
	in := logs(uid, base)

	od, bal, err := repos.DeductAndLog(ctx, uid, cost, in)
	require.NoError(t, err)

	st := deductState{balance: bal, od: od}
	rows, err := repos.Client.TempBalance.Query().
		Where(tempbalance.UserIDEQ(uid)).All(ctx)
	require.NoError(t, err)
	for _, r := range rows {
		st.temp = append(st.temp, r.Amount)
	}
	sort.Slice(st.temp, func(i, j int) bool { return st.temp[i] < st.temp[j] })
	out, _, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{UserID: uid, Limit: 10000})
	require.NoError(t, err)
	sort.Slice(out, func(i, j int) bool { return out[i].RequestID < out[j].RequestID })
	for _, l := range out {
		l.UserID = 0 // 两路径用户不同，非语义字段
		l.ID = 0     // 序列推进随路径不同，非语义字段
		st.logs = append(st.logs, l)
	}
	return st, uid
}

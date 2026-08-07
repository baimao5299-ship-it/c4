package handler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
	"go-proxy-mini/internal/service"
)

// fakeStore 以值语义模拟真实仓库（ent 每次返回新对象、无指针别名）：
// Create/Get/Update 均返回副本，存库条目一经写入不再被外部指针修改。
// 若直接存/返回调用方指针，原地修改会透过别名污染测试持有的旧引用
// （评审发现：测试必然失败或退化为恒真断言）。
// 实现 service.Store 接口，供 handler 测试使用。
type fakeStore struct {
	mu        sync.Mutex
	tpls      map[int64]*domain.Template
	accs      map[int64]*domain.Account
	groups    map[int64]*domain.Group
	accGroups map[int64][]int64 // accountID → groupIDs（账号侧绑定，Set/GetAccountGroups）
	keys      map[int64]*domain.Key
	users     map[int64]*domain.User
	settings  map[string]*domain.Setting
	rules     map[int64]domain.Rule
	logs      []*domain.UsageLog
	stats     []*domain.StatBucket
	nextID    int64
	// lastPatch 记录最近一次 UpdateAccountsBatch 收到的 patch（评审 M3：
	// 断言 handler 的 group_ids nil/[] 映射是否真正传到了 repo 层）。
	lastPatch repository.AccountPatch
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tpls: make(map[int64]*domain.Template), accs: make(map[int64]*domain.Account),
		groups: make(map[int64]*domain.Group), accGroups: make(map[int64][]int64),
		keys: make(map[int64]*domain.Key), users: make(map[int64]*domain.User),
		settings: make(map[string]*domain.Setting), rules: make(map[int64]domain.Rule), nextID: 1,
	}
}

// DeleteKeysByGroup 满足 KeyStore（组删除前置清理；返回被删 hash 列表）。
func (f *fakeStore) DeleteKeysByGroup(ctx context.Context, groupID int64) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var hashes []string
	for id, k := range f.keys {
		if k.GroupID == groupID {
			hashes = append(hashes, k.KeyHash)
			delete(f.keys, id)
		}
	}
	return hashes, nil
}

var _ service.Store = (*fakeStore)(nil)

// missingErr 模拟真实 repo 单资源缺 id 错误（与批量 fake 同格式：
// repository.ErrNotFound 包装，service mapRepoErr 据此映射 404 含 id）。
func missingErr(id int64) error {
	return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
}

func (f *fakeStore) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t.ID = f.nextID
	f.nextID++
	c := *t
	f.tpls[t.ID] = &c
	return t, nil
}

func (f *fakeStore) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tpls[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *t
	return &c, nil
}

func (f *fakeStore) ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Template, 0, len(f.tpls))
	for _, t := range f.tpls {
		c := *t
		out = append(out, &c)
	}
	return out, int64(len(f.tpls)), nil
}

func (f *fakeStore) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *t
	f.tpls[t.ID] = &c
	return &c, nil
}

func (f *fakeStore) DeleteTemplate(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.tpls[id]; !ok {
		return missingErr(id)
	}
	delete(f.tpls, id)
	return nil
}

func (f *fakeStore) CreateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.ID = f.nextID
	f.nextID++
	c := *a
	f.accs[a.ID] = &c
	return a, nil
}

func (f *fakeStore) GetAccount(ctx context.Context, id int64) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accs[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *a
	return &c, nil
}

func (f *fakeStore) ListAccounts(ctx context.Context, q repository.ListQuery) ([]*domain.Account, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Account, 0, len(f.accs))
	for _, a := range f.accs {
		c := *a
		out = append(out, &c)
	}
	return out, int64(len(f.accs)), nil
}

func (f *fakeStore) UpdateAccount(ctx context.Context, a *domain.Account) (*domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *a
	f.accs[a.ID] = &c
	return &c, nil
}

func (f *fakeStore) DeleteAccount(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accs[id]; !ok {
		return missingErr(id)
	}
	delete(f.accs, id)
	return nil
}

func (f *fakeStore) CreateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g.ID = f.nextID
	f.nextID++
	c := *g
	f.groups[g.ID] = &c
	return g, nil
}

func (f *fakeStore) GetGroup(ctx context.Context, id int64) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.groups[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *g
	return &c, nil
}

func (f *fakeStore) ListGroups(ctx context.Context, q repository.ListQuery) ([]*domain.Group, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*domain.Group, 0, len(f.groups))
	for _, g := range f.groups {
		c := *g
		out = append(out, &c)
	}
	return out, int64(len(f.groups)), nil
}

func (f *fakeStore) UpdateGroup(ctx context.Context, g *domain.Group) (*domain.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := *g
	f.groups[g.ID] = &c
	return &c, nil
}

func (f *fakeStore) DeleteGroup(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.groups[id]; !ok {
		return missingErr(id)
	}
	delete(f.groups, id)
	return nil
}

func (f *fakeStore) SetAccountGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accGroups[accountID] = slices.Clone(groupIDs)
	return nil
}

func (f *fakeStore) GetAccountGroups(ctx context.Context, accountID int64) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accGroups[accountID]), nil
}

func (f *fakeStore) QueryLogs(ctx context.Context, q repository.LogQuery) ([]*domain.UsageLog, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs, int64(len(f.logs)), nil
}

// --- 批量操作（模拟 repo 语义：事务内存在性检查，缺失 id 返回
// repository.ErrNotFound 包装错误含 id；更新仅应用 patch 非 nil 字段） ---

// missingID 返回 ids 中第一个不存在条目的 id（与真实 repo 的
// checkXxxExist 差集语义一致）。
func (f *fakeStore) missingID(ids []int64, exists func(id int64) bool) (int64, bool) {
	for _, id := range ids {
		if !exists(id) {
			return id, true
		}
	}
	return 0, false
}

func (f *fakeStore) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.tpls[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.tpls, id)
	}
	return nil
}

func (f *fakeStore) UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.tpls[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		t := f.tpls[id]
		if p.Name != nil {
			t.Name = *p.Name
		}
		if p.BaseURL != nil {
			t.BaseURL = *p.BaseURL
		}
		if p.SupportedFormats != nil {
			t.SupportedFormats = *p.SupportedFormats
		}
		if p.Models != nil {
			t.Models = *p.Models
		}
		if p.FormatModels != nil {
			t.FormatModels = *p.FormatModels
		}
		if p.ModelMapping != nil {
			m := make(map[string]string, len(*p.ModelMapping))
			for k, v := range *p.ModelMapping {
				m[k] = v
			}
			t.ModelMapping = m
		}
	}
	return nil
}

func (f *fakeStore) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.accs[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.accs, id)
	}
	return nil
}

func (f *fakeStore) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.accs[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	// 组存在性（与真实 repo 的 checkGroupExist 同级语义：非空 group_ids 全查）
	if p.GroupIDs != nil {
		for _, gid := range *p.GroupIDs {
			if _, ok := f.groups[gid]; !ok {
				return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, gid)
			}
		}
	}
	f.lastPatch = p
	for _, id := range ids {
		a := f.accs[id]
		if p.Name != nil {
			a.Name = *p.Name
		}
		if p.TemplateID != nil {
			a.TemplateID = *p.TemplateID
		}
		if p.UpstreamKey != nil {
			a.UpstreamKey = *p.UpstreamKey
		}
		if p.Status != nil {
			a.Status = *p.Status
		}
		if p.Weight != nil {
			a.Weight = *p.Weight
		}
		if p.MaxConcurrency != nil {
			a.MaxConcurrency = *p.MaxConcurrency
		}
		if p.GroupIDs != nil {
			f.accGroups[id] = slices.Clone(*p.GroupIDs)
		}
	}
	return nil
}

func (f *fakeStore) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.groups[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.groups, id)
	}
	return nil
}

func (f *fakeStore) UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.groups[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		g := f.groups[id]
		if p.Name != nil {
			g.Name = *p.Name
		}
	}
	return nil
}

func (f *fakeStore) ScanStats(ctx context.Context, q repository.StatQuery) ([]*domain.StatBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}

// --- 规则（RuleStore）：priority/name 唯一冲突模拟真实 repo 的 ErrConflict ---

func (f *fakeStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b domain.Rule) int { return a.Priority - b.Priority })
	return out, nil
}

func (f *fakeStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.ruleConflictLocked(0, r); err != nil {
		return 0, err
	}
	r.ID = f.nextID
	f.nextID++
	f.rules[r.ID] = r
	return r.ID, nil
}

func (f *fakeStore) UpdateRule(ctx context.Context, r domain.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[r.ID]; !ok {
		return missingErr(r.ID)
	}
	if err := f.ruleConflictLocked(r.ID, r); err != nil {
		return err
	}
	f.rules[r.ID] = r
	return nil
}

func (f *fakeStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return missingErr(id)
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.missingID(ids, func(id int64) bool { _, ok := f.rules[id]; return ok }); ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	for _, id := range ids {
		delete(f.rules, id)
	}
	return nil
}

func (f *fakeStore) CountRules(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rules)), nil
}

// ruleConflictLocked 检查 priority/name 唯一冲突（持锁调用；excludeID 为更新目标自身）。
func (f *fakeStore) ruleConflictLocked(excludeID int64, r domain.Rule) error {
	for _, e := range f.rules {
		if e.ID != excludeID && (e.Priority == r.Priority || e.Name == r.Name) {
			return fmt.Errorf("%w: priority=%d or name=%q", repository.ErrConflict, r.Priority, r.Name)
		}
	}
	return nil
}

// --- Phase 3a：UserStore / SettingStore 假实现 ---

func (f *fakeStore) CreateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u.ID = f.nextID
	f.nextID++
	c := *u
	f.users[u.ID] = &c
	return &c, nil
}

func (f *fakeStore) GetUser(ctx context.Context, id int64) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return nil, missingErr(id)
	}
	c := *u
	return &c, nil
}

func (f *fakeStore) GetUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.users {
		if u.Email == email {
			c := *u
			return &c, nil
		}
	}
	return nil, nil
}

func (f *fakeStore) ListUsers(ctx context.Context, q repository.ListQuery) ([]*domain.User, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.User
	for _, u := range f.users {
		if q.Email != "" && !strings.Contains(u.Email, q.Email) {
			continue
		}
		c := *u
		out = append(out, &c)
	}
	return out, int64(len(out)), nil
}

func (f *fakeStore) UpdateUser(ctx context.Context, u *domain.User) (*domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cur, ok := f.users[u.ID]
	if !ok {
		return nil, missingErr(u.ID)
	}
	cur.Role = u.Role
	cur.Status = u.Status
	cur.MaxConcurrency = u.MaxConcurrency
	cur.Balance = u.Balance
	c := *cur
	return &c, nil
}

func (f *fakeStore) UpdateUserPassword(ctx context.Context, id int64, passwordHash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return missingErr(id)
	}
	u.PasswordHash = passwordHash
	return nil
}

func (f *fakeStore) GetSetting(ctx context.Context, key string) (*domain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.settings[key]; ok {
		c := *s
		return &c, nil
	}
	return domain.DefaultSetting(key), nil
}

func (f *fakeStore) GetAllSettings(ctx context.Context) ([]*domain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*domain.Setting
	for _, d := range domain.DefaultSettings {
		if s, ok := f.settings[d.Key]; ok {
			c := *s
			out = append(out, &c)
		} else {
			dd := d
			out = append(out, &dd)
		}
	}
	return out, nil
}

func (f *fakeStore) SetSetting(ctx context.Context, key string, typ domain.SettingType, value string) (*domain.Setting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[key] = &domain.Setting{Key: key, Type: typ, Value: value}
	return &domain.Setting{Key: key, Type: typ, Value: value}, nil
}

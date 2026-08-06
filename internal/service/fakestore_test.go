package service

import (
	"context"
	"fmt"
	"sync"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// fakeStore 以值语义模拟真实仓库（ent 每次返回新对象、无指针别名）：
// Create/Get/Update 均返回副本，存库条目一经写入不再被外部指针修改。
// 若直接存/返回调用方指针，RotateGroupKey 等原地修改会透过别名污染
// 测试持有的旧引用（评审发现：测试必然失败或退化为恒真断言）。
type fakeStore struct {
	mu        sync.Mutex
	tpls      map[int64]*domain.Template
	accs      map[int64]*domain.Account
	groups    map[int64]*domain.Group
	members   map[int64][]int64
	logs      []*domain.UsageLog
	stats     []*domain.StatBucket
	nextID    int64
	keyHashes map[int64]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tpls: make(map[int64]*domain.Template), accs: make(map[int64]*domain.Account),
		groups: make(map[int64]*domain.Group), members: make(map[int64][]int64),
		keyHashes: make(map[int64]string), nextID: 1,
	}
}

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
	f.keyHashes[g.ID] = g.KeyHash
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

func (f *fakeStore) SetGroupAccounts(ctx context.Context, groupID int64, accountIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[groupID] = accountIDs
	return nil
}

// --- 批量操作（缺失 id → repository.ErrNotFound 包装，模拟真实事务内存在性检查） ---

func (f *fakeStore) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.tpls[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
	}
	for _, id := range ids {
		delete(f.tpls, id)
	}
	return nil
}

func (f *fakeStore) UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		t, ok := f.tpls[id]
		if !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
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
			t.ModelMapping = *p.ModelMapping
		}
	}
	return nil
}

func (f *fakeStore) DeleteAccountsBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.accs[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
	}
	for _, id := range ids {
		delete(f.accs, id)
	}
	return nil
}

func (f *fakeStore) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		a, ok := f.accs[id]
		if !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
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
	}
	return nil
}

func (f *fakeStore) DeleteGroupsBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.groups[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
	}
	for _, id := range ids {
		delete(f.groups, id)
	}
	return nil
}

func (f *fakeStore) UpdateGroupsBatch(ctx context.Context, ids []int64, p repository.GroupPatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		g, ok := f.groups[id]
		if !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
		if p.Name != nil {
			g.Name = *p.Name
		}
	}
	return nil
}

func (f *fakeStore) QueryLogs(ctx context.Context, q repository.LogQuery) ([]*domain.UsageLog, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logs, int64(len(f.logs)), nil
}

func (f *fakeStore) ScanStats(ctx context.Context, q repository.StatQuery) ([]*domain.StatBucket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, nil
}

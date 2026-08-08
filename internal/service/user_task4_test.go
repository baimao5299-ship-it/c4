package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go-proxy-mini/internal/domain"
)

// newTask4Svc 构造带 fakeKeyRegistrar 的 Service（key 增量注册可断言）。
func newTask4Svc() (*Service, *fakeStore, *fakeKeyRegistrar) {
	fs := newFakeStore()
	keys := &fakeKeyRegistrar{}
	svc := &Service{store: fs, invalidate: func() {}, keys: keys, log: nil}
	return svc, fs, keys
}

// TestCreateKeyGroupEligibility key 创建组可选性：public 可建；private 未授予
// → ErrGroupNotEligible（Is ErrInvalidInput）；授予后可建；非法参数 → 400。
func TestCreateKeyGroupEligibility(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	user, err := fs.CreateUser(ctx, &domain.User{Email: "u@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	pub, err := fs.CreateGroup(ctx, &domain.Group{Name: "pub", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	priv, err := fs.CreateGroup(ctx, &domain.Group{Name: "priv", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	// public → 可建；raw 明文仅创建返回
	k, raw, err := svc.CreateKey(ctx, user.ID, "k1", pub.ID, 0, 0)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	require.Equal(t, domain.KeyStatusActive, k.Status)
	require.Len(t, keys.upserted, 1, "创建后必须增量注册 Auth 快照")

	// private 未授予 → ErrGroupNotEligible
	_, _, err = svc.CreateKey(ctx, user.ID, "k2", priv.ID, 0, 0)
	require.ErrorIs(t, err, ErrGroupNotEligible)
	require.ErrorIs(t, err, ErrInvalidInput, "必须映射 400")

	// 组缺失 → ErrNotFound（404）
	_, _, err = svc.CreateKey(ctx, user.ID, "k3", 99999, 0, 0)
	require.ErrorIs(t, err, ErrNotFound)

	// 非法参数
	_, _, err = svc.CreateKey(ctx, user.ID, "", pub.ID, 0, 0)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, _, err = svc.CreateKey(ctx, user.ID, "k4", pub.ID, -1, 0)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, _, err = svc.CreateKey(ctx, user.ID, "k5", pub.ID, 0, -1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, _, err = svc.CreateKey(ctx, user.ID, "k6", 0, 0, 0)
	require.ErrorIs(t, err, ErrInvalidInput)

	// 授予后可建
	require.NoError(t, fs.GrantGroup(ctx, priv.ID, user.ID))
	_, _, err = svc.CreateKey(ctx, user.ID, "k2", priv.ID, 0, 0)
	require.NoError(t, err, "授予后 private 可建")
}

// TestKeyOwnership 越权隔离：他人 key 一律 404（Get/Update/Rotate/Delete），
// 不泄露存在性。
func TestKeyOwnership(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()

	alice, err := fs.CreateUser(ctx, &domain.User{Email: "a@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	bob, err := fs.CreateUser(ctx, &domain.User{Email: "b@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, _, err := svc.CreateKey(ctx, alice.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)

	// bob 访问 alice 的 key → 404
	_, err = svc.GetKey(ctx, bob.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.UpdateKey(ctx, bob.ID, k.ID, nil, nil, nil, nil)
	require.ErrorIs(t, err, ErrNotFound)
	_, _, err = svc.RotateKey(ctx, bob.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound)
	err = svc.DeleteKey(ctx, bob.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound)

	// alice 本人操作正常
	updated, err := svc.UpdateKey(ctx, alice.ID, k.ID, nil, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, k.ID, updated.ID)
	raw, rotated, err := svc.RotateKey(ctx, alice.ID, k.ID)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	require.NotEqual(t, k.KeyHash, rotated.KeyHash, "轮换后 hash 变化")
	require.NoError(t, svc.DeleteKey(ctx, alice.ID, k.ID))
	_, err = svc.GetKey(ctx, alice.ID, k.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestKeyUpdateFields 更新字段校验：空 name / 非法 status / 负值 → 400；
// 变更后 Auth 增量注册。
func TestKeyUpdateFields(t *testing.T) {
	svc, fs, keys := newTask4Svc()
	ctx := context.Background()

	u, err := fs.CreateUser(ctx, &domain.User{Email: "u@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPublic})
	require.NoError(t, err)
	k, _, err := svc.CreateKey(ctx, u.ID, "k", g.ID, 0, 0)
	require.NoError(t, err)

	empty := ""
	st := domain.KeyStatus(domain.KeyStatusDisabled)
	neg := -1
	bad := domain.KeyStatus("bogus")
	q := int64(1000)

	_, err = svc.UpdateKey(ctx, u.ID, k.ID, &empty, nil, nil, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, &bad, nil, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateKey(ctx, u.ID, k.ID, nil, nil, &neg, nil)
	require.ErrorIs(t, err, ErrInvalidInput)

	updated, err := svc.UpdateKey(ctx, u.ID, k.ID, nil, &st, nil, &q)
	require.NoError(t, err)
	require.Equal(t, domain.KeyStatusDisabled, updated.Status)
	require.Equal(t, int64(1000), updated.Quota)
	require.Greater(t, len(keys.upserted), 1, "更新后必须增量注册")
}

// TestSetGroupAssignments 替换语义：差集授予/撤销；非法/重复/缺失校验。
func TestSetGroupAssignments(t *testing.T) {
	svc, fs, _ := newTask4Svc()
	ctx := context.Background()

	u1, err := fs.CreateUser(ctx, &domain.User{Email: "a@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	u2, err := fs.CreateUser(ctx, &domain.User{Email: "b@example.com", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	g, err := fs.CreateGroup(ctx, &domain.Group{Name: "g", Visibility: domain.GroupVisibilityPrivate})
	require.NoError(t, err)

	// 授予两人 → 列表一致
	applied, err := svc.SetGroupAssignments(ctx, g.ID, []int64{u1.ID, u2.ID})
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{u1.ID, u2.ID}, applied)
	got, err := fs.ListAssignmentsByGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)

	// 替换为只留 u1（u2 撤销）
	applied, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u1.ID})
	require.NoError(t, err)
	require.Equal(t, []int64{u1.ID}, applied)
	got, err = fs.ListAssignmentsByGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, u1.ID, got[0].UserID)

	// 清空
	applied, err = svc.SetGroupAssignments(ctx, g.ID, nil)
	require.NoError(t, err)
	require.Empty(t, applied)
	got, err = fs.ListAssignmentsByGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Empty(t, got)

	// 校验：组缺失 → 404；用户缺失 → 404；重复 → 400；超长 → 400
	_, err = svc.SetGroupAssignments(ctx, 99999, []int64{u1.ID})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.SetGroupAssignments(ctx, g.ID, []int64{99999})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = svc.SetGroupAssignments(ctx, g.ID, []int64{u1.ID, u1.ID})
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.SetGroupAssignments(ctx, g.ID, []int64{0})
	require.ErrorIs(t, err, ErrInvalidInput)
	big := make([]int64, 101)
	_, err = svc.SetGroupAssignments(ctx, g.ID, big)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestCreateUserAdmin 管理面创建用户：email 唯一/格式、密码长度、role/status
// 校验；创建后 invalidate。
func TestCreateUserAdmin(t *testing.T) {
	fs := newFakeStore()
	invalidated := 0
	svc := &Service{store: fs, invalidate: func() { invalidated++ }, log: nil}
	ctx := context.Background()

	u, err := svc.CreateUser(ctx, "admin@example.com", "s3cret-pass",
		domain.RolePlatformAdmin, domain.UserStatusActive, 4, 100, nil)
	require.NoError(t, err)
	require.Equal(t, domain.RolePlatformAdmin, u.Role)
	require.Equal(t, 4, u.MaxConcurrency)
	require.Equal(t, int64(100), u.Balance)
	require.Greater(t, invalidated, 0, "创建用户必须 invalidate（Auth 状态快照）")

	// email 重复 → 409
	_, err = svc.CreateUser(ctx, "admin@example.com", "x", domain.RoleUser, domain.UserStatusActive, 0, 0, nil)
	require.ErrorIs(t, err, ErrConflict)

	// 非法输入
	_, err = svc.CreateUser(ctx, "bad", "x", domain.RoleUser, domain.UserStatusActive, 0, 0, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateUser(ctx, "ok@example.com", "", domain.RoleUser, domain.UserStatusActive, 0, 0, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateUser(ctx, "ok@example.com", "short", domain.Role("bogus"), domain.UserStatusActive, 0, 0, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateUser(ctx, "ok@example.com", "short", domain.RoleUser, domain.UserStatus("bogus"), 0, 0, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CreateUser(ctx, "ok@example.com", "short", domain.RoleUser, domain.UserStatusActive, -1, 0, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

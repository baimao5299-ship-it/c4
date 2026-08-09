import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Ban, CircleCheck, UserCog, Filter } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { ListToolbar } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { StatusBadge } from '@/components/status-badge'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'
import { formatDateTime } from '@/components/fmt'
import type { TFunction } from 'i18next'
import type { components } from '@/lib/api/schema'

type User = components['schemas']['User']
type UserCreate = components['schemas']['UserCreate']
type UserUpdate = components['schemas']['UserUpdate']
type UserRole = components['schemas']['UserRole']
type UserStatus = components['schemas']['UserStatus']

const ROLES: UserRole[] = ['platform_admin', 'user']
const STATUSES: UserStatus[] = ['active', 'disabled']

// 价格倍率（万分数）→ 展示：null = 未设置（—）；0 = 免费；其余 ×N（10000 → ×1.0）。
const formatMultiplier = (m: number | null | undefined, t: TFunction): string => {
  if (m == null) return '—'
  if (m === 0) return t('users.free')
  return `×${(m / 10000).toFixed(1)}`
}

// 余额（USD 浮点，已由 API 边界换算）→ $N.NN；空 → —。
const formatBalance = (b?: number): string => (b == null ? '—' : `$${b.toFixed(2)}`)

// 角色徽章：platform_admin 蓝点（管理面）/ user 灰点（普通用户，与 groups
// VisibilityBadge 同风格）。
function RoleBadge({ role }: { role?: UserRole }) {
  const { t } = useTranslation()
  const isAdmin = role === 'platform_admin'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', isAdmin ? 'text-blue-700 dark:text-blue-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', isAdmin ? 'bg-blue-500' : 'bg-muted-foreground/60')} />
      {t(isAdmin ? 'users.role.platform_admin' : 'users.role.user')}
    </Badge>
  )
}

// 创建/编辑共用一个表单态：email/password 仅创建时使用（email 不可变），
// clearMultiplier 仅编辑时使用（勾选 → 显式发送 null 清除倍率）。
interface UserForm {
  email: string
  password: string
  role: UserRole
  status: UserStatus
  max_concurrency: string
  balance: string
  multiplier: string
  clearMultiplier: boolean
}

const emptyForm = (): UserForm => ({
  email: '',
  password: '',
  role: 'user',
  status: 'active',
  max_concurrency: '',
  balance: '',
  multiplier: '',
  clearMultiplier: false,
})

function toForm(u: User): UserForm {
  return {
    email: u.Email ?? '',
    password: '',
    role: u.Role ?? 'user',
    status: u.Status ?? 'active',
    max_concurrency: u.MaxConcurrency == null ? '' : String(u.MaxConcurrency),
    balance: u.Balance == null ? '' : String(u.Balance),
    multiplier: u.PriceMultiplier == null ? '' : String(u.PriceMultiplier),
    clearMultiplier: false,
  }
}

// 创建体：email/password 必填；数值字段空 = 不发送（后端 settings 缺省）。
function toCreateBody(f: UserForm): UserCreate {
  const body: UserCreate = {
    email: f.email.trim(),
    password: f.password,
    role: f.role,
    status: f.status,
  }
  if (f.max_concurrency !== '') body.max_concurrency = Number(f.max_concurrency)
  if (f.balance !== '') body.balance = Number(f.balance)
  if (f.multiplier !== '') body.price_multiplier = Number(f.multiplier)
  return body
}

// 更新体（UserUpdate 全可选）：role/status 为 Select 必有值，总是发送
// （读改写幂等）；数值字段空 = 不发送；倍率：勾选清除 → null，有值 → 数字，
// 都无 → 缺省（后端视为未提供，保持不变）。
function toUpdateBody(f: UserForm): UserUpdate {
  const body: UserUpdate = {
    role: f.role,
    status: f.status,
  }
  if (f.max_concurrency !== '') body.max_concurrency = Number(f.max_concurrency)
  if (f.balance !== '') body.balance = Number(f.balance)
  if (f.clearMultiplier) body.price_multiplier = null
  else if (f.multiplier !== '') body.price_multiplier = Number(f.multiplier)
  return body
}

export default function Users() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey（排序白名单：id/email/role/status/
  // max_concurrency/created_at/updated_at——balance/price_multiplier 不可排）——
  const [email, setEmail] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['users', { limit, offset, email, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listUsers({ limit, offset, email: email || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = data?.rows ?? []

  const resetPage = () => setOffset(0)
  // 每页条数变化 → 重置 offset。
  const changeLimit = (l: number) => { setLimit(l); resetPage() }
  const changeEmail = (v: string) => { setEmail(v); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）
  const onColumnToggle = (col: string) => {
    resetPage()
    if (activeSort !== col) {
      setActiveSort(col)
      setOrder('desc')
    } else if (order === 'desc') {
      setOrder('asc')
    } else {
      setActiveSort(null)
      setOrder('desc')
    }
  }
  const hasFilters = email !== ''
  const clearFilters = () => { setEmail(''); resetPage() }

  // —— 创建/编辑 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<User | null>(null)
  const [form, setForm] = useState<UserForm>(emptyForm())

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setDialogOpen(true)
  }
  const openEdit = (u: User) => {
    setEditing(u)
    setForm(toForm(u))
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: (f: UserForm) =>
      editing ? api.updateUser(editing.ID!, toUpdateBody(f)) : api.createUser(toCreateBody(f)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['users'] })
      setDialogOpen(false)
    },
  })
  // 启用/禁用 quick action（与 accounts 同款；无 DELETE API，不提供删除）。
  const toggleStatus = useMutation({
    mutationFn: (u: User) =>
      api.updateUser(u.ID!, { status: u.Status === 'disabled' ? 'active' : 'disabled' }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['users'] }),
  })

  const submit = () => {
    if (!form.email.trim() || (!editing && !form.password)) return
    save.mutate(form)
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{t('users.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('users.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('users.new')}</Button>
      </div>

      <ListToolbar
        name={email}
        onNameChange={changeEmail}
        placeholder={t('users.searchEmail')}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <UserCog className="size-10" />
            <p className="font-medium">{hasFilters ? t('users.filterEmpty') : t('users.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('users.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {t('users.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="email" label={t('users.table.email')} active={activeSort === 'email'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="role" label={t('users.table.role')} active={activeSort === 'role'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="status" label={t('users.table.status')} active={activeSort === 'status'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="max_concurrency" label={t('users.table.maxConcurrency')} active={activeSort === 'max_concurrency'} order={order} onToggle={onColumnToggle} className="text-right [&_button]:justify-end" />
                  <TableHead className="text-right">{t('users.table.balance')}</TableHead>
                  <TableHead className="text-right">{t('users.table.priceMultiplier')}</TableHead>
                  <SortableHeader field="created_at" label={t('users.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('users.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(u => (
                  <TableRow key={u.ID}>
                    <TableCell className="tabular-nums">{u.ID}</TableCell>
                    <TableCell className="max-w-52 truncate" title={u.Email}>{u.Email}</TableCell>
                    <TableCell><RoleBadge role={u.Role} /></TableCell>
                    <TableCell><StatusBadge status={u.Status} /></TableCell>
                    <TableCell className="text-right tabular-nums">{u.MaxConcurrency ?? 0}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatBalance(u.Balance)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatMultiplier(u.PriceMultiplier, t)}</TableCell>
                    <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(u.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title={u.Status === 'disabled' ? t('users.enable') : t('users.disable')}
                          onClick={() => toggleStatus.mutate(u)}
                          disabled={toggleStatus.isPending}
                        >
                          {u.Status === 'disabled' ? <CircleCheck /> : <Ban />}
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(u)}><Pencil /></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <Pagination total={data?.total ?? 0} limit={limit} offset={offset} onOffsetChange={setOffset} onLimitChange={changeLimit} />
        </>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editing ? t('users.editTitle', { id: editing.ID }) : t('users.newTitle')}</DialogTitle>
            <DialogDescription>{t('users.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="usr-email">{t('users.emailLabel')}</Label>
              <Input
                id="usr-email"
                type="email"
                value={form.email}
                placeholder={t('users.emailPlaceholder')}
                onChange={e => setForm(f => ({ ...f, email: e.target.value }))}
                disabled={!!editing}
              />
              {editing && <p className="text-xs text-muted-foreground">{t('users.emailImmutable')}</p>}
            </div>
            {!editing && (
              <div className="space-y-1.5">
                <Label htmlFor="usr-password">{t('users.passwordLabel')}</Label>
                <Input
                  id="usr-password"
                  type="password"
                  value={form.password}
                  onChange={e => setForm(f => ({ ...f, password: e.target.value }))}
                />
                <p className="text-xs text-muted-foreground">{t('users.passwordHint')}</p>
              </div>
            )}
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label>{t('users.roleLabel')}</Label>
                <Select value={form.role} items={Object.fromEntries(ROLES.map(r => [r, t(`users.role.${r}`)]))} onValueChange={v => setForm(f => ({ ...f, role: v as UserRole }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {ROLES.map(r => <SelectItem key={r} value={r} label={t(`users.role.${r}`)}>{t(`users.role.${r}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{t('users.statusLabel')}</Label>
                <Select value={form.status} items={Object.fromEntries(STATUSES.map(s => [s, t(`status.${s}`)]))} onValueChange={v => setForm(f => ({ ...f, status: v as UserStatus }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="usr-max">{t('users.maxLabel')}</Label>
                <Input id="usr-max" type="number" min={0} value={form.max_concurrency} onChange={e => setForm(f => ({ ...f, max_concurrency: e.target.value }))} />
                <p className="text-xs text-muted-foreground">{t('users.maxHint')}</p>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="usr-balance">{t('users.balanceLabel')}</Label>
                <Input id="usr-balance" type="number" min={0} step={0.01} value={form.balance} onChange={e => setForm(f => ({ ...f, balance: e.target.value }))} />
              </div>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="usr-mult">{t('users.multiplierLabel')}</Label>
              <div className="flex items-center gap-3">
                <Input
                  id="usr-mult"
                  type="number"
                  min={0}
                  max={100000}
                  value={form.multiplier}
                  placeholder="10000"
                  className="flex-1"
                  onChange={e => setForm(f => ({ ...f, multiplier: e.target.value }))}
                  disabled={form.clearMultiplier}
                />
                {editing && (
                  <label className="flex shrink-0 cursor-pointer items-center gap-2">
                    <Checkbox checked={form.clearMultiplier} onCheckedChange={c => setForm(f => ({ ...f, clearMultiplier: c === true }))} />
                    <span className="text-sm">{t('users.clearMultiplier')}</span>
                  </label>
                )}
              </div>
              <p className="text-xs text-muted-foreground">{t('users.multiplierHint')}</p>
            </div>
            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending || !form.email.trim() || (!editing && !form.password)}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

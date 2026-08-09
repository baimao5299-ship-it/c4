import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, FolderOpen, Filter, UserPlus, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
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
import { Badge } from '@/components/ui/badge'
import { toast } from '@/components/ui/toast'
import { formatDateTime } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { TFunction } from 'i18next'
import type { components } from '@/lib/api/schema'

type Group = components['schemas']['Group']
type GroupVisibility = components['schemas']['GroupVisibility']
type GroupAssignmentsBody = components['schemas']['GroupAssignmentsBody']
type GroupAssignmentsResponse = components['schemas']['GroupAssignmentsResponse']

// 授予弹窗行内专属倍率态：mult = 输入框文本（'' = 未填）；cleared = 用户显式点过
// 「清除为未设置」（提交 null）；勾选留空且未清除 = 省略键（沿用当前值）。
interface AssignRowMult { mult: string; cleared: boolean }

// 价格倍率（正常值，API 边界已换算）→ 展示：null = 未设置（—）；0 = 免费；
// 其余 ×N（1 = ×1.0，1.5 = ×1.5）。
const formatMultiplier = (m: number | null | undefined, t: TFunction): string => {
  if (m == null) return '—'
  if (m === 0) return t('groups.free')
  return `×${m.toFixed(1)}`
}

// 可见性徽章：public 绿点 / private 灰点（与 StatusBadge 同风格）。
function VisibilityBadge({ visibility }: { visibility?: GroupVisibility }) {
  const { t } = useTranslation()
  const isPublic = visibility === 'public'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', isPublic ? 'text-emerald-700 dark:text-emerald-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', isPublic ? 'bg-emerald-500' : 'bg-muted-foreground/60')} />
      {t(isPublic ? 'groups.visibilityPublic' : 'groups.visibilityPrivate')}
    </Badge>
  )
}

export default function Groups() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey ——
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [limit, setLimit] = useState(20)

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['groups', { limit, offset, name, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listGroups({ limit, offset, name: name || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = data?.rows ?? []

  // —— 行勾选（跨页保留，筛选/翻页后清空）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID!)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  const resetPage = () => {
    setOffset(0)
    setSelected([])
  }
  // 每页条数变化 → 重置 offset 并清勾选。
  const changeLimit = (l: number) => { setLimit(l); resetPage() }
  const changeName = (v: string) => { setName(v); resetPage() }
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
  const hasFilters = name !== ''
  const clearFilters = () => {
    setName('')
    resetPage()
  }

  // —— 批量删除/重命名 ——
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteGroupsBatch(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setSelected([])
    },
  })
  const batchRename = useMutation({
    mutationFn: (p: { ids: number[]; name: string }) => api.updateGroupsBatch(p.ids, { name: p.name }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setSelected([])
      closeBatchRename('submitted')
    },
  })
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const [batchRenameOpen, setBatchRenameOpen] = useState(false)
  const [batchRenameValue, setBatchRenameValue] = useState('')
  const [batchRenameErr, setBatchRenameErr] = useState<string | null>(null)
  const batchResolve = useRef<((r: 'cancelled' | 'submitted') => void) | null>(null)
  const closeBatchRename = (r: 'cancelled' | 'submitted' = 'cancelled') => {
    setBatchRenameOpen(false)
    batchResolve.current?.(r)
    batchResolve.current = null
  }
  const openBatchRename = () => {
    setBatchRenameValue('')
    setBatchRenameErr(null)
    setBatchRenameOpen(true)
  }
  const submitBatchRename = () => {
    if (!batchRenameValue.trim()) {
      setBatchRenameErr(t('groups.batchUpdateEmpty'))
      return
    }
    batchRename.mutate({ ids: selected, name: batchRenameValue.trim() })
  }

  // —— 授予用户（替换语义：勾选 = 授予，未勾选 = 撤销；无 GET 回显，重开默认空）——
  const [assignTarget, setAssignTarget] = useState<Group | null>(null)
  const [assignChecked, setAssignChecked] = useState<number[]>([])
  const [assignMult, setAssignMult] = useState<Record<number, AssignRowMult>>({})
  const [assignQuery, setAssignQuery] = useState('')
  const [assignOffset, setAssignOffset] = useState(0)
  const [assignLimit, setAssignLimit] = useState(20)
  // 上次提交成功的响应本地保留（弹窗内提示「已保存的授予」；契约无 GET 端点）。
  const [assignSaved, setAssignSaved] = useState<GroupAssignmentsResponse | null>(null)

  const openAssign = (g: Group) => {
    setAssignTarget(g)
    setAssignChecked([])
    setAssignMult({})
    setAssignQuery('')
    setAssignOffset(0)
  }
  const toggleAssignUser = (id: number, on: boolean) =>
    setAssignChecked(s => (on ? (s.includes(id) ? s : [...s, id]) : s.filter(x => x !== id)))
  const assignUsers = useQuery({
    queryKey: ['users', 'assign', { limit: assignLimit, offset: assignOffset, email: assignQuery }],
    queryFn: () => api.listUsers({ limit: assignLimit, offset: assignOffset, email: assignQuery || undefined }),
    enabled: !!assignTarget,
  })
  const assignRows = assignUsers.data?.rows ?? []
  const assignTotal = assignUsers.data?.total ?? 0

  // multipliers 三态：勾选且填值 → 数字；勾选留空（未清除）→ 省略键（沿用当前值）；
  // 勾选且显式清除 → null（回退组倍率）。
  const assign = useMutation({
    mutationFn: () => {
      const body: GroupAssignmentsBody = { user_ids: assignChecked }
      const muls: Record<string, number | null> = {}
      for (const uid of assignChecked) {
        const row = assignMult[uid]
        const v = row?.mult.trim()
        if (v !== undefined && v !== '') {
          const n = Number(v)
          if (!Number.isFinite(n) || n < 0 || n > 10) throw new Error(t('groups.multiplierInvalid'))
          muls[String(uid)] = n
        } else if (row?.cleared) {
          muls[String(uid)] = null
        }
      }
      if (Object.keys(muls).length > 0) body.multipliers = muls
      return api.setGroupAssignments(assignTarget!.ID!, body)
    },
    onSuccess: (resp) => {
      setAssignSaved(resp)
      setAssignTarget(null)
      toast.add({ title: t('groups.assignSuccess'), description: t('groups.assignSuccessDesc', { count: resp.user_ids.length }), type: 'success' })
    },
  })
  const setRowMult = (uid: number, mult: string) => setAssignMult(m => ({ ...m, [uid]: { mult, cleared: false } }))
  const clearRowMult = (uid: number) => setAssignMult(m => ({ ...m, [uid]: { mult: '', cleared: true } }))

  // —— 创建（表单：name + visibility；POST 不设倍率）——
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createVisibility, setCreateVisibility] = useState<GroupVisibility>('public')
  const openCreate = () => {
    setCreateName('')
    setCreateVisibility('public')
    setCreateOpen(true)
  }
  const create = useMutation({
    mutationFn: (n: string) => api.createGroup({ name: n, visibility: createVisibility }),
    onSuccess: (_g, name) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setCreateOpen(false)
      toast.add({ title: t('groups.createdSuccess'), description: name, type: 'success' })
    },
  })
  // —— 编辑（name + visibility；PUT 缺省字段保持原值，此处总是显式提交）——
  const [editTarget, setEditTarget] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  const [editVisibility, setEditVisibility] = useState<GroupVisibility>('public')
  // 倍率用字符串态：空 = 不修改（PUT 省略键，后端保持原值）
  const [editMultiplier, setEditMultiplier] = useState('')
  // —— 删除 ——
  const [deleting, setDeleting] = useState<Group | null>(null)

  const rename = useMutation({
    mutationFn: () => {
      const body: components['schemas']['GroupCreate'] = { name: editName.trim(), visibility: editVisibility }
      const m = editMultiplier.trim()
      if (m !== '') {
        const v = Number(m)
        if (!Number.isFinite(v) || v < 0 || v > 10) {
          throw new Error(t('groups.multiplierInvalid'))
        }
        body.price_multiplier = v // 正常值直接提交：0 = 免费组，1 = ×1，上限 10；输入为空则省略键（后端保持原值）
      }
      return api.updateGroup(editTarget!.ID!, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setEditTarget(null)
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteGroup(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setDeleting(null)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{t('groups.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('groups.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('groups.new')}</Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={changeName}
      />

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
        onUpdate={() => new Promise<'cancelled' | 'submitted'>(resolve => {
          batchResolve.current = resolve
          openBatchRename()
        })}
      />

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 3 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <FolderOpen className="size-10" />
            <p className="font-medium">{hasFilters ? t('groups.filterEmpty') : t('groups.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('groups.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {t('groups.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead className="w-10">
                    <Checkbox
                      checked={allChecked}
                      indeterminate={someChecked && !allChecked}
                      onCheckedChange={c => toggleAll(c === true)}
                    />
                  </TableHead>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="name" label={t('groups.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{t('groups.table.visibility')}</TableHead>
                  <TableHead>{t('groups.table.priceMultiplier')}</TableHead>
                  <SortableHeader field="created_at" label={t('groups.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('groups.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(g => (
                  <TableRow key={g.ID} data-state={selected.includes(g.ID!) ? 'selected' : undefined}>
                    <TableCell>
                      <Checkbox checked={selected.includes(g.ID!)} onCheckedChange={() => toggleRow(g.ID!)} />
                    </TableCell>
                    <TableCell className="tabular-nums">{g.ID}</TableCell>
                    <TableCell className="max-w-36 truncate" title={g.Name}>{g.Name}</TableCell>
                    <TableCell><VisibilityBadge visibility={g.Visibility} /></TableCell>
                    <TableCell className="tabular-nums">{formatMultiplier(g.PriceMultiplier, t)}</TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateTime(g.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('groups.assignButton')} onClick={() => openAssign(g)}><UserPlus /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => { setEditTarget(g); setEditName(g.Name ?? ''); setEditVisibility(g.Visibility ?? 'public'); setEditMultiplier(g.PriceMultiplier != null ? String(g.PriceMultiplier) : '') }}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(g)}><Trash2 /></Button>
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

      {/* —— 创建分组：表单（name + visibility）；创建成功仅提示，不再返回 key 明文 —— */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.newTitle')}</DialogTitle>
            <DialogDescription>{t('groups.newDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-name">{t('groups.nameLabel')}</Label>
              <Input
                id="grp-name"
                value={createName}
                placeholder={t('groups.namePlaceholder')}
                onChange={e => setCreateName(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && createName.trim() && !create.isPending) create.mutate(createName.trim()) }}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-visibility">{t('groups.visibilityLabel')}</Label>
              <Select
                items={Object.fromEntries([['public', t('groups.visibilityPublic')], ['private', t('groups.visibilityPrivate')]])}
                value={createVisibility}
                onValueChange={v => setCreateVisibility(v as GroupVisibility)}
              >
                <SelectTrigger id="grp-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {create.isError && errMsg(create.error) && (
              <p className="text-sm text-destructive">{errMsg(create.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setCreateOpen(false)} disabled={create.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => create.mutate(createName.trim())} disabled={create.isPending || !createName.trim()}>
              {create.isPending ? t('common.creating') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 编辑（name + visibility） —— */}
      <Dialog open={!!editTarget} onOpenChange={o => { if (!o && !rename.isPending) setEditTarget(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.editTitle', { id: editTarget?.ID })}</DialogTitle>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-name">{t('groups.nameLabel')}</Label>
              <Input id="grp-edit-name" value={editName} onChange={e => setEditName(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-visibility">{t('groups.visibilityLabel')}</Label>
              <Select
                items={Object.fromEntries([['public', t('groups.visibilityPublic')], ['private', t('groups.visibilityPrivate')]])}
                value={editVisibility}
                onValueChange={v => setEditVisibility(v as GroupVisibility)}
              >
                <SelectTrigger id="grp-edit-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="grp-edit-multiplier">{t('groups.multiplierLabel')}</Label>
              <Input
                id="grp-edit-multiplier"
                type="number"
                min={0}
                max={10}
                step={0.1}
                value={editMultiplier}
                onChange={e => setEditMultiplier(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && editName.trim() && !rename.isPending) rename.mutate() }}
              />
              <p className="text-xs text-muted-foreground">{t('groups.multiplierHint')}</p>
            </div>
            {rename.isError && errMsg(rename.error) && (
              <p className="text-sm text-destructive">{errMsg(rename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setEditTarget(null)} disabled={rename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => rename.mutate()} disabled={rename.isPending || !editName.trim()}>
              {rename.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认（单行） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('groups.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID!)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量更新对话框：仅 name —— */}
      <Dialog open={batchRenameOpen} onOpenChange={o => { if (!o) closeBatchRename() }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.batchUpdateTitle')}</DialogTitle>
            <DialogDescription>{t('groups.batchUpdateDesc', { count: selected.length })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="grp-batch-name">{t('groups.nameLabel')}</Label>
              <Input
                id="grp-batch-name"
                value={batchRenameValue}
                placeholder={t('groups.namePlaceholder')}
                onChange={e => setBatchRenameValue(e.target.value)}
                onKeyDown={e => { if (e.key === 'Enter' && batchRenameValue.trim() && !batchRename.isPending) submitBatchRename() }}
              />
            </div>
            {batchRenameErr && <p className="text-sm text-destructive">{batchRenameErr}</p>}
            {batchRename.isError && errMsg(batchRename.error) && (
              <p className="text-sm text-destructive">{errMsg(batchRename.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => closeBatchRename()} disabled={batchRename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitBatchRename} disabled={batchRename.isPending || !batchRenameValue.trim()}>
              {batchRename.isPending ? t('common.saving') : t('list.batchUpdate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 授予用户：替换语义（勾选 = 授予，未勾选 = 撤销）+ 专属倍率三态 —— */}
      <Dialog open={!!assignTarget} onOpenChange={o => { if (!o && !assign.isPending) setAssignTarget(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t('groups.assignTitle', { name: assignTarget?.Name })}</DialogTitle>
            <DialogDescription>{t('groups.assignDesc')}</DialogDescription>
            <p className="text-xs text-muted-foreground">{t('groups.assignMultiplierHint')}</p>
            <p className="text-xs text-amber-600 dark:text-amber-400">{t('groups.assignEchoNote')}</p>
            {assignSaved && assignSaved.user_ids.length > 0 && (
              <p className="text-xs text-muted-foreground">{t('groups.assignSavedNote', { count: assignSaved.user_ids.length })}</p>
            )}
          </DialogHeader>
          <div className="space-y-3">
            <Input
              value={assignQuery}
              placeholder={t('groups.assignSearchPlaceholder')}
              onChange={e => { setAssignQuery(e.target.value); setAssignOffset(0) }}
            />
            {assignUsers.isLoading ? (
              <div className="space-y-1.5">
                {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-9" />)}
              </div>
            ) : assignUsers.isError ? (
              <p className="text-sm text-destructive">{t('common.loadFailed', { message: (assignUsers.error as Error).message })}</p>
            ) : assignRows.length === 0 ? (
              <p className="py-6 text-center text-sm text-muted-foreground">{t('groups.assignEmpty')}</p>
            ) : (
              <>
                <div className="max-h-72 space-y-1.5 overflow-y-auto pr-1">
                  {assignRows.map(u => {
                    const checked = assignChecked.includes(u.ID!)
                    const row = assignMult[u.ID!]
                    return (
                      <div key={u.ID} className="flex items-center gap-2.5 rounded-md border px-2 py-1.5">
                        <Checkbox checked={checked} onCheckedChange={c => toggleAssignUser(u.ID!, c === true)} />
                        <span className="min-w-0 flex-1 truncate text-sm" title={u.Email}>{u.Email}</span>
                        {checked && (
                          <>
                            <Input
                              type="number"
                              min={0}
                              max={10}
                              step={0.1}
                              value={row?.mult ?? ''}
                              placeholder={t('groups.assignMultiplierPlaceholder')}
                              onChange={e => setRowMult(u.ID!, e.target.value)}
                              className="h-7 w-24 text-xs"
                            />
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              title={t('groups.assignMultiplierClear')}
                              disabled={!row?.mult}
                              onClick={() => clearRowMult(u.ID!)}
                            >
                              <X />
                            </Button>
                            {row?.cleared && (
                              <span className="w-24 text-xs text-muted-foreground">{t('groups.assignMultiplierUnset')}</span>
                            )}
                          </>
                        )}
                      </div>
                    )
                  })}
                </div>
                <Pagination total={assignTotal} limit={assignLimit} offset={assignOffset} onOffsetChange={setAssignOffset} onLimitChange={l => { setAssignLimit(l); setAssignOffset(0) }} />
              </>
            )}
            {assign.isError && errMsg(assign.error) && (
              <p className="text-sm text-destructive">{errMsg(assign.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setAssignTarget(null)} disabled={assign.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => assign.mutate()} disabled={assign.isPending}>
              {assign.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

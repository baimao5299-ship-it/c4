import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, FolderOpen, Filter } from 'lucide-react'
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

const LIMIT = 20

// 价格倍率（万分数）→ 展示：0 = 免费；其余 ×N（10000 → ×1.0）。
const formatMultiplier = (m: number | undefined, t: TFunction): string =>
  (m ?? 0) === 0 ? t('groups.free') : `×${((m ?? 0) / 10000).toFixed(1)}`

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

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['groups', { limit: LIMIT, offset, name, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listGroups({ limit: LIMIT, offset, name: name || undefined, sort: activeSort ?? 'id', order }),
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
  // —— 删除 ——
  const [deleting, setDeleting] = useState<Group | null>(null)

  const rename = useMutation({
    mutationFn: () => api.updateGroup(editTarget!.ID!, { name: editName.trim(), visibility: editVisibility }),
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
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => { setEditTarget(g); setEditName(g.Name ?? ''); setEditVisibility(g.Visibility ?? 'public') }}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(g)}><Trash2 /></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <Pagination total={data?.total ?? 0} limit={LIMIT} offset={offset} onOffsetChange={setOffset} />
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
              <Select value={createVisibility} onValueChange={v => setCreateVisibility(v as GroupVisibility)}>
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
              <Select value={editVisibility} onValueChange={v => setEditVisibility(v as GroupVisibility)}>
                <SelectTrigger id="grp-edit-visibility" className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="public" label={t('groups.visibilityPublic')}>{t('groups.visibilityPublic')}</SelectItem>
                  <SelectItem value="private" label={t('groups.visibilityPrivate')}>{t('groups.visibilityPrivate')}</SelectItem>
                </SelectContent>
              </Select>
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
    </div>
  )
}

import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, FolderOpen, Link2, RefreshCw, Filter } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { ListToolbar, type SortOrder } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader } from '@/components/sortable-header'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { StatusBadge } from '@/components/status-badge'
import { KeyBox } from '@/components/key-box'
import { formatDateTime, truncate } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type Group = components['schemas']['Group']

const LIMIT = 20

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
  const accountsQ = useQuery({ queryKey: ['accounts'], queryFn: () => api.listAccounts({ limit: 100 }) })
  const accounts = accountsQ.data?.rows ?? []
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
  const changeSort = (v: string) => { setActiveSort(v); resetPage() }
  const changeOrder = (o: SortOrder) => { setOrder(o); resetPage() }
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

  const sortOptions = [
    { value: 'id', label: 'ID' },
    { value: 'name', label: t('groups.table.name') },
    { value: 'created_at', label: t('groups.table.createdAt') },
    { value: 'updated_at', label: t('groups.sort.updatedAt') },
  ]

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
      closeBatchRename()
    },
  })
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const [batchRenameOpen, setBatchRenameOpen] = useState(false)
  const [batchRenameValue, setBatchRenameValue] = useState('')
  const [batchRenameErr, setBatchRenameErr] = useState<string | null>(null)
  const batchResolve = useRef<(() => void) | null>(null)
  const closeBatchRename = () => {
    setBatchRenameOpen(false)
    batchResolve.current?.()
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

  // —— 创建（form → 明文 key 展示）——
  const [createOpen, setCreateOpen] = useState(false)
  const [createName, setCreateName] = useState('')
  const [createdKey, setCreatedKey] = useState<{ name: string; key: string } | null>(null)
  // —— 编辑（重命名）——
  const [editTarget, setEditTarget] = useState<Group | null>(null)
  const [editName, setEditName] = useState('')
  // —— 绑定账号 ——
  const [bindTarget, setBindTarget] = useState<Group | null>(null)
  const [bindChecked, setBindChecked] = useState<number[]>([])
  // —— 轮换 key（确认 → 明文 key 展示）——
  const [rotateTarget, setRotateTarget] = useState<Group | null>(null)
  const [rotateResult, setRotateResult] = useState<{ name: string; key: string } | null>(null)
  // —— 删除 ——
  const [deleting, setDeleting] = useState<Group | null>(null)

  const create = useMutation({
    mutationFn: (n: string) => api.createGroup({ name: n }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setCreatedKey({ name: res.group.Name ?? '', key: res.key })
    },
  })
  const rename = useMutation({
    mutationFn: () => api.updateGroup(editTarget!.ID!, { name: editName.trim() }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setEditTarget(null)
    },
  })
  const bind = useMutation({
    mutationFn: () => api.setGroupAccounts(bindTarget!.ID!, bindChecked),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setBindTarget(null)
    },
  })
  const rotate = useMutation({
    mutationFn: (id: number) => api.rotateGroupKey(id),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ['groups'] })
      setRotateResult({ name: rotateTarget?.Name ?? '', key: res.key })
      setRotateTarget(null)
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

  const openBind = (g: Group) => {
    // API 无读取当前绑定的端点，绑定对话框为全量重选（不勾选 = 全解绑）。
    setBindTarget(g)
    setBindChecked([])
  }
  const toggleChecked = (id: number) =>
    setBindChecked(cs => (cs.includes(id) ? cs.filter(c => c !== id) : [...cs, id]))

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{t('groups.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('groups.subtitle')}</p>
        </div>
        <Button onClick={() => { setCreateName(''); setCreatedKey(null); setCreateOpen(true) }}><Plus /> {t('groups.new')}</Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={changeName}
        sort={activeSort ?? 'id'}
        onSortChange={changeSort}
        order={order}
        onOrderChange={changeOrder}
        sortOptions={sortOptions}
      />

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
        onUpdate={() => new Promise<void>(resolve => {
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
              <Button className="mt-2" onClick={() => { setCreateName(''); setCreatedKey(null); setCreateOpen(true) }}><Plus /> {t('groups.new')}</Button>
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
                  <TableHead>KeyPrefix</TableHead>
                  <TableHead>KeyHash</TableHead>
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
                    <TableCell className="font-mono text-xs">{g.KeyPrefix ?? '—'}</TableCell>
                    <TableCell className="max-w-32 truncate font-mono text-xs text-muted-foreground" title={g.KeyHash}>
                      {truncate(g.KeyHash, 24)}
                    </TableCell>
                    <TableCell className="text-xs text-muted-foreground">{formatDateTime(g.CreatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => { setEditTarget(g); setEditName(g.Name ?? '') }}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('groups.bind')} onClick={() => openBind(g)}><Link2 /></Button>
                        <Button variant="ghost" size="icon-sm" title={t('groups.rotate')} onClick={() => setRotateTarget(g)}><RefreshCw /></Button>
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

      {/* —— 创建分组：表单 → 明文 key 展示 —— */}
      <Dialog open={createOpen} onOpenChange={o => { setCreateOpen(o); if (!o) setCreatedKey(null) }}>
        <DialogContent className="sm:max-w-md">
          {createdKey ? (
            <>
              <DialogHeader>
                <DialogTitle>{t('groups.createdTitle')}</DialogTitle>
                <DialogDescription>{t('groups.createdDesc')}</DialogDescription>
              </DialogHeader>
              <KeyBox
                title={createdKey.name ? t('groups.accessKeyTitle', { name: createdKey.name }) : t('groups.accessKeyFallback')}
                value={createdKey.key}
                hint={t('groups.keyHint')}
              />
              <DialogFooter>
                <Button onClick={() => { setCreateOpen(false); setCreatedKey(null) }}>{t('common.done')}</Button>
              </DialogFooter>
            </>
          ) : (
            <>
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
            </>
          )}
        </DialogContent>
      </Dialog>

      {/* —— 编辑（重命名） —— */}
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

      {/* —— 绑定账号 —— */}
      <Dialog open={!!bindTarget} onOpenChange={o => { if (!o && !bind.isPending) setBindTarget(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('groups.bindTitle', { name: bindTarget?.Name })}</DialogTitle>
            <DialogDescription>
              {t('groups.bindDesc', { count: bindChecked.length })}
            </DialogDescription>
          </DialogHeader>
          <div className="max-h-72 space-y-1 overflow-y-auto rounded-lg border p-2">
            {accounts.length === 0 ? (
              <p className="py-4 text-center text-sm text-muted-foreground">{t('groups.noAccounts')}</p>
            ) : (
              accounts.map(a => (
                <label
                  key={a.ID}
                  className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted"
                >
                  <Checkbox checked={bindChecked.includes(a.ID!)} onCheckedChange={() => toggleChecked(a.ID!)} />
                  <span className="flex-1 truncate text-sm">{a.Name}</span>
                  <span className="max-w-32 truncate text-xs text-muted-foreground">{a.Template?.Name ?? `#${a.TemplateID}`}</span>
                  <StatusBadge status={a.Status} />
                </label>
              ))
            )}
          </div>
          {bind.isError && errMsg(bind.error) && (
            <p className="text-sm text-destructive">{errMsg(bind.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setBindTarget(null)} disabled={bind.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => bind.mutate()} disabled={bind.isPending}>
              {bind.isPending ? t('common.saving') : t('groups.saveBind', { count: bindChecked.length })}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 轮换 key 确认 —— */}
      <Dialog open={!!rotateTarget} onOpenChange={o => { if (!o && !rotate.isPending) setRotateTarget(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('groups.rotateTitle')}</DialogTitle>
            <DialogDescription>
              {t('groups.rotateDesc', { name: rotateTarget?.Name })}
            </DialogDescription>
          </DialogHeader>
          {rotate.isError && errMsg(rotate.error) && (
            <p className="text-sm text-destructive">{errMsg(rotate.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setRotateTarget(null)} disabled={rotate.isPending}>{t('common.cancel')}</Button>
            <Button onClick={() => rotateTarget && rotate.mutate(rotateTarget.ID!)} disabled={rotate.isPending}>
              {rotate.isPending ? t('groups.rotating') : t('groups.confirmRotate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 轮换结果：明文 key —— */}
      <Dialog open={!!rotateResult} onOpenChange={o => { if (!o) setRotateResult(null) }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('groups.rotatedTitle')}</DialogTitle>
            <DialogDescription>{t('groups.rotatedDesc')}</DialogDescription>
          </DialogHeader>
          <KeyBox
            title={rotateResult?.name ? t('groups.newKeyTitle', { name: rotateResult.name }) : t('groups.newKeyFallback')}
            value={rotateResult?.key ?? ''}
            hint={t('groups.oldKeyHint')}
          />
          <DialogFooter>
            <Button onClick={() => setRotateResult(null)}>{t('common.done')}</Button>
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
            <Button variant="outline" onClick={closeBatchRename} disabled={batchRename.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitBatchRename} disabled={batchRename.isPending || !batchRenameValue.trim()}>
              {batchRename.isPending ? t('common.saving') : t('list.batchUpdate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

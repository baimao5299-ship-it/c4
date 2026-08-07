import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, Users, Ban, CircleCheck, Filter } from 'lucide-react'
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
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { StatusBadge } from '@/components/status-badge'
import { formatPercent, truncate } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type AccountView = components['schemas']['AccountView']
type AccountCreate = components['schemas']['AccountCreate']
type AccountPatch = components['schemas']['AccountPatch']
type AccountStatus = components['schemas']['AccountStatus']
// 批量更新表单里 status/template_id 的「不修改」哨兵值。
type BatchStatus = 'all' | AccountStatus

const LIMIT = 20
const STATUSES: AccountStatus[] = ['active', 'unhealthy', '429', 'disabled']

interface FormState {
  name: string
  template_id: string // Select 值统一用字符串，提交时转 number
  upstream_key: string
  status: AccountStatus
  weight: string
  max_concurrency: string
}

const emptyForm = (): FormState => ({
  name: '',
  template_id: '',
  upstream_key: '',
  status: 'active',
  weight: '0',
  max_concurrency: '8',
})

function toForm(a: AccountView): FormState {
  return {
    name: a.Name ?? '',
    template_id: String(a.TemplateID ?? ''),
    upstream_key: a.UpstreamKey ?? '',
    status: a.Status ?? 'active',
    weight: String(a.Weight ?? 0),
    max_concurrency: String(a.MaxConcurrency ?? 8),
  }
}

// PUT 全量替换：重建 AccountCreate（只带契约字段，不带运行时字段）。
function toBody(f: FormState): AccountCreate {
  return {
    name: f.name.trim(),
    template_id: Number(f.template_id),
    upstream_key: f.upstream_key,
    status: f.status,
    weight: f.weight === '' ? 0 : Number(f.weight),
    max_concurrency: f.max_concurrency === '' ? 8 : Number(f.max_concurrency),
  }
}

// 禁用/启用 quick action：取当前对象重建请求体 + status 翻转。
function toggleBody(a: AccountView, next: AccountStatus): AccountCreate {
  return {
    name: a.Name ?? '',
    template_id: a.TemplateID ?? 0,
    upstream_key: a.UpstreamKey ?? '',
    status: next,
    weight: a.Weight ?? 0,
    max_concurrency: a.MaxConcurrency ?? 8,
  }
}

// 批量更新表单：空字段 = 不发送（保持原值）。
interface BatchForm {
  name: string
  upstream_key: string
  status: BatchStatus
  weight: string
  max_concurrency: string
  template_id: string
}

const emptyBatchForm = (): BatchForm => ({
  name: '',
  upstream_key: '',
  status: 'all',
  weight: '',
  max_concurrency: '',
  template_id: 'all',
})

export default function Accounts() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：筛选/分页状态归 queryKey ——
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [offset, setOffset] = useState(0)
  const [statusFilter, setStatusFilter] = useState<AccountStatus[]>([])
  const [templateId, setTemplateId] = useState('all') // 'all' = 全部模板

  const { data, isLoading, isError, error } = useQuery({
    queryKey: [
      'accounts',
      { limit: LIMIT, offset, name, sort: activeSort ?? 'id', order, status: statusFilter.join(','), template_id: templateId === 'all' ? undefined : Number(templateId) },
    ],
    queryFn: () =>
      api.listAccounts({
        limit: LIMIT,
        offset,
        name: name || undefined,
        sort: activeSort ?? 'id',
        order,
        status: statusFilter.length > 0 ? statusFilter.join(',') : undefined,
        template_id: templateId === 'all' ? undefined : Number(templateId),
      }),
    refetchInterval: 10_000,
  })
  const templatesQ = useQuery({ queryKey: ['templates'], queryFn: () => api.listTemplates({ limit: 100 }) })
  const templates = templatesQ.data?.rows ?? []
  const rows = data?.rows ?? []

  // —— 行勾选（跨页保留，筛选/翻页后清空）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID!)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  // 筛选/翻页变化 → 回第一页 + 清勾选。
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
  const toggleStatusFilter = (s: AccountStatus) => {
    setStatusFilter(cur => (cur.includes(s) ? cur.filter(x => x !== s) : [...cur, s]))
    resetPage()
  }
  const changeTemplate = (v: string) => { setTemplateId(v); resetPage() }
  const hasFilters = name !== '' || statusFilter.length > 0 || templateId !== 'all'
  const clearFilters = () => {
    setName('')
    setStatusFilter([])
    setTemplateId('all')
    resetPage()
  }

  // —— 批量删除/更新 ——
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteAccountsBatch(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setSelected([])
    },
  })
  const batchUpdate = useMutation({
    mutationFn: (p: { ids: number[]; fields: AccountPatch }) => api.updateAccountsBatch(p.ids, p.fields),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setSelected([])
      closeBatchUpdate()
    },
  })
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const [batchUpdateOpen, setBatchUpdateOpen] = useState(false)
  const [batchForm, setBatchForm] = useState<BatchForm>(emptyBatchForm())
  const [batchFormErr, setBatchFormErr] = useState<string | null>(null)
  const batchResolve = useRef<(() => void) | null>(null)
  const closeBatchUpdate = () => {
    setBatchUpdateOpen(false)
    batchResolve.current?.()
    batchResolve.current = null
  }
  const openBatchUpdate = () => {
    setBatchForm(emptyBatchForm())
    setBatchFormErr(null)
    setBatchUpdateOpen(true)
  }
  const submitBatchUpdate = () => {
    const fields: AccountPatch = {}
    if (batchForm.name.trim()) fields.name = batchForm.name.trim()
    if (batchForm.upstream_key) fields.upstream_key = batchForm.upstream_key
    if (batchForm.status !== 'all') fields.status = batchForm.status
    if (batchForm.weight !== '') fields.weight = Number(batchForm.weight)
    if (batchForm.max_concurrency !== '') fields.max_concurrency = Number(batchForm.max_concurrency)
    if (batchForm.template_id !== 'all') fields.template_id = Number(batchForm.template_id)
    if (Object.keys(fields).length === 0) {
      setBatchFormErr(t('accounts.batchUpdateEmpty'))
      return
    }
    batchUpdate.mutate({ ids: selected, fields })
  }

  // —— 单行 创建/编辑/删除 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<AccountView | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [deleting, setDeleting] = useState<AccountView | null>(null)

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setDialogOpen(true)
  }
  const openEdit = (a: AccountView) => {
    setEditing(a)
    setForm(toForm(a))
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: (f: FormState) =>
      editing ? api.updateAccount(editing.ID!, toBody(f)) : api.createAccount(toBody(f)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setDialogOpen(false)
    },
  })
  const toggle = useMutation({
    mutationFn: (a: AccountView) =>
      api.updateAccount(a.ID!, toggleBody(a, a.Status === 'disabled' ? 'active' : 'disabled')),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['accounts'] }),
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteAccount(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      setDeleting(null)
    },
  })

  const submit = () => {
    if (!form.name.trim() || !form.template_id || !form.upstream_key) return
    save.mutate(form)
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const templateName = (a: AccountView) => a.Template?.Name ?? (a.TemplateID ? `#${a.TemplateID}` : '—')

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{t('accounts.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('accounts.subtitle')}</p>
        </div>
        <Button onClick={openCreate} disabled={templates.length === 0} title={templates.length === 0 ? t('accounts.noTemplate') : undefined}>
          <Plus /> {t('accounts.new')}
        </Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={changeName}
      >
        {/* status 多选筛选（逗号拼接传参） */}
        <Popover>
          <PopoverTrigger render={<Button variant="outline" size="lg" />}>
            <Filter />
            {statusFilter.length > 0
              ? statusFilter.map(s => t(`status.${s}`)).join(', ')
              : t('accounts.filterStatus')}
          </PopoverTrigger>
          <PopoverContent className="w-48 p-2">
            <div className="space-y-0.5">
              {STATUSES.map(s => (
                <label key={s} className="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 hover:bg-muted">
                  <Checkbox checked={statusFilter.includes(s)} onCheckedChange={() => toggleStatusFilter(s)} />
                  <span className="text-sm">{t(`status.${s}`)}</span>
                </label>
              ))}
            </div>
            {statusFilter.length > 0 && (
              <Button variant="ghost" size="sm" className="mt-1 w-full" onClick={clearFilters}>
                {t('list.reset')}
              </Button>
            )}
          </PopoverContent>
        </Popover>
        {/* template 精确筛选 */}
        <Select value={templateId} onValueChange={changeTemplate}>
          <SelectTrigger size="default" className="w-44 data-[size=default]:h-9" aria-label={t('accounts.filterTemplate')}>
            <SelectValue placeholder={t('accounts.filterTemplate')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all" label={t('accounts.allTemplates')}>{t('accounts.allTemplates')}</SelectItem>
            {templates.map(tp => (
              <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>
            ))}
          </SelectContent>
        </Select>
      </ListToolbar>

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
        onUpdate={() => new Promise<void>(resolve => {
          batchResolve.current = resolve
          openBatchUpdate()
        })}
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
            <Users className="size-10" />
            <p className="font-medium">{hasFilters ? t('accounts.filterEmpty') : t('accounts.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('accounts.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate} disabled={templates.length === 0}><Plus /> {t('accounts.new')}</Button>
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
                  <SortableHeader field="name" label={t('accounts.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="template_id" label={t('accounts.table.template')} active={activeSort === 'template_id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="status" label={t('accounts.table.status')} active={activeSort === 'status'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="weight" label={t('accounts.table.weight')} active={activeSort === 'weight'} order={order} onToggle={onColumnToggle} className="text-right [&_button]:justify-end" />
                  <SortableHeader field="max_concurrency" label={t('accounts.table.maxConcurrency')} active={activeSort === 'max_concurrency'} order={order} onToggle={onColumnToggle} className="text-right [&_button]:justify-end" />
                  <TableHead className="text-right">{t('accounts.table.curConcurrency')}</TableHead>
                  <TableHead className="text-right">{t('accounts.table.errRate')}</TableHead>
                  <TableHead className="text-right">{t('accounts.table.errCount')}</TableHead>
                  <TableHead>{t('accounts.table.lastError')}</TableHead>
                  <TableHead className="text-right">{t('accounts.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(a => (
                  <TableRow key={a.ID} data-state={selected.includes(a.ID!) ? 'selected' : undefined}>
                    <TableCell>
                      <Checkbox checked={selected.includes(a.ID!)} onCheckedChange={() => toggleRow(a.ID!)} />
                    </TableCell>
                    <TableCell className="tabular-nums">{a.ID}</TableCell>
                    <TableCell className="max-w-32 truncate" title={a.Name}>{a.Name}</TableCell>
                    <TableCell className="max-w-32 truncate" title={templateName(a)}>{templateName(a)}</TableCell>
                    <TableCell><StatusBadge status={a.Status} /></TableCell>
                    <TableCell className="text-right tabular-nums">{a.Weight ?? 0}</TableCell>
                    <TableCell className="text-right tabular-nums">{a.MaxConcurrency ?? 8}</TableCell>
                    <TableCell className="text-right tabular-nums">{a.concurrency ?? 0}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPercent(a.err_rate)}</TableCell>
                    <TableCell className="text-right tabular-nums">{a.err_count ?? 0}</TableCell>
                    <TableCell className="max-w-40">
                      {a.LastError ? (
                        <Tooltip>
                          <TooltipTrigger render={<span className="block cursor-help truncate text-xs text-muted-foreground" />}>
                            {truncate(a.LastError, 20)}
                          </TooltipTrigger>
                          <TooltipContent>{a.LastError}</TooltipContent>
                        </Tooltip>
                      ) : (
                        <span className="text-xs text-muted-foreground">—</span>
                      )}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          title={a.Status === 'disabled' ? t('accounts.enable') : t('accounts.disable')}
                          onClick={() => toggle.mutate(a)}
                          disabled={toggle.isPending}
                        >
                          {a.Status === 'disabled' ? <CircleCheck /> : <Ban />}
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(a)}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(a)}><Trash2 /></Button>
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

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{editing ? t('accounts.editTitle', { id: editing.ID }) : t('accounts.newTitle')}</DialogTitle>
            <DialogDescription>{t('accounts.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="acc-name">{t('accounts.nameLabel')}</Label>
              <Input id="acc-name" value={form.name} placeholder={t('accounts.namePlaceholder')} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label>{t('accounts.templateLabel')}</Label>
              <Select
                items={Object.fromEntries(templates.map(tp => [String(tp.ID), tp.Name ?? `#${tp.ID}`]))}
                value={form.template_id || null}
                onValueChange={v => setForm(f => ({ ...f, template_id: String(v) }))}
              >
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {templates.map(tp => <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="acc-key">Upstream Key</Label>
              <Input id="acc-key" type="password" value={form.upstream_key} placeholder="sk-..." onChange={e => setForm(f => ({ ...f, upstream_key: e.target.value }))} />
            </div>
            <div className="grid grid-cols-3 gap-3">
              <div className="space-y-1.5">
                <Label>{t('accounts.statusLabel')}</Label>
                <Select
                  items={Object.fromEntries(STATUSES.map(s => [s, t(`status.${s}`)]))}
                  value={form.status}
                  onValueChange={v => setForm(f => ({ ...f, status: v as AccountStatus }))}
                >
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="acc-weight">{t('accounts.weightLabel')}</Label>
                <Input id="acc-weight" type="number" min={0} value={form.weight} onChange={e => setForm(f => ({ ...f, weight: e.target.value }))} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="acc-max">{t('accounts.maxLabel')}</Label>
                <Input id="acc-max" type="number" min={1} value={form.max_concurrency} onChange={e => setForm(f => ({ ...f, max_concurrency: e.target.value }))} />
              </div>
            </div>
            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending || !form.name.trim() || !form.template_id || !form.upstream_key}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认（单行） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('accounts.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('accounts.deleteDesc', { name: deleting?.Name })}
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

      {/* —— 批量更新对话框：AccountPatch 字段子集（空 = 保持原值） —— */}
      <Dialog open={batchUpdateOpen} onOpenChange={o => { if (!o) closeBatchUpdate() }}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t('accounts.batchUpdateTitle')}</DialogTitle>
            <DialogDescription>{t('accounts.batchUpdateDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="ba-name">{t('accounts.nameLabel')}</Label>
              <Input id="ba-name" value={batchForm.name} placeholder={t('accounts.namePlaceholder')} onChange={e => setBatchForm(f => ({ ...f, name: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="ba-key">Upstream Key</Label>
              <Input id="ba-key" type="password" value={batchForm.upstream_key} placeholder="sk-..." onChange={e => setBatchForm(f => ({ ...f, upstream_key: e.target.value }))} />
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label>{t('accounts.statusLabel')}</Label>
                <Select value={batchForm.status} onValueChange={v => setBatchForm(f => ({ ...f, status: v as BatchStatus }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all" label={t('list.unchanged')}>{t('list.unchanged')}</SelectItem>
                    {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-1.5">
                <Label>{t('accounts.templateLabel')}</Label>
                <Select value={batchForm.template_id} onValueChange={v => setBatchForm(f => ({ ...f, template_id: v }))}>
                  <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                  <SelectContent>
                    <SelectItem value="all" label={t('list.unchanged')}>{t('list.unchanged')}</SelectItem>
                    {templates.map(tp => <SelectItem key={tp.ID} value={String(tp.ID)} label={tp.Name ?? `#${tp.ID}`}>{tp.Name ?? `#${tp.ID}`}</SelectItem>)}
                  </SelectContent>
                </Select>
              </div>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <Label htmlFor="ba-weight">{t('accounts.weightLabel')}</Label>
                <Input id="ba-weight" type="number" min={0} value={batchForm.weight} placeholder="0" onChange={e => setBatchForm(f => ({ ...f, weight: e.target.value }))} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="ba-max">{t('accounts.maxLabel')}</Label>
                <Input id="ba-max" type="number" min={1} value={batchForm.max_concurrency} placeholder="8" onChange={e => setBatchForm(f => ({ ...f, max_concurrency: e.target.value }))} />
              </div>
            </div>
            {batchFormErr && <p className="text-sm text-destructive">{batchFormErr}</p>}
            {batchUpdate.isError && errMsg(batchUpdate.error) && (
              <p className="text-sm text-destructive">{errMsg(batchUpdate.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={closeBatchUpdate} disabled={batchUpdate.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submitBatchUpdate} disabled={batchUpdate.isPending}>
              {batchUpdate.isPending ? t('common.saving') : t('list.batchUpdate')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

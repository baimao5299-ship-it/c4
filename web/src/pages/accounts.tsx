import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, Users, Ban, CircleCheck } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { StatusBadge } from '@/components/status-badge'
import { formatPercent, truncate } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type AccountView = components['schemas']['AccountView']
type AccountCreate = components['schemas']['AccountCreate']
type AccountStatus = components['schemas']['AccountStatus']

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

export default function Accounts() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  // 运行时视图 10s 轮询。
  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['accounts'],
    queryFn: () => api.listAccounts(),
    refetchInterval: 10_000,
  })
  const templatesQ = useQuery({ queryKey: ['templates'], queryFn: () => api.listTemplates() })
  const templates = templatesQ.data?.rows ?? []

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

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : (data?.rows ?? []).length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Users className="size-10" />
            <p className="font-medium">{t('accounts.emptyTitle')}</p>
            <p className="text-sm">{t('accounts.emptyDesc')}</p>
            <Button className="mt-2" onClick={openCreate} disabled={templates.length === 0}><Plus /> {t('accounts.new')}</Button>
          </Card>
        </motion.div>
      ) : (
        <Card className="overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>{t('accounts.table.name')}</TableHead>
                <TableHead>{t('accounts.table.template')}</TableHead>
                <TableHead>{t('accounts.table.status')}</TableHead>
                <TableHead className="text-right">{t('accounts.table.weight')}</TableHead>
                <TableHead className="text-right">{t('accounts.table.maxConcurrency')}</TableHead>
                <TableHead className="text-right">{t('accounts.table.curConcurrency')}</TableHead>
                <TableHead className="text-right">{t('accounts.table.errRate')}</TableHead>
                <TableHead className="text-right">{t('accounts.table.errCount')}</TableHead>
                <TableHead>{t('accounts.table.lastError')}</TableHead>
                <TableHead className="text-right">{t('accounts.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data?.rows ?? []).map(a => (
                <TableRow key={a.ID}>
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
        </Card>
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
                items={Object.fromEntries(templates.map(t => [String(t.ID), t.Name ?? `#${t.ID}`]))}
                value={form.template_id || null}
                onValueChange={v => setForm(f => ({ ...f, template_id: String(v) }))}
              >
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {templates.map(t => <SelectItem key={t.ID} value={String(t.ID)}>{t.Name ?? `#${t.ID}`}</SelectItem>)}
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
                    {STATUSES.map(s => <SelectItem key={s} value={s}>{t(`status.${s}`)}</SelectItem>)}
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

      {/* —— 删除确认 —— */}
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
    </div>
  )
}

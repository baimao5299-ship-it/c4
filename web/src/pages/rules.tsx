import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, ScrollText, Ban, CircleCheck } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { BatchBar } from '@/components/batch-bar'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import type { components } from '@/lib/api/schema'

type Rule = components['schemas']['Rule']
type RuleCreate = components['schemas']['RuleCreate']
type AccountStatus = components['schemas']['AccountStatus']

// 规则事件类型（spec：ok | 429 | error）。
const KINDS = ['ok', '429', 'error'] as const
// then.status 可选值（空 = 不设置状态）。
const STATUSES: AccountStatus[] = ['active', 'unhealthy', '429', 'disabled']

// —— 结构化 when/then 表单：空字符串 = 字段不发送 ——
interface WhenForm {
  kind: string
  http_status: string
  error_message_contains: string
  account_id: string
  template_id: string
  group_id: string
  model: string
  window_seconds: string
  count_429_ge: string
  count_error_ge: string
  count_ok_ge: string
  count_total_ge: string
  ratio_429_ge: string
  ratio_error_ge: string
}
interface ThenForm {
  status: string
  cooldown: string
  weight: string
}
interface FormState {
  name: string
  priority: string
  enabled: boolean
  when: WhenForm
  then: ThenForm
}

const emptyWhen = (): WhenForm => ({
  kind: '', http_status: '', error_message_contains: '', account_id: '', template_id: '',
  group_id: '', model: '', window_seconds: '', count_429_ge: '', count_error_ge: '',
  count_ok_ge: '', count_total_ge: '', ratio_429_ge: '', ratio_error_ge: '',
})
const emptyThen = (): ThenForm => ({ status: '', cooldown: '', weight: '' })
const emptyForm = (): FormState => ({ name: '', priority: '', enabled: true, when: emptyWhen(), then: emptyThen() })

// 数字字段：空/NaN → 不发送；其他字符串化。
function num(s: string): number | undefined {
  if (s === '') return undefined
  const n = Number(s)
  return Number.isNaN(n) ? undefined : n
}

// Rule.When（[key: string]: unknown）→ 表单（未知键忽略，编辑往返保留全部已知字段）。
function whenToForm(w: Rule['When']): WhenForm {
  const f = emptyWhen()
  for (const k of Object.keys(f) as (keyof WhenForm)[]) {
    const v = w[k]
    if (v !== undefined && v !== null) f[k] = String(v)
  }
  return f
}
function thenToForm(th: Rule['Then']): ThenForm {
  const f = emptyThen()
  if (typeof th.status === 'string') f.status = th.status
  if (typeof th.cooldown === 'string') f.cooldown = th.cooldown
  if (th.weight !== undefined && th.weight !== null) f.weight = String(th.weight)
  return f
}
function toForm(r: Rule): FormState {
  return {
    name: r.Name,
    priority: String(r.Priority),
    enabled: r.Enabled,
    when: whenToForm(r.When ?? {}),
    then: thenToForm(r.Then ?? {}),
  }
}

// 表单 → 请求体：仅发送非空字段（后端白名单校验，未知键 400）。
function toWhen(f: WhenForm): Record<string, unknown> {
  const w: Record<string, unknown> = {}
  if (f.kind) w.kind = f.kind
  for (const [k, v] of [
    ['http_status', num(f.http_status)], ['account_id', num(f.account_id)],
    ['template_id', num(f.template_id)], ['group_id', num(f.group_id)],
    ['window_seconds', num(f.window_seconds)], ['count_429_ge', num(f.count_429_ge)],
    ['count_error_ge', num(f.count_error_ge)], ['count_ok_ge', num(f.count_ok_ge)],
    ['count_total_ge', num(f.count_total_ge)], ['ratio_429_ge', num(f.ratio_429_ge)],
    ['ratio_error_ge', num(f.ratio_error_ge)],
  ] as const) {
    if (v !== undefined) w[k] = v
  }
  if (f.error_message_contains) w.error_message_contains = f.error_message_contains
  if (f.model) w.model = f.model
  return w
}
function toThen(f: ThenForm): Record<string, unknown> {
  const th: Record<string, unknown> = {}
  if (f.status) th.status = f.status
  if (f.cooldown) th.cooldown = f.cooldown
  const w = num(f.weight)
  if (w !== undefined) th.weight = w
  return th
}
function toBody(f: FormState): RuleCreate {
  return { name: f.name.trim(), priority: Number(f.priority), enabled: f.enabled, when: toWhen(f.when), then: toThen(f.then) }
}

// —— 摘要渲染 ——
function WhenSummary({ w, t }: { w: Rule['When']; t: (k: string) => string }) {
  if (!w || Object.keys(w).length === 0) return <span className="text-muted-foreground">—</span>
  const parts: string[] = []
  if (typeof w.kind === 'string') parts.push(t(`rules.kind.${w.kind}`))
  if (typeof w.http_status === 'number') parts.push(`HTTP ${w.http_status}`)
  if (typeof w.error_message_contains === 'string') parts.push(t('rules.when.errorContains') + ` "${w.error_message_contains}"`)
  if (typeof w.account_id === 'number') parts.push(`acc#${w.account_id}`)
  if (typeof w.template_id === 'number') parts.push(`tpl#${w.template_id}`)
  if (typeof w.group_id === 'number') parts.push(`grp#${w.group_id}`)
  if (typeof w.model === 'string') parts.push(w.model)
  if (typeof w.window_seconds === 'number') parts.push(`${w.window_seconds}s`)
  if (typeof w.count_429_ge === 'number') parts.push(`429≥${w.count_429_ge}`)
  if (typeof w.count_error_ge === 'number') parts.push(`err≥${w.count_error_ge}`)
  if (typeof w.count_ok_ge === 'number') parts.push(`ok≥${w.count_ok_ge}`)
  if (typeof w.count_total_ge === 'number') parts.push(`total≥${w.count_total_ge}`)
  if (typeof w.ratio_429_ge === 'number') parts.push(`429率≥${w.ratio_429_ge}`)
  if (typeof w.ratio_error_ge === 'number') parts.push(`err率≥${w.ratio_error_ge}`)
  return <span className="block max-w-64 truncate text-xs" title={parts.join(' · ')}>{parts.join(' · ') || '—'}</span>
}

function ThenSummary({ th }: { th: Rule['Then'] }) {
  if (!th || Object.keys(th).length === 0) return <span className="text-muted-foreground">—</span>
  const parts: string[] = []
  if (typeof th.status === 'string') parts.push(`→${th.status}`)
  if (typeof th.cooldown === 'string') parts.push(`⏱${th.cooldown}`)
  if (typeof th.weight === 'number') parts.push(`w=${th.weight}`)
  return <span className="block max-w-40 truncate text-xs" title={parts.join(' · ')}>{parts.join(' · ') || '—'}</span>
}

export default function Rules() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['rules'],
    queryFn: () => api.listRules({}),
    refetchInterval: 10_000,
  })
  const rows = data?.rows ?? []

  // —— 行勾选（规则表全量无分页，pageIds = 全部行）——
  const [selected, setSelected] = useState<number[]>([])
  const pageIds = rows.map(r => r.ID)
  const allChecked = rows.length > 0 && pageIds.every(id => selected.includes(id))
  const someChecked = pageIds.some(id => selected.includes(id))
  const toggleRow = (id: number) => setSelected(s => (s.includes(id) ? s.filter(x => x !== id) : [...s, id]))
  const toggleAll = (c: boolean) =>
    setSelected(s => (c ? Array.from(new Set([...s, ...pageIds])) : s.filter(x => !pageIds.includes(x))))

  // —— 创建/编辑/删除/批量删除 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Rule | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [deleting, setDeleting] = useState<Rule | null>(null)

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setDialogOpen(true)
  }
  const openEdit = (r: Rule) => {
    setEditing(r)
    setForm(toForm(r))
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: (f: FormState) =>
      editing ? api.updateRule(editing.ID, toBody(f)) : api.createRule(toBody(f)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rules'] })
      setDialogOpen(false)
    },
  })
  const toggle = useMutation({
    mutationFn: (r: Rule) =>
      api.updateRule(r.ID, { enabled: !r.Enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['rules'] }),
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteRule(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rules'] })
      setDeleting(null)
    },
  })
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteRulesBatch(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['rules'] })
      setSelected([])
    },
  })

  const submit = () => {
    if (!form.name.trim() || form.priority === '') return
    save.mutate(form)
  }
  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const setWhen = (k: keyof WhenForm, v: string) => setForm(f => ({ ...f, when: { ...f.when, [k]: v } }))
  const setThen = (k: keyof ThenForm, v: string) => setForm(f => ({ ...f, then: { ...f.then, [k]: v } }))

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{t('rules.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('rules.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {t('rules.new')}</Button>
      </div>

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={async () => {
          await batchDelete.mutateAsync(selected)
        }}
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
            <ScrollText className="size-10" />
            <p className="font-medium">{t('rules.emptyTitle')}</p>
            <p className="text-sm">{t('rules.emptyDesc')}</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> {t('rules.new')}</Button>
          </Card>
        </motion.div>
      ) : (
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
                <TableHead className="w-14">ID</TableHead>
                <TableHead>{t('rules.table.name')}</TableHead>
                <TableHead className="w-16 text-right">{t('rules.table.priority')}</TableHead>
                <TableHead className="w-20">{t('rules.table.enabled')}</TableHead>
                <TableHead className="w-64">{t('rules.table.when')}</TableHead>
                <TableHead className="w-44">{t('rules.table.then')}</TableHead>
                <TableHead className="w-24 text-right">{t('rules.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map(r => (
                <TableRow key={r.ID} data-state={selected.includes(r.ID) ? 'selected' : undefined}>
                  <TableCell>
                    <Checkbox checked={selected.includes(r.ID)} onCheckedChange={() => toggleRow(r.ID)} />
                  </TableCell>
                  <TableCell className="tabular-nums">{r.ID}</TableCell>
                  <TableCell className="max-w-40 truncate" title={r.Name}>{r.Name}</TableCell>
                  <TableCell className="text-right tabular-nums">{r.Priority}</TableCell>
                  <TableCell>
                    <Button
                      variant="ghost"
                      size="icon-sm"
                      title={r.Enabled ? t('rules.disable') : t('rules.enable')}
                      onClick={() => toggle.mutate(r)}
                      disabled={toggle.isPending}
                    >
                      {r.Enabled ? <CircleCheck className="text-emerald-500" /> : <Ban />}
                    </Button>
                  </TableCell>
                  <TableCell><WhenSummary w={r.When} t={t} /></TableCell>
                  <TableCell><ThenSummary th={r.Then} /></TableCell>
                  <TableCell className="text-right">
                    <div className="flex justify-end gap-1">
                      <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(r)}><Pencil /></Button>
                      <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => setDeleting(r)}><Trash2 /></Button>
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editing ? t('rules.editTitle', { id: editing.ID }) : t('rules.newTitle')}</DialogTitle>
            <DialogDescription>{t('rules.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="max-h-[70vh] space-y-4 overflow-y-auto pr-1">
            {/* 基础 */}
            <div className="grid grid-cols-3 gap-3">
              <div className="col-span-2 space-y-1.5">
                <Label htmlFor="rl-name">{t('rules.nameLabel')}</Label>
                <Input id="rl-name" value={form.name} placeholder={t('rules.namePlaceholder')} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="rl-priority">{t('rules.priorityLabel')}</Label>
                <Input id="rl-priority" type="number" min={0} value={form.priority} placeholder="10" onChange={e => setForm(f => ({ ...f, priority: e.target.value }))} />
              </div>
            </div>
            <label className="flex cursor-pointer items-center gap-2 text-sm">
              <Checkbox checked={form.enabled} onCheckedChange={c => setForm(f => ({ ...f, enabled: c === true }))} />
              {t('rules.enabledLabel')}
            </label>

            {/* 匹配 when（全部可选，未填不限定） */}
            <div className="space-y-2">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('rules.whenTitle')}</p>
              <div className="grid grid-cols-3 gap-3">
                <div className="space-y-1.5">
                  <Label>{t('rules.when.kind')}</Label>
                  <Select
                    items={Object.fromEntries([['', t('rules.any')], ...KINDS.map(k => [k, t(`rules.kind.${k}`)])])}
                    value={form.when.kind || null}
                    onValueChange={v => setWhen('kind', v)}
                  >
                    <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="" label={t('rules.any')}>{t('rules.any')}</SelectItem>
                      {KINDS.map(k => <SelectItem key={k} value={k} label={t(`rules.kind.${k}`)}>{t(`rules.kind.${k}`)}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-http">{t('rules.when.httpStatus')}</Label>
                  <Input id="rl-http" type="number" min={100} max={599} placeholder="503" value={form.when.http_status} onChange={e => setWhen('http_status', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-model">{t('rules.when.model')}</Label>
                  <Input id="rl-model" placeholder="gpt-4o" value={form.when.model} onChange={e => setWhen('model', e.target.value)} />
                </div>
                <div className="col-span-3 space-y-1.5">
                  <Label htmlFor="rl-errmsg">{t('rules.when.errorContains')}</Label>
                  <Input id="rl-errmsg" placeholder="unhealthy" value={form.when.error_message_contains} onChange={e => setWhen('error_message_contains', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-acc">{t('rules.when.accountId')}</Label>
                  <Input id="rl-acc" type="number" placeholder="12" value={form.when.account_id} onChange={e => setWhen('account_id', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-tpl">{t('rules.when.templateId')}</Label>
                  <Input id="rl-tpl" type="number" placeholder="3" value={form.when.template_id} onChange={e => setWhen('template_id', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-grp">{t('rules.when.groupId')}</Label>
                  <Input id="rl-grp" type="number" placeholder="1" value={form.when.group_id} onChange={e => setWhen('group_id', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-win">{t('rules.when.windowSeconds')}</Label>
                  <Input id="rl-win" type="number" min={1} placeholder="60" value={form.when.window_seconds} onChange={e => setWhen('window_seconds', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-c429">{t('rules.when.count429')}</Label>
                  <Input id="rl-c429" type="number" min={0} placeholder="3" value={form.when.count_429_ge} onChange={e => setWhen('count_429_ge', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-cerr">{t('rules.when.countError')}</Label>
                  <Input id="rl-cerr" type="number" min={0} placeholder="5" value={form.when.count_error_ge} onChange={e => setWhen('count_error_ge', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-cok">{t('rules.when.countOK')}</Label>
                  <Input id="rl-cok" type="number" min={0} placeholder="1" value={form.when.count_ok_ge} onChange={e => setWhen('count_ok_ge', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-ctot">{t('rules.when.countTotal')}</Label>
                  <Input id="rl-ctot" type="number" min={1} placeholder="10" value={form.when.count_total_ge} onChange={e => setWhen('count_total_ge', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-r429">{t('rules.when.ratio429')}</Label>
                  <Input id="rl-r429" type="number" min={0} max={1} step={0.01} placeholder="0.5" value={form.when.ratio_429_ge} onChange={e => setWhen('ratio_429_ge', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-rerr">{t('rules.when.ratioError')}</Label>
                  <Input id="rl-rerr" type="number" min={0} max={1} step={0.01} placeholder="0.8" value={form.when.ratio_error_ge} onChange={e => setWhen('ratio_error_ge', e.target.value)} />
                </div>
              </div>
              <p className="text-xs text-muted-foreground">{t('rules.whenHint')}</p>
            </div>

            {/* 动作 then（可选组合） */}
            <div className="space-y-2">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{t('rules.thenTitle')}</p>
              <div className="grid grid-cols-3 gap-3">
                <div className="space-y-1.5">
                  <Label>{t('rules.then.status')}</Label>
                  <Select
                    items={Object.fromEntries([['', t('rules.any')], ...STATUSES.map(s => [s, t(`status.${s}`)])])}
                    value={form.then.status || null}
                    onValueChange={v => setThen('status', v)}
                  >
                    <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="" label={t('rules.any')}>{t('rules.any')}</SelectItem>
                      {STATUSES.map(s => <SelectItem key={s} value={s} label={t(`status.${s}`)}>{t(`status.${s}`)}</SelectItem>)}
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-cd">{t('rules.then.cooldown')}</Label>
                  <Input id="rl-cd" placeholder="30s / 5m / 1h" value={form.then.cooldown} onChange={e => setThen('cooldown', e.target.value)} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="rl-w">{t('rules.then.weight')}</Label>
                  <Input id="rl-w" type="number" min={0} max={100} placeholder="0" value={form.then.weight} onChange={e => setThen('weight', e.target.value)} />
                </div>
              </div>
              <p className="text-xs text-muted-foreground">{t('rules.thenHint')}</p>
            </div>

            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending || !form.name.trim() || form.priority === ''}>
              {save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('rules.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {t('rules.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID)} disabled={remove.isPending}>
              {remove.isPending ? t('common.deleting') : t('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

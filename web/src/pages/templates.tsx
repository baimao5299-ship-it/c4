import { useEffect, useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Boxes, Pencil, Plus, Trash2, X } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import type { components } from '@/lib/api/schema'
import { BatchBar } from '@/components/batch-bar'
import { commaList, formatDateTime, truncate } from '@/components/fmt'
import { ListToolbar } from '@/components/list-toolbar'
import { Pagination } from '@/components/pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

type Template = components['schemas']['Template']
type TemplateCreate = components['schemas']['TemplateCreate']
type TemplatePatch = components['schemas']['TemplatePatch']
type RequestFormat = components['schemas']['RequestFormat']

const FORMAT_LABELS: Record<RequestFormat, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  anthropic: 'Anthropic',
}
const FORMATS = Object.keys(FORMAT_LABELS) as RequestFormat[]

const LIMIT = 20

// —— 多格式表单状态（supported_formats/format_models；default_format/model_formats 已废弃） ——
interface FormatRow {
  format: RequestFormat
  modelsText: string
}
interface MappingRow {
  key: string
  value: string
}
interface FormState {
  name: string
  base_url: string
  supported_formats: RequestFormat[]
  modelsText: string
  format_models: FormatRow[]
  model_mapping: MappingRow[]
}

const emptyForm = (): FormState => ({
  name: '',
  base_url: '',
  supported_formats: [],
  modelsText: '',
  format_models: [],
  model_mapping: [],
})

function toForm(t: Template): FormState {
  return {
    name: t.Name ?? '',
    base_url: t.BaseURL ?? '',
    supported_formats: [...(t.SupportedFormats ?? [])],
    modelsText: (t.Models ?? []).join(', '),
    format_models: Object.entries(t.FormatModels ?? {}).map(([format, models]) => ({
      format: format as RequestFormat,
      modelsText: (models ?? []).join(', '),
    })),
    model_mapping: Object.entries(t.ModelMapping ?? {}).map(([key, value]) => ({ key, value })),
  }
}

const splitList = (s: string) => s.split(',').map(x => x.trim()).filter(Boolean)

function formatModelsOf(f: FormState): Record<string, string[]> {
  const out: Record<string, string[]> = {}
  for (const r of f.format_models) {
    const models = splitList(r.modelsText)
    if (r.format && models.length) out[r.format] = models
  }
  return out
}

function mappingOf(f: FormState): Record<string, string> {
  const out: Record<string, string> = {}
  for (const r of f.model_mapping) if (r.key.trim() && r.value.trim()) out[r.key.trim()] = r.value.trim()
  return out
}

function toBody(f: FormState): TemplateCreate {
  const format_models = formatModelsOf(f)
  const model_mapping = mappingOf(f)
  return {
    name: f.name.trim(),
    base_url: f.base_url.trim(),
    supported_formats: f.supported_formats,
    models: splitList(f.modelsText),
    format_models: Object.keys(format_models).length ? format_models : undefined,
    model_mapping: Object.keys(model_mapping).length ? model_mapping : undefined,
  }
}

// 批量更新：仅包含已填写的字段（TemplatePatch 子集）
function toPatch(f: FormState): TemplatePatch {
  const patch: TemplatePatch = {}
  if (f.name.trim()) patch.name = f.name.trim()
  if (f.base_url.trim()) patch.base_url = f.base_url.trim()
  if (f.supported_formats.length) patch.supported_formats = f.supported_formats
  const models = splitList(f.modelsText)
  if (models.length) patch.models = models
  const format_models = formatModelsOf(f)
  if (Object.keys(format_models).length) patch.format_models = format_models
  const model_mapping = mappingOf(f)
  if (Object.keys(model_mapping).length) patch.model_mapping = model_mapping
  return patch
}

function FormatBadge({ format }: { format?: string }) {
  return <Badge variant="outline">{format ? (FORMAT_LABELS[format as RequestFormat] ?? format) : '—'}</Badge>
}

// —— 表单区（创建/编辑与批量更新共用；batch 模式下所有字段可选） ——
function FormFields({
  form,
  setForm,
  error,
}: {
  form: FormState
  setForm: (updater: (f: FormState) => FormState) => void
  error?: string | null
}) {
  const { t } = useTranslation()

  const toggleFormat = (f: RequestFormat) => {
    setForm(prev => {
      const on = prev.supported_formats.includes(f)
      const supported_formats = on ? prev.supported_formats.filter(x => x !== f) : [...prev.supported_formats, f]
      // 取消勾选的格式同步移除其 format_models 行（后端要求 key ∈ supported_formats）
      const format_models = on ? prev.format_models.filter(r => r.format !== f) : prev.format_models
      return { ...prev, supported_formats, format_models }
    })
  }
  const setFormatRow = (i: number, patch: Partial<FormatRow>) =>
    setForm(f => ({ ...f, format_models: f.format_models.map((r, j) => (j === i ? { ...r, ...patch } : r)) }))
  const removeFormatRow = (i: number) =>
    setForm(f => ({ ...f, format_models: f.format_models.filter((_, j) => j !== i) }))
  const addFormatRow = () =>
    setForm(f => ({
      ...f,
      format_models: [...f.format_models, { format: f.supported_formats[0] ?? 'openai-chat', modelsText: '' }],
    }))
  const setMappingRow = (i: number, patch: Partial<MappingRow>) =>
    setForm(f => ({ ...f, model_mapping: f.model_mapping.map((r, j) => (j === i ? { ...r, ...patch } : r)) }))
  const removeMappingRow = (i: number) =>
    setForm(f => ({ ...f, model_mapping: f.model_mapping.filter((_, j) => j !== i) }))
  const addMappingRow = () => setForm(f => ({ ...f, model_mapping: [...f.model_mapping, { key: '', value: '' }] }))

  // 格式行下拉选项 = 已选 supported_formats
  const formatOptions: Record<string, string> = {}
  for (const f of form.supported_formats) formatOptions[f] = FORMAT_LABELS[f]

  return (
    <div className="space-y-3">
      <div className="space-y-1.5">
        <Label htmlFor="tpl-name">{t('templates.nameLabel')}</Label>
        <Input
          id="tpl-name"
          value={form.name}
          placeholder={t('templates.namePlaceholder')}
          onChange={e => setForm(f => ({ ...f, name: e.target.value }))}
        />
      </div>
      <div className="space-y-1.5">
        <Label htmlFor="tpl-base">BaseURL</Label>
        <Input
          id="tpl-base"
          value={form.base_url}
          placeholder="https://api.openai.com/v1"
          onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))}
        />
      </div>

      {/* supported_formats：chips 多选（非空校验） */}
      <div className="space-y-1.5">
        <Label>{t('templates.supportedFormatsLabel')}</Label>
        <div className="flex flex-wrap gap-1.5">
          {FORMATS.map(f => {
            const on = form.supported_formats.includes(f)
            return (
              <Button
                key={f}
                type="button"
                size="sm"
                variant={on ? 'secondary' : 'outline'}
                aria-pressed={on}
                className="rounded-full"
                onClick={() => toggleFormat(f)}
              >
                {FORMAT_LABELS[f]}
              </Button>
            )
          })}
        </div>
        {error && <p className="text-sm text-destructive">{error}</p>}
      </div>

      <div className="space-y-1.5">
        <Label htmlFor="tpl-models">{t('templates.modelsLabel')}</Label>
        <Input
          id="tpl-models"
          value={form.modelsText}
          placeholder="gpt-4o, gpt-4o-mini"
          onChange={e => setForm(f => ({ ...f, modelsText: e.target.value }))}
        />
      </div>

      {/* format_models 动态行：格式 → 模型列表（逗号分隔）；未配置的格式支持全部模型 */}
      <div className="space-y-1.5">
        <Label>{t('templates.formatModelsLabel')}</Label>
        <p className="text-xs text-muted-foreground">{t('templates.formatModelsHint')}</p>
        <div className="space-y-1.5">
          {form.format_models.map((row, i) => (
            <div key={i} className="flex items-center gap-1.5">
              <Select
                items={formatOptions}
                value={row.format}
                onValueChange={v => setFormatRow(i, { format: v as RequestFormat })}
              >
                <SelectTrigger className="w-40"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {form.supported_formats.map(f => (
                    <SelectItem key={f} value={f} label={FORMAT_LABELS[f]}>{FORMAT_LABELS[f]}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Input
                className="flex-1"
                placeholder={t('templates.modelsListPlaceholder')}
                value={row.modelsText}
                onChange={e => setFormatRow(i, { modelsText: e.target.value })}
              />
              <Button variant="ghost" size="icon-sm" title={t('templates.deleteRow')} onClick={() => removeFormatRow(i)}>
                <X />
              </Button>
            </div>
          ))}
          <Button variant="outline" size="sm" disabled={form.supported_formats.length === 0} onClick={addFormatRow}>
            <Plus /> {t('templates.addFormatRow')}
          </Button>
        </div>
      </div>

      {/* model_mapping 动态行：客户端模型 → 上游模型 */}
      <div className="space-y-1.5">
        <Label>{t('templates.modelMappingLabel')}</Label>
        <div className="space-y-1.5">
          {form.model_mapping.map((row, i) => (
            <div key={i} className="flex items-center gap-1.5">
              <Input
                className="flex-1"
                placeholder={t('templates.clientModelPlaceholder')}
                value={row.key}
                onChange={e => setMappingRow(i, { key: e.target.value })}
              />
              <span className="text-muted-foreground">→</span>
              <Input
                className="flex-1"
                placeholder={t('templates.upstreamModelPlaceholder')}
                value={row.value}
                onChange={e => setMappingRow(i, { value: e.target.value })}
              />
              <Button variant="ghost" size="icon-sm" title={t('templates.deleteRow')} onClick={() => removeMappingRow(i)}>
                <X />
              </Button>
            </div>
          ))}
          <Button variant="outline" size="sm" onClick={addMappingRow}>
            <Plus /> {t('templates.addMapping')}
          </Button>
        </div>
      </div>
    </div>
  )
}

export default function Templates() {
  const { t: tr } = useTranslation()
  const qc = useQueryClient()

  // —— 列表状态：分页/筛选/排序 ——
  const [offset, setOffset] = useState(0)
  const [name, setName] = useState('')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 无主动排序（默认 id desc）
  const [order, setOrder] = useState<SortOrder>('desc')
  const [debouncedName, setDebouncedName] = useState('')
  const [selected, setSelected] = useState<number[]>([])

  // 搜索输入防抖 300ms
  useEffect(() => {
    const timer = setTimeout(() => setDebouncedName(name), 300)
    return () => clearTimeout(timer)
  }, [name])

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['templates', { limit: LIMIT, offset, name: debouncedName, sort: activeSort ?? 'id', order }],
    queryFn: () => api.listTemplates({ limit: LIMIT, offset, name: debouncedName || undefined, sort: activeSort ?? 'id', order }),
  })
  const rows = data?.rows ?? []

  // 行数据变化后清理已不存在的勾选
  useEffect(() => {
    const ids = new Set(rows.map(r => r.ID))
    setSelected(s => s.filter(id => ids.has(id)))
  }, [rows])

  // 筛选/排序/翻页变化 → 重置 offset 并清空选择
  const onNameChange = (v: string) => { setName(v); setOffset(0); setSelected([]) }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 id desc）
  const onColumnToggle = (col: string) => {
    setOffset(0)
    setSelected([])
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
  const onOffsetChange = (o: number) => { setOffset(o); setSelected([]) }

  // —— 创建/编辑对话框 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Template | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  const [validationMsg, setValidationMsg] = useState<string | null>(null)
  // —— 批量更新对话框 ——
  const [batchOpen, setBatchOpen] = useState(false)
  const batchResolve = useRef<((r: 'cancelled' | 'submitted') => void) | null>(null)
  // —— 删除确认 ——
  const [deleting, setDeleting] = useState<Template | null>(null)

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setValidationMsg(null)
    setDialogOpen(true)
  }
  const openEdit = (t: Template) => {
    setEditing(t)
    setForm(toForm(t))
    setValidationMsg(null)
    setDialogOpen(true)
  }
  // BatchBar 的 onUpdate 返回 promise：对话框关闭（提交成功/取消）时 resolve。
  const closeBatchUpdate = (r: 'cancelled' | 'submitted' = 'cancelled') => {
    setBatchOpen(false)
    setValidationMsg(null)
    batchResolve.current?.(r)
    batchResolve.current = null
  }
  const openBatchUpdate = () => {
    setForm(emptyForm())
    setValidationMsg(null)
    setBatchOpen(true)
    return new Promise<'cancelled' | 'submitted'>(resolve => {
      batchResolve.current = resolve
    })
  }

  const save = useMutation({
    mutationFn: (f: FormState) => (editing ? api.updateTemplate(editing.ID, toBody(f)) : api.createTemplate(toBody(f))),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setDialogOpen(false)
    },
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.deleteTemplate(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setDeleting(null)
      // 删除的是当前页最后一行时回退一页
      if (rows.length === 1 && offset > 0) setOffset(offset - LIMIT)
    },
  })
  const batchDelete = useMutation({
    mutationFn: (ids: number[]) => api.deleteTemplatesBatch(ids),
    onSuccess: (_res, ids) => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      setSelected([])
      // 当前页被删空时回到最后有效页
      const after = (data?.total ?? 0) - ids.length
      if (offset > 0 && offset >= after) setOffset(Math.max(0, after - (after % LIMIT)))
    },
  })
  const batchUpdate = useMutation({
    mutationFn: ({ ids, fields }: { ids: number[]; fields: TemplatePatch }) => api.updateTemplatesBatch(ids, fields),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['templates'] })
      closeBatchUpdate('submitted')
      setSelected([])
    },
    // onError 不 resolve：对话框保持打开就地展示错误，取消时由 closeBatchUpdate resolve
  })

  const submit = () => {
    setValidationMsg(null)
    if (!form.name.trim() || !form.base_url.trim() || form.supported_formats.length === 0) {
      setValidationMsg(tr('templates.formRequired'))
      return
    }
    save.mutate(form)
  }

  const submitBatch = () => {
    setValidationMsg(null)
    const fields = toPatch(form)
    if (Object.keys(fields).length === 0) {
      setValidationMsg(tr('templates.batchUpdateEmpty'))
      return
    }
    batchUpdate.mutate({ ids: selected, fields })
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  const toggleRow = (id: number, on: boolean) => setSelected(s => (on ? [...s, id] : s.filter(x => x !== id)))
  const allChecked = rows.length > 0 && rows.every(r => selected.includes(r.ID))
  const someChecked = rows.some(r => selected.includes(r.ID)) && !allChecked

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{tr('templates.title')}</h1>
          <p className="text-sm text-muted-foreground">{tr('templates.subtitle')}</p>
        </div>
        <Button onClick={openCreate}><Plus /> {tr('templates.new')}</Button>
      </div>

      <ListToolbar
        name={name}
        onNameChange={onNameChange}
      />

      <BatchBar
        selected={selected}
        onClear={() => setSelected([])}
        onDelete={() => batchDelete.mutate(selected)}
        onUpdate={openBatchUpdate}
      />
      {batchDelete.isError && errMsg(batchDelete.error) && (
        <p className="text-sm text-destructive">{errMsg(batchDelete.error)}</p>
      )}
      {batchUpdate.isError && errMsg(batchUpdate.error) && (
        <p className="text-sm text-destructive">{errMsg(batchUpdate.error)}</p>
      )}

      {isError ? (
        <p className="text-sm text-destructive">{tr('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Boxes className="size-10" />
            <p className="font-medium">{debouncedName ? tr('templates.noResultsTitle') : tr('templates.emptyTitle')}</p>
            <p className="text-sm">{debouncedName ? tr('templates.noResultsDesc') : tr('templates.emptyDesc')}</p>
            {debouncedName ? (
              <Button className="mt-2" variant="outline" onClick={() => onNameChange('')}>{tr('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {tr('templates.new')}</Button>
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
                      indeterminate={someChecked}
                      onCheckedChange={(c: boolean) => setSelected(c ? rows.map(r => r.ID) : [])}
                      aria-label={tr('list.selected', { count: rows.length })}
                    />
                  </TableHead>
                  <SortableHeader field="id" label="ID" active={activeSort === 'id'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="name" label={tr('templates.table.name')} active={activeSort === 'name'} order={order} onToggle={onColumnToggle} />
                  <SortableHeader field="base_url" label="BaseURL" active={activeSort === 'base_url'} order={order} onToggle={onColumnToggle} />
                  <TableHead>{tr('templates.table.supportedFormats')}</TableHead>
                  <TableHead>{tr('templates.table.models')}</TableHead>
                  <TableHead>{tr('templates.table.formatModels')}</TableHead>
                  <TableHead>{tr('templates.table.modelMapping')}</TableHead>
                  <SortableHeader field="created_at" label={tr('templates.table.createdAt')} active={activeSort === 'created_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{tr('templates.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(t => {
                  const models = commaList(t.Models)
                  const formats = Object.entries(t.FormatModels ?? {})
                  const mappings = Object.entries(t.ModelMapping ?? {})
                  return (
                    <TableRow key={t.ID} data-state={selected.includes(t.ID) ? 'selected' : undefined}>
                      <TableCell>
                        <Checkbox
                          checked={selected.includes(t.ID)}
                          onCheckedChange={(c: boolean) => toggleRow(t.ID, c)}
                          aria-label={t.Name ?? String(t.ID)}
                        />
                      </TableCell>
                      <TableCell className="tabular-nums">{t.ID}</TableCell>
                      <TableCell className="max-w-36 truncate" title={t.Name}>{t.Name}</TableCell>
                      <TableCell className="max-w-52 truncate font-mono text-xs" title={t.BaseURL}>{t.BaseURL}</TableCell>
                      <TableCell>
                        <div className="flex max-w-44 flex-wrap gap-1">
                          {(t.SupportedFormats ?? []).map(f => <FormatBadge key={f} format={f} />)}
                        </div>
                      </TableCell>
                      <TableCell className="max-w-40 truncate" title={models.full || undefined}>{models.text}</TableCell>
                      <TableCell>
                        {formats.length === 0 ? '—' : (
                          <div className="flex max-w-56 flex-wrap gap-1">
                            {formats.slice(0, 3).map(([f, ms]) => {
                              const label = FORMAT_LABELS[f as RequestFormat] ?? f
                              const list = (ms ?? []).join(', ')
                              return (
                                <Badge key={f} variant="outline" className="font-mono text-xs" title={`${label}: ${list}`}>
                                  {truncate(label, 10)}:{truncate(list, 12)}
                                </Badge>
                              )
                            })}
                            {formats.length > 3 && <Badge variant="outline">+{formats.length - 3}</Badge>}
                          </div>
                        )}
                      </TableCell>
                      <TableCell>
                        {mappings.length === 0 ? '—' : (
                          <div className="flex max-w-56 flex-wrap gap-1">
                            {mappings.slice(0, 3).map(([k, v]) => (
                              <Badge key={k} variant="outline" className="font-mono text-xs" title={`${k} → ${v}`}>
                                {truncate(k, 10)}→{truncate(v, 10)}
                              </Badge>
                            ))}
                            {mappings.length > 3 && <Badge variant="outline">+{mappings.length - 3}</Badge>}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="text-xs text-muted-foreground">{formatDateTime(t.CreatedAt)}</TableCell>
                      <TableCell className="text-right">
                        <div className="flex justify-end gap-1">
                          <Button variant="ghost" size="icon-sm" title={tr('common.edit')} onClick={() => openEdit(t)}>
                            <Pencil />
                          </Button>
                          <Button
                            variant="ghost"
                            size="icon-sm"
                            className="text-destructive"
                            title={tr('common.delete')}
                            onClick={() => setDeleting(t)}
                          >
                            <Trash2 />
                          </Button>
                        </div>
                      </TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </div>
          <Pagination total={data?.total ?? 0} limit={LIMIT} offset={offset} onOffsetChange={onOffsetChange} />
        </>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={o => { setDialogOpen(o); if (!o) setValidationMsg(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{editing ? tr('templates.editTitle', { id: editing.ID }) : tr('templates.newTitle')}</DialogTitle>
            <DialogDescription>{tr('templates.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <FormFields form={form} setForm={setForm} error={validationMsg} />
            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>{tr('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? tr('common.saving') : editing ? tr('common.saveChanges') : tr('common.create')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 批量更新对话框 —— */}
      <Dialog open={batchOpen} onOpenChange={o => { if (!o) closeBatchUpdate() }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{tr('templates.batchUpdateTitle', { count: selected.length })}</DialogTitle>
            <DialogDescription>{tr('templates.batchUpdateDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <FormFields form={form} setForm={setForm} />
            {validationMsg && <p className="text-sm text-destructive">{validationMsg}</p>}
            {batchUpdate.isError && errMsg(batchUpdate.error) && (
              <p className="text-sm text-destructive">{errMsg(batchUpdate.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => closeBatchUpdate()}>{tr('common.cancel')}</Button>
            <Button onClick={submitBatch} disabled={batchUpdate.isPending}>
              {batchUpdate.isPending ? tr('common.saving') : tr('common.saveChanges')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{tr('templates.deleteTitle')}</DialogTitle>
            <DialogDescription>
              {tr('templates.deleteDesc', { name: deleting?.Name })}
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>{tr('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID)} disabled={remove.isPending}>
              {remove.isPending ? tr('common.deleting') : tr('common.confirmDelete')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

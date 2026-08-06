import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Plus, Pencil, Trash2, Boxes, X } from 'lucide-react'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime, truncate, commaList } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type Template = components['schemas']['Template']
type TemplateCreate = components['schemas']['TemplateCreate']
type RequestFormat = components['schemas']['RequestFormat']

const FORMAT_LABELS: Record<RequestFormat, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  anthropic: 'Anthropic',
}
const FORMATS = Object.keys(FORMAT_LABELS) as RequestFormat[]

// 动态行（model_formats / model_mapping）编辑。
interface RowForm { key: string; value: string }

interface FormState {
  name: string
  base_url: string
  default_format: RequestFormat
  modelsText: string
  model_formats: RowForm[]
  model_mapping: RowForm[]
}

const emptyForm = (): FormState => ({
  name: '',
  base_url: '',
  default_format: 'openai-chat',
  modelsText: '',
  model_formats: [],
  model_mapping: [],
})

function toForm(t: Template): FormState {
  return {
    name: t.Name ?? '',
    base_url: t.BaseURL ?? '',
    default_format: t.DefaultFormat ?? 'openai-chat',
    modelsText: (t.Models ?? []).join(', '),
    model_formats: Object.entries(t.ModelFormats ?? {}).map(([key, value]) => ({ key, value })),
    model_mapping: Object.entries(t.ModelMapping ?? {}).map(([key, value]) => ({ key, value })),
  }
}

function toBody(f: FormState): TemplateCreate {
  const model_formats: Record<string, RequestFormat> = {}
  for (const r of f.model_formats) if (r.key.trim()) model_formats[r.key.trim()] = r.value as RequestFormat
  const model_mapping: Record<string, string> = {}
  for (const r of f.model_mapping) if (r.key.trim() && r.value.trim()) model_mapping[r.key.trim()] = r.value.trim()
  return {
    name: f.name.trim(),
    base_url: f.base_url.trim(),
    default_format: f.default_format,
    models: f.modelsText.split(',').map(s => s.trim()).filter(Boolean),
    model_formats: f.model_formats.length ? model_formats : undefined,
    model_mapping: f.model_mapping.length ? model_mapping : undefined,
  }
}

function FormatBadge({ format }: { format?: RequestFormat }) {
  return <Badge variant="outline">{format ? FORMAT_LABELS[format] : '—'}</Badge>
}

export default function Templates() {
  const qc = useQueryClient()
  const { data, isLoading, isError, error } = useQuery({ queryKey: ['templates'], queryFn: api.listTemplates })

  // —— 创建/编辑对话框 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Template | null>(null)
  const [form, setForm] = useState<FormState>(emptyForm())
  // —— 删除确认 ——
  const [deleting, setDeleting] = useState<Template | null>(null)

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setDialogOpen(true)
  }
  const openEdit = (t: Template) => {
    setEditing(t)
    setForm(toForm(t))
    setDialogOpen(true)
  }

  const save = useMutation({
    mutationFn: (f: FormState) =>
      editing ? api.updateTemplate(editing.ID!, toBody(f)) : api.createTemplate(toBody(f)),
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
    },
  })

  const submit = () => {
    if (!form.name.trim() || !form.base_url.trim()) return
    save.mutate(form)
  }

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)

  const setRow = (list: 'model_formats' | 'model_mapping', i: number, patch: Partial<RowForm>) =>
    setForm(f => ({ ...f, [list]: f[list].map((r, j) => (j === i ? { ...r, ...patch } : r)) }))
  const removeRow = (list: 'model_formats' | 'model_mapping', i: number) =>
    setForm(f => ({ ...f, [list]: f[list].filter((_, j) => j !== i) }))
  const addRow = (list: 'model_formats' | 'model_mapping') =>
    setForm(f => ({ ...f, [list]: [...f[list], { key: '', value: list === 'model_formats' ? f.default_format : '' }] }))

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">模板</h1>
          <p className="text-sm text-muted-foreground">上游厂商配置：base_url、格式、模型集与映射</p>
        </div>
        <Button onClick={openCreate}><Plus /> 新建模板</Button>
      </div>

      {isError ? (
        <p className="text-sm text-destructive">加载失败：{(error as Error).message}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : (data ?? []).length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Boxes className="size-10" />
            <p className="font-medium">暂无模板</p>
            <p className="text-sm">先创建模板，再配置账号</p>
            <Button className="mt-2" onClick={openCreate}><Plus /> 新建模板</Button>
          </Card>
        </motion.div>
      ) : (
        <Card className="overflow-hidden">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>名称</TableHead>
                <TableHead>BaseURL</TableHead>
                <TableHead>默认格式</TableHead>
                <TableHead>模型</TableHead>
                <TableHead>格式覆盖</TableHead>
                <TableHead>模型映射</TableHead>
                <TableHead>创建时间</TableHead>
                <TableHead className="text-right">操作</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data ?? []).map(t => {
                const models = commaList(t.Models)
                const formats = Object.entries(t.ModelFormats ?? {})
                const mappings = Object.entries(t.ModelMapping ?? {})
                return (
                  <TableRow key={t.ID}>
                    <TableCell className="tabular-nums">{t.ID}</TableCell>
                    <TableCell className="max-w-36 truncate" title={t.Name}>{t.Name}</TableCell>
                    <TableCell className="max-w-52 truncate font-mono text-xs" title={t.BaseURL}>{t.BaseURL}</TableCell>
                    <TableCell><FormatBadge format={t.DefaultFormat} /></TableCell>
                    <TableCell className="max-w-40 truncate" title={models.full || undefined}>{models.text}</TableCell>
                    <TableCell>
                      {formats.length === 0 ? '—' : (
                        <div className="flex max-w-56 flex-wrap gap-1">
                          {formats.slice(0, 3).map(([k, v]) => (
                            <Badge key={k} variant="outline" className="font-mono text-xs" title={`${k} → ${FORMAT_LABELS[v]}`}>
                              {truncate(k, 12)}→{FORMAT_LABELS[v]}
                            </Badge>
                          ))}
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
                        <Button variant="ghost" size="icon-sm" title="编辑" onClick={() => openEdit(t)}><Pencil /></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title="删除" onClick={() => setDeleting(t)}><Trash2 /></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </Card>
      )}

      {/* —— 创建/编辑对话框 —— */}
      <Dialog open={dialogOpen} onOpenChange={setDialogOpen}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{editing ? `编辑模板 #${editing.ID}` : '新建模板'}</DialogTitle>
            <DialogDescription>base_url 需为完整上游地址（含 /v1 前缀）</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="space-y-1.5">
              <Label htmlFor="tpl-name">名称</Label>
              <Input id="tpl-name" value={form.name} placeholder="如 openai-main" onChange={e => setForm(f => ({ ...f, name: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="tpl-base">BaseURL</Label>
              <Input id="tpl-base" value={form.base_url} placeholder="https://api.openai.com/v1" onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))} />
            </div>
            <div className="space-y-1.5">
              <Label>默认请求格式</Label>
              <Select items={FORMAT_LABELS} value={form.default_format} onValueChange={v => setForm(f => ({ ...f, default_format: v as RequestFormat }))}>
                <SelectTrigger className="w-full"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {FORMATS.map(f => <SelectItem key={f} value={f}>{FORMAT_LABELS[f]}</SelectItem>)}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="tpl-models">模型列表（逗号分隔）</Label>
              <Input id="tpl-models" value={form.modelsText} placeholder="gpt-4o, gpt-4o-mini" onChange={e => setForm(f => ({ ...f, modelsText: e.target.value }))} />
            </div>

            {/* model_formats 动态行：模型 → 格式覆盖 */}
            <div className="space-y-1.5">
              <Label>模型格式覆盖（model_formats）</Label>
              <div className="space-y-1.5">
                {form.model_formats.map((row, i) => (
                  <div key={i} className="flex items-center gap-1.5">
                    <Input className="flex-1" placeholder="模型名" value={row.key} onChange={e => setRow('model_formats', i, { key: e.target.value })} />
                    <Select
                      items={FORMAT_LABELS}
                      value={row.value as RequestFormat}
                      onValueChange={v => setRow('model_formats', i, { value: v as string })}
                    >
                      <SelectTrigger className="w-40"><SelectValue /></SelectTrigger>
                      <SelectContent>
                        {FORMATS.map(f => <SelectItem key={f} value={f}>{FORMAT_LABELS[f]}</SelectItem>)}
                      </SelectContent>
                    </Select>
                    <Button variant="ghost" size="icon-sm" title="删除行" onClick={() => removeRow('model_formats', i)}><X /></Button>
                  </div>
                ))}
                <Button variant="outline" size="sm" onClick={() => addRow('model_formats')}><Plus /> 添加覆盖</Button>
              </div>
            </div>

            {/* model_mapping 动态行：客户端模型 → 上游模型 */}
            <div className="space-y-1.5">
              <Label>模型映射（model_mapping）</Label>
              <div className="space-y-1.5">
                {form.model_mapping.map((row, i) => (
                  <div key={i} className="flex items-center gap-1.5">
                    <Input className="flex-1" placeholder="客户端模型名" value={row.key} onChange={e => setRow('model_mapping', i, { key: e.target.value })} />
                    <span className="text-muted-foreground">→</span>
                    <Input className="flex-1" placeholder="上游模型名" value={row.value} onChange={e => setRow('model_mapping', i, { value: e.target.value })} />
                    <Button variant="ghost" size="icon-sm" title="删除行" onClick={() => removeRow('model_mapping', i)}><X /></Button>
                  </div>
                ))}
                <Button variant="outline" size="sm" onClick={() => addRow('model_mapping')}><Plus /> 添加映射</Button>
              </div>
            </div>

            {save.isError && errMsg(save.error) && (
              <p className="text-sm text-destructive">{errMsg(save.error)}</p>
            )}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)}>取消</Button>
            <Button onClick={submit} disabled={save.isPending || !form.name.trim() || !form.base_url.trim()}>
              {save.isPending ? '保存中…' : editing ? '保存修改' : '创建'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认 —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>删除模板</DialogTitle>
            <DialogDescription>
              确认删除模板「{deleting?.Name}」？绑定该模板的账号将无法使用。
            </DialogDescription>
          </DialogHeader>
          {remove.isError && errMsg(remove.error) && (
            <p className="text-sm text-destructive">{errMsg(remove.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={remove.isPending}>取消</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID!)} disabled={remove.isPending}>
              {remove.isPending ? '删除中…' : '确认删除'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

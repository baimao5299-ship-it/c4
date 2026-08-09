import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import { Coins, Filter, Pencil, Plus, RefreshCw, Trash2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import { ApiUnauthorized } from '@/lib/api/client'
import { ListToolbar } from '@/components/list-toolbar'
import { PagePagination } from '@/components/page-pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { toast } from '@/components/ui/toast'
import { cn } from '@/lib/utils'
import { formatDateTime, formatPricePerMillion } from '@/components/fmt'
import type { components } from '@/lib/api/schema'

type Pricing = components['schemas']['Pricing']
type PricingUpsert = components['schemas']['PricingUpsert']
type PricingSource = components['schemas']['PricingSource']

const PAGE_SIZE = 20
const SOURCES: PricingSource[] = ['litellm', 'manual']

// 可选价格字段（全部毫分/1M tokens；表单留空 = 提交时省略 → 落库 NULL）。
// 类型唯一来源 schema.d.ts：PricingUpsert 键名为小写下划线（与 Go 响应大写字段区分）。
type OptKey = Exclude<keyof PricingUpsert, 'prompt_price_per_million' | 'completion_price_per_million'>
const OPT_KEYS: OptKey[] = [
  'cache_read_price_per_million',
  'cache_creation_price_per_million',
  'priority_prompt_price_per_million',
  'priority_completion_price_per_million',
  'priority_cache_read_price_per_million',
  'priority_cache_creation_price_per_million',
  'flex_prompt_price_per_million',
  'flex_completion_price_per_million',
  'flex_cache_read_price_per_million',
  'flex_cache_creation_price_per_million',
  'above_threshold',
  'above_prompt_price_per_million',
  'above_completion_price_per_million',
  'above_cache_read_price_per_million',
  'above_cache_creation_price_per_million',
  'above_priority_prompt_price_per_million',
  'above_priority_completion_price_per_million',
  'above_priority_cache_read_price_per_million',
  'above_priority_cache_creation_price_per_million',
  'above_flex_prompt_price_per_million',
  'above_flex_completion_price_per_million',
  'above_flex_cache_read_price_per_million',
  'above_flex_cache_creation_price_per_million',
  'fast_multiplier',
]

// 回显映射：Pricing 响应字段（Go 大写）→ PricingUpsert 提交键（小写下划线）。
const RESP_TO_KEY: { r: keyof Pricing; k: OptKey }[] = [
  { r: 'CacheReadPricePerMillion', k: 'cache_read_price_per_million' },
  { r: 'CacheCreationPricePerMillion', k: 'cache_creation_price_per_million' },
  { r: 'PriorityPromptPricePerMillion', k: 'priority_prompt_price_per_million' },
  { r: 'PriorityCompletionPricePerMillion', k: 'priority_completion_price_per_million' },
  { r: 'PriorityCacheReadPricePerMillion', k: 'priority_cache_read_price_per_million' },
  { r: 'PriorityCacheCreationPricePerMillion', k: 'priority_cache_creation_price_per_million' },
  { r: 'FlexPromptPricePerMillion', k: 'flex_prompt_price_per_million' },
  { r: 'FlexCompletionPricePerMillion', k: 'flex_completion_price_per_million' },
  { r: 'FlexCacheReadPricePerMillion', k: 'flex_cache_read_price_per_million' },
  { r: 'FlexCacheCreationPricePerMillion', k: 'flex_cache_creation_price_per_million' },
  { r: 'AboveThreshold', k: 'above_threshold' },
  { r: 'FastMultiplier', k: 'fast_multiplier' },
  { r: 'AbovePromptPricePerMillion', k: 'above_prompt_price_per_million' },
  { r: 'AboveCompletionPricePerMillion', k: 'above_completion_price_per_million' },
  { r: 'AboveCacheReadPricePerMillion', k: 'above_cache_read_price_per_million' },
  { r: 'AboveCacheCreationPricePerMillion', k: 'above_cache_creation_price_per_million' },
  { r: 'AbovePriorityPromptPricePerMillion', k: 'above_priority_prompt_price_per_million' },
  { r: 'AbovePriorityCompletionPricePerMillion', k: 'above_priority_completion_price_per_million' },
  { r: 'AbovePriorityCacheReadPricePerMillion', k: 'above_priority_cache_read_price_per_million' },
  { r: 'AbovePriorityCacheCreationPricePerMillion', k: 'above_priority_cache_creation_price_per_million' },
  { r: 'AboveFlexPromptPricePerMillion', k: 'above_flex_prompt_price_per_million' },
  { r: 'AboveFlexCompletionPricePerMillion', k: 'above_flex_completion_price_per_million' },
  { r: 'AboveFlexCacheReadPricePerMillion', k: 'above_flex_cache_read_price_per_million' },
  { r: 'AboveFlexCacheCreationPricePerMillion', k: 'above_flex_cache_creation_price_per_million' },
]

// 表单分组（仅渲染用）：label 取 pricing 作用域 tKey；键与 OPT_KEYS 一一对应。
interface OptField { key: OptKey; tKey: string }
const CACHE_FIELDS: OptField[] = [
  { key: 'cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const PRIORITY_FIELDS: OptField[] = [
  { key: 'priority_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'priority_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'priority_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'priority_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const FLEX_FIELDS: OptField[] = [
  { key: 'flex_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'flex_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'flex_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'flex_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const ABOVE_FIELDS: OptField[] = [
  { key: 'above_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'above_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'above_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'above_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const ABOVE_PRIORITY_FIELDS: OptField[] = [
  { key: 'above_priority_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'above_priority_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'above_priority_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'above_priority_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]
const ABOVE_FLEX_FIELDS: OptField[] = [
  { key: 'above_flex_prompt_price_per_million', tKey: 'promptLabel' },
  { key: 'above_flex_completion_price_per_million', tKey: 'completionLabel' },
  { key: 'above_flex_cache_read_price_per_million', tKey: 'cacheReadLabel' },
  { key: 'above_flex_cache_creation_price_per_million', tKey: 'cacheWriteLabel' },
]

// 快速倍率（万分数）→ 展示：20000 = ×2.0；null = 无倍率。
const formatFastMultiplier = (m: number | null | undefined): string =>
  m == null ? '—' : `×${(m / 10000).toFixed(1)}`

// 来源徽章：自动同步灰点 / 手动蓝点（手动价由用户维护）。
function SourceBadge({ source }: { source: PricingSource }) {
  const { t } = useTranslation()
  const manual = source === 'manual'
  return (
    <Badge variant="secondary" className={cn('gap-1.5', manual ? 'text-blue-700 dark:text-blue-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 shrink-0 rounded-full', manual ? 'bg-blue-500' : 'bg-muted-foreground/60')} />
      {t(`pricing.source.${source}`)}
    </Badge>
  )
}

// 手动设价表单态：所有价格字段均为字符串（'' = 未填/不设置），提交时转换。
interface PriceForm {
  model: string
  prompt: string
  completion: string
  opt: Record<OptKey, string>
}

const emptyForm = (): PriceForm => {
  const opt = {} as Record<OptKey, string>
  for (const k of OPT_KEYS) opt[k] = ''
  return { model: '', prompt: '', completion: '', opt }
}

function toForm(p: Pricing): PriceForm {
  const f = emptyForm()
  f.model = p.Model
  f.prompt = String(p.PromptPricePerMillion)
  f.completion = String(p.CompletionPricePerMillion)
  for (const { r, k } of RESP_TO_KEY) {
    const v = p[r]
    f.opt[k] = v == null ? '' : String(v)
  }
  return f
}

// 提交体：必填 prompt/completion 恒发；可选字段空 = 省略（服务端落库 NULL）。
function toBody(f: PriceForm): PricingUpsert {
  const body: PricingUpsert = {
    prompt_price_per_million: Number(f.prompt),
    completion_price_per_million: Number(f.completion),
  }
  for (const k of OPT_KEYS) {
    const v = f.opt[k]
    if (v !== '') body[k] = Number(v)
  }
  return body
}

// 非负整数校验（价格字段通用；'' = 未填不校验）。
const isNonNegInt = (v: string) => v === '' || (Number.isInteger(Number(v)) && Number(v) >= 0)

export default function PricingPage() {
  const { t } = useTranslation()
  const qc = useQueryClient()

  // —— 列表：page/page_size 1-based 分页（PagePagination 范式）+ source 筛选 + model 模糊搜索 ——
  const [page, setPage] = useState(1)
  const [model, setModel] = useState('')
  const [sourceFilter, setSourceFilter] = useState<'all' | PricingSource>('all')
  const [activeSort, setActiveSort] = useState<string | null>(null) // null = 默认 model asc
  const [order, setOrder] = useState<SortOrder>('desc')
  const sort = activeSort ?? 'model'
  const ord = activeSort ? order : 'asc'

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ['pricing', { page, page_size: PAGE_SIZE, source: sourceFilter, model, sort, order: ord }],
    queryFn: () =>
      api.listPricing({
        page,
        page_size: PAGE_SIZE,
        source: sourceFilter === 'all' ? undefined : sourceFilter,
        model: model || undefined,
        sort,
        order: ord,
      }),
  })
  const rows = data?.rows ?? []

  // 末页死胡同守卫：非首页的当前页数据被清空（筛选把末页清空）时回退到第 1 页。
  useEffect(() => {
    if (!isLoading && !isError && rows.length === 0 && page > 1) setPage(1)
  }, [isLoading, isError, rows.length, page])

  const resetPage = () => setPage(1)
  const changeModel = (v: string) => { setModel(v); resetPage() }
  const changeSource = (v: string) => { setSourceFilter(v as 'all' | PricingSource); resetPage() }
  // 列头三态：新列 → 降序；同列降序 → 升序；同列升序 → 取消（回默认 model asc）。
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
  const hasFilters = model !== '' || sourceFilter !== 'all'
  const clearFilters = () => { setModel(''); setSourceFilter('all'); resetPage() }

  // —— 手动触发价格同步（成功后展示统计并刷新列表） ——
  const sync = useMutation({
    mutationFn: () => api.syncPricing(),
    onSuccess: res => {
      toast.add({ title: t('pricing.syncDone', { rows: res.rows, skipped: res.skipped, updated: res.updated }), type: 'success' })
      qc.invalidateQueries({ queryKey: ['pricing'] })
    },
    onError: (e: Error) => toast.add({ title: e.message, type: 'error' }),
  })

  // —— 手动设价（新建/编辑复用）：编辑 litellm 行 = 保存后接管为手动价 ——
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<Pricing | null>(null)
  const [form, setForm] = useState<PriceForm>(emptyForm())
  const [formErr, setFormErr] = useState<string | null>(null)
  const [activeTab, setActiveTab] = useState('base')

  const openCreate = () => {
    setEditing(null)
    setForm(emptyForm())
    setFormErr(null)
    setActiveTab('base')
    setDialogOpen(true)
  }
  const openEdit = (p: Pricing) => {
    setEditing(p)
    setForm(toForm(p))
    setFormErr(null)
    setActiveTab('base')
    setDialogOpen(true)
  }
  const setOpt = (k: OptKey, v: string) => {
    setForm(f => ({ ...f, opt: { ...f.opt, [k]: v } }))
    setFormErr(null)
  }

  const save = useMutation({
    mutationFn: (f: PriceForm) => api.upsertPricing(editing ? editing.Model : f.model.trim(), toBody(f)),
    onSuccess: () => {
      toast.add({ title: t('pricing.saved'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['pricing'] })
      setDialogOpen(false)
    },
  })
  const submit = () => {
    const fm = form
    const fast = fm.opt.fast_multiplier
    const valid =
      (editing || fm.model.trim() !== '') &&
      isNonNegInt(fm.prompt) && Number(fm.prompt) >= 0 && fm.prompt !== '' &&
      isNonNegInt(fm.completion) && fm.completion !== '' &&
      (fast === '' || (Number.isInteger(Number(fast)) && Number(fast) >= 1 && Number(fast) <= 100000)) &&
      OPT_KEYS.every(k => k === 'fast_multiplier' || isNonNegInt(fm.opt[k]))
    if (!valid) {
      setFormErr(t('pricing.formInvalid'))
      return
    }
    save.mutate(fm)
  }

  // —— 删除手动价（自动同步维护的行 → 服务端 409，错误文案展示服务端返回） ——
  const [deleting, setDeleting] = useState<Pricing | null>(null)
  const del = useMutation({
    mutationFn: (p: Pricing) => api.deletePricing(p.Model),
    onSuccess: () => {
      toast.add({ title: t('pricing.deleted'), type: 'success' })
      qc.invalidateQueries({ queryKey: ['pricing'] })
      setDeleting(null)
    },
  })

  const errMsg = (e: unknown) => (e instanceof ApiUnauthorized ? null : (e as Error)?.message)
  const sourceItems = Object.fromEntries([['all', t('pricing.all')], ...SOURCES.map(s => [s, t(`pricing.source.${s}`)])])

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-lg font-semibold">{t('pricing.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('pricing.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={() => sync.mutate()} disabled={sync.isPending}>
            <RefreshCw className={sync.isPending ? 'animate-spin' : ''} />
            {sync.isPending ? t('pricing.syncing') : t('pricing.sync')}
          </Button>
          <Button onClick={openCreate}><Plus /> {t('pricing.new')}</Button>
        </div>
      </div>

      <ListToolbar name={model} onNameChange={changeModel} placeholder={t('pricing.searchModel')}>
        <Select items={sourceItems} value={sourceFilter} onValueChange={changeSource}>
          <SelectTrigger size="default" className="w-40" aria-label={t('pricing.all')}>
            <SelectValue placeholder={t('pricing.all')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all" label={t('pricing.all')}>{t('pricing.all')}</SelectItem>
            {SOURCES.map(s => <SelectItem key={s} value={s} label={t(`pricing.source.${s}`)}>{t(`pricing.source.${s}`)}</SelectItem>)}
          </SelectContent>
        </Select>
        {sourceFilter !== 'all' && (
          <Button variant="ghost" size="lg" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
        )}
      </ListToolbar>

      {isError ? (
        <p className="text-sm text-destructive">{t('common.loadFailed', { message: (error as Error).message })}</p>
      ) : isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 4 }).map((_, i) => <Skeleton key={i} className="h-12" />)}
        </div>
      ) : rows.length === 0 ? (
        <motion.div initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
          <Card className="flex flex-col items-center gap-2 py-12 text-muted-foreground">
            <Coins className="size-10" />
            <p className="font-medium">{hasFilters ? t('pricing.filterEmpty') : t('pricing.emptyTitle')}</p>
            {!hasFilters && <p className="text-sm">{t('pricing.emptyDesc')}</p>}
            {hasFilters ? (
              <Button className="mt-2" variant="outline" onClick={clearFilters}><Filter /> {t('list.reset')}</Button>
            ) : (
              <Button className="mt-2" onClick={openCreate}><Plus /> {t('pricing.new')}</Button>
            )}
          </Card>
        </motion.div>
      ) : (
        <>
          <div className="overflow-hidden rounded-lg border bg-card">
            <Table>
              <TableHeader>
                <TableRow>
                  <SortableHeader field="model" label={t('pricing.table.model')} active={activeSort === 'model'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('pricing.table.prompt')}</TableHead>
                  <TableHead className="text-right">{t('pricing.table.completion')}</TableHead>
                  <TableHead className="text-right">{t('pricing.table.cacheRead')}</TableHead>
                  <TableHead className="text-right">{t('pricing.table.cacheWrite')}</TableHead>
                  <TableHead className="text-right">{t('pricing.table.fastMultiplier')}</TableHead>
                  <TableHead className="text-right">{t('pricing.table.aboveThreshold')}</TableHead>
                  <TableHead>{t('pricing.table.source')}</TableHead>
                  <TableHead>{t('pricing.table.provider')}</TableHead>
                  <SortableHeader field="updated_at" label={t('pricing.table.updatedAt')} active={activeSort === 'updated_at'} order={order} onToggle={onColumnToggle} />
                  <TableHead className="text-right">{t('pricing.table.actions')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map(p => (
                  <TableRow key={p.Model}>
                    <TableCell className="max-w-48 truncate font-mono text-sm" title={p.Model}>{p.Model}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.PromptPricePerMillion)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CompletionPricePerMillion)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CacheReadPricePerMillion)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPricePerMillion(p.CacheCreationPricePerMillion)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatFastMultiplier(p.FastMultiplier)}</TableCell>
                    <TableCell className="text-right tabular-nums">{p.AboveThreshold == null ? '—' : t('pricing.table.aboveThresholdValue', { value: p.AboveThreshold })}</TableCell>
                    <TableCell><SourceBadge source={p.Source} /></TableCell>
                    <TableCell className="max-w-32 truncate" title={p.Provider ?? undefined}>{p.Provider || '—'}</TableCell>
                    <TableCell className="text-xs text-muted-foreground whitespace-nowrap">{formatDateTime(p.UpdatedAt)}</TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(p)}><Pencil /></Button>
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          className="text-destructive"
                          title={t('pricing.deleteTitle')}
                          onClick={() => setDeleting(p)}
                          disabled={del.isPending}
                        >
                          <Trash2 />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </div>
          <PagePagination total={data?.total ?? 0} pageSize={PAGE_SIZE} page={page} onPageChange={setPage} />
        </>
      )}

      {/* —— 手动设价对话框（新建/编辑复用；Tabs 分组防撑爆） —— */}
      <Dialog open={dialogOpen} onOpenChange={o => { if (!o && !save.isPending) setDialogOpen(false) }}>
        <DialogContent className="sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>{editing ? t('pricing.editTitle', { model: editing.Model }) : t('pricing.newTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <Tabs value={activeTab} onValueChange={v => v && setActiveTab(v)}>
            <TabsList className="w-full">
              <TabsTrigger value="base" className="flex-1">{t('pricing.tabBase')}</TabsTrigger>
              <TabsTrigger value="cache" className="flex-1">{t('pricing.tabCache')}</TabsTrigger>
              <TabsTrigger value="tier" className="flex-1">{t('pricing.tabTier')}</TabsTrigger>
              <TabsTrigger value="segment" className="flex-1">{t('pricing.tabSegment')}</TabsTrigger>
              <TabsTrigger value="multiplier" className="flex-1">{t('pricing.tabMultiplier')}</TabsTrigger>
            </TabsList>

            {/* 基础：model/输入/输出必填 */}
            <TabsContent value="base" className="space-y-3 pt-3">
              <div className="space-y-1.5">
                <Label htmlFor="pf-model">{t('pricing.modelLabel')} <span className="text-destructive">*</span></Label>
                <Input
                  id="pf-model"
                  value={form.model}
                  placeholder={t('pricing.modelPlaceholder')}
                  onChange={e => { setForm(f => ({ ...f, model: e.target.value })); setFormErr(null) }}
                  disabled={!!editing}
                />
                {editing?.Source === 'litellm' && (
                  <p className="text-xs text-muted-foreground">{t('pricing.takeoverHint')}</p>
                )}
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div className="space-y-1.5">
                  <Label htmlFor="pf-prompt">{t('pricing.promptLabel')} <span className="text-destructive">*</span></Label>
                  <Input id="pf-prompt" type="number" min={0} value={form.prompt} onChange={e => { setForm(f => ({ ...f, prompt: e.target.value })); setFormErr(null) }} />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="pf-completion">{t('pricing.completionLabel')} <span className="text-destructive">*</span></Label>
                  <Input id="pf-completion" type="number" min={0} value={form.completion} onChange={e => { setForm(f => ({ ...f, completion: e.target.value })); setFormErr(null) }} />
                </div>
              </div>
            </TabsContent>

            {/* 缓存 */}
            <TabsContent value="cache" className="space-y-3 pt-3">
              <p className="text-xs text-muted-foreground">{t('pricing.cacheHint')}</p>
              <div className="grid grid-cols-2 gap-3">
                {CACHE_FIELDS.map(f => (
                  <div key={f.key} className="space-y-1.5">
                    <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                    <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                  </div>
                ))}
              </div>
            </TabsContent>

            {/* 档位：优先档 + 弹性档 */}
            <TabsContent value="tier" className="space-y-4 pt-3">
              <p className="text-xs text-muted-foreground">{t('pricing.tierHint')}</p>
              <div className="space-y-2">
                <p className="text-sm font-medium">{t('pricing.priorityGroup')}</p>
                <div className="grid grid-cols-2 gap-3">
                  {PRIORITY_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="text-sm font-medium">{t('pricing.flexGroup')}</p>
                <div className="grid grid-cols-2 gap-3">
                  {FLEX_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
            </TabsContent>

            {/* 分段：阈值 + 基础/优先档/弹性档三组分段价 */}
            <TabsContent value="segment" className="space-y-4 pt-3">
              <div className="space-y-1.5">
                <Label htmlFor="pf-above_threshold">{t('pricing.segmentThresholdLabel')}</Label>
                <Input id="pf-above_threshold" type="number" min={0} value={form.opt.above_threshold} onChange={e => setOpt('above_threshold', e.target.value)} />
                <p className="text-xs text-muted-foreground">{t('pricing.segmentThresholdHint')}</p>
              </div>
              <div className="space-y-2">
                <p className="text-sm font-medium">{t('pricing.segmentBaseGroup')}</p>
                <div className="grid grid-cols-2 gap-3">
                  {ABOVE_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="text-sm font-medium">{t('pricing.segmentPriorityGroup')}</p>
                <div className="grid grid-cols-2 gap-3">
                  {ABOVE_PRIORITY_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <div className="space-y-2">
                <p className="text-sm font-medium">{t('pricing.segmentFlexGroup')}</p>
                <div className="grid grid-cols-2 gap-3">
                  {ABOVE_FLEX_FIELDS.map(f => (
                    <div key={f.key} className="space-y-1.5">
                      <Label htmlFor={`pf-${f.key}`}>{t(`pricing.${f.tKey}`)}</Label>
                      <Input id={`pf-${f.key}`} type="number" min={0} value={form.opt[f.key]} onChange={e => setOpt(f.key, e.target.value)} />
                    </div>
                  ))}
                </div>
              </div>
              <p className="text-xs text-muted-foreground">{t('pricing.segmentHint')}</p>
            </TabsContent>

            {/* 倍率 */}
            <TabsContent value="multiplier" className="space-y-3 pt-3">
              <div className="space-y-1.5">
                <Label htmlFor="pf-fast_multiplier">{t('pricing.fastMultiplierLabel')}</Label>
                <Input id="pf-fast_multiplier" type="number" min={1} max={100000} value={form.opt.fast_multiplier} onChange={e => setOpt('fast_multiplier', e.target.value)} />
                <p className="text-xs text-muted-foreground">{t('pricing.fastMultiplierHint')}</p>
              </div>
            </TabsContent>
          </Tabs>
          {formErr && <p className="text-sm text-destructive">{formErr}</p>}
          {save.isError && errMsg(save.error) && (
            <p className="text-sm text-destructive">{errMsg(save.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>
              {save.isPending ? t('common.saving') : t('common.save')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* —— 删除确认（自动同步维护的行删除 → 服务端 409 报错就地展示） —— */}
      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !del.isPending) setDeleting(null) }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader>
            <DialogTitle>{t('pricing.deleteTitle')}</DialogTitle>
            <DialogDescription>{t('pricing.deleteDesc', { model: deleting?.Model })}</DialogDescription>
          </DialogHeader>
          {del.isError && errMsg(del.error) && (
            <p className="text-sm text-destructive">{errMsg(del.error)}</p>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setDeleting(null)} disabled={del.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && del.mutate(deleting)} disabled={del.isPending}>
              {del.isPending ? t('common.deleting') : t('pricing.deleteConfirm')}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion } from 'framer-motion'
import {
  Activity,
  CircleAlert,
  CircleCheck,
  CircleX,
  Gauge,
  Info,
  Pencil,
  Plus,
  RefreshCw,
  Route,
  Send,
  Trash2,
  WalletCards,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '@/App'
import {
  ApiError,
  ApiUnauthorized,
  type UpstreamCreateInput,
  type UpstreamModelsPreviewInput,
  type UpstreamRecord,
  type UpstreamStatus,
} from '@/lib/api/client'
import { useDebounced } from '@/lib/use-debounced'
import { formatDateTime } from '@/components/fmt'
import { ListToolbar } from '@/components/list-toolbar'
import { PagePagination } from '@/components/page-pagination'
import { SortableHeader, type SortOrder } from '@/components/sortable-header'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Skeleton } from '@/components/ui/skeleton'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { toast } from '@/components/ui/toast'
import { cn } from '@/lib/utils'
import { sortModelsLatestFirst } from '@/lib/model-sort'

type FormState = {
  name: string
  base_url: string
  upstream_key: string
  multiplier: string
  clear_upstream_key: boolean
}

type SaveArgs = {
  editingID: number | null
  body: UpstreamCreateInput
  preview?: UpstreamModelsPreviewInput
}

const EMPTY_FORM: FormState = {
  name: '',
  base_url: '',
  upstream_key: '',
  multiplier: '1',
  clear_upstream_key: false,
}

function recordEnabled(row: UpstreamRecord): boolean {
  if (typeof row.Enabled === 'boolean') return row.Enabled
  return row.Status === 'active'
}

function multiplierOf(row: UpstreamRecord): number {
  if (typeof row.MultiplierBP === 'number' && Number.isFinite(row.MultiplierBP)) return row.MultiplierBP / 10_000
  if (typeof row.Multiplier === 'number' && Number.isFinite(row.Multiplier)) return row.Multiplier
  return 1
}

function successRate(row: UpstreamRecord): number | null {
  const total = row.RequestCount ?? 0
  if (total <= 0) return null
  return Math.max(0, Math.min(1, (row.SuccessCount ?? 0) / total))
}

function averageLatency(row: UpstreamRecord): number | null {
  if (typeof row.AverageLatencyMS === 'number' && Number.isFinite(row.AverageLatencyMS)) return Math.max(0, row.AverageLatencyMS)
  const total = row.RequestCount ?? 0
  if (total <= 0 || row.LatencyTotalMS == null) return null
  return Math.max(0, row.LatencyTotalMS / total)
}

function stabilityKey(row: UpstreamRecord): 'excellent' | 'good' | 'fair' | 'poor' | 'unknown' {
  if (row.StabilityRating && ['excellent', 'good', 'fair', 'poor', 'unknown'].includes(row.StabilityRating)) return row.StabilityRating as ReturnType<typeof stabilityKey>
  const rate = successRate(row)
  if (rate == null) return 'unknown'
  const score = Math.max(0, Math.min(100, Math.floor(rate * 100)))
  const latency = averageLatency(row) ?? 0
  if (score >= 99 && latency <= 800) return 'excellent'
  if (score >= 97 && latency <= 1500) return 'good'
  if (score >= 90 && latency <= 3000) return 'fair'
  return 'poor'
}

function formatLatency(ms: number | null): string {
  if (ms == null) return '—'
  if (ms < 1000) return `${Math.round(ms)} ms`
  return `${(ms / 1000).toFixed(2)} s`
}

function formatMultiplier(value: number): string {
  return `×${value.toFixed(4).replace(/\.?(0+)$/, '')}`
}

function errorMessage(error: unknown): string | null {
  if (error == null) return null
  return error instanceof ApiUnauthorized ? null : (error as Error)?.message ?? String(error)
}

function isRevisionConflict(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409
}

function probeErrorLabel(code: string | null | undefined, t: (key: string) => string): string {
  const normalized = code?.trim() || 'unknown'
  const key = `upstreams.probeErrors.${normalized}`
  const translated = t(key)
  return translated === key ? normalized : translated
}

function statusBadge(row: UpstreamRecord, t: (key: string) => string) {
  const enabled = recordEnabled(row)
  return (
    <Badge variant={enabled ? 'secondary' : 'outline'} className={cn('gap-1.5', enabled ? 'text-emerald-700 dark:text-emerald-400' : 'text-muted-foreground')}>
      <span className={cn('size-1.5 rounded-full', enabled ? 'bg-emerald-500' : 'bg-muted-foreground/60')} />
      {t(enabled ? 'upstreams.status.active' : 'upstreams.status.disabled')}
    </Badge>
  )
}

function stabilityBadge(row: UpstreamRecord, t: (key: string) => string) {
  const key = stabilityKey(row)
  const styles: Record<typeof key, string> = {
    excellent: 'text-emerald-700 dark:text-emerald-400',
    good: 'text-blue-700 dark:text-blue-400',
    fair: 'text-amber-700 dark:text-amber-400',
    poor: 'text-red-700 dark:text-red-400',
    unknown: 'text-muted-foreground',
  }
  return <Badge variant="outline" className={cn('gap-1.5', styles[key])}><Activity className="size-3" />{t(`upstreams.stability.${key}`)}</Badge>
}

function balanceLabel(row: UpstreamRecord, t: (key: string) => string): { value: string; tone: string; staleLabel: string | null } {
  const amount = row.BalanceAmount ? formatBalanceAmount(row.BalanceAmount) : undefined
  const currency = row.BalanceCurrency?.trim()
  const status = row.BalanceStatus?.trim()
  if (amount) {
    const tone = status === 'unavailable'
      ? 'text-red-700 dark:text-red-400'
      : status === 'stale'
        ? 'text-amber-700 dark:text-amber-400'
        : 'text-foreground'
    return {
      value: `${amount}${currency ? ` ${currency}` : ''}`,
      tone,
      staleLabel: status === 'stale' ? t('upstreams.balanceStatus.stale') : null,
    }
  }
  if (status) {
    const translated = t(`upstreams.balanceStatus.${status}`)
    return { value: translated === `upstreams.balanceStatus.${status}` ? status : translated, tone: 'text-muted-foreground', staleLabel: null }
  }
  return { value: '—', tone: 'text-muted-foreground', staleLabel: null }
}

// Keep the provider value as a string for accounting; round only the visible
// dashboard value to two decimal places without going through a JS float.
function formatBalanceAmount(value: string): string {
  const raw = value.trim()
  const match = /^(\d+)(?:\.(\d+))?$/.exec(raw)
  if (!match) return raw
  const whole = match[1]
  const fraction = match[2] ?? ''
  const cents = (fraction + '00').slice(0, 2).split('').map(Number)
  if (fraction.length > 2 && fraction[2] >= '5') {
    let index = cents.length - 1
    while (index >= 0 && cents[index] === 9) {
      cents[index] = 0
      index -= 1
    }
    if (index < 0) return `${incrementWhole(whole)}.00`
    cents[index] += 1
  }
  return `${whole}.${cents.join('')}`
}

function incrementWhole(value: string): string {
  const digits = value.split('').map(Number)
  let index = digits.length - 1
  while (index >= 0 && digits[index] === 9) {
    digits[index] = 0
    index -= 1
  }
  if (index < 0) return `1${digits.join('')}`
  digits[index] += 1
  return digits.join('')
}

function balanceRefreshBlockReason(row: UpstreamRecord, t: (key: string) => string): string | null {
  if (!row.BalanceConfigured) return t('upstreams.balanceActionUnconfigured')
  if (row.BalanceAuth !== 'none' && row.CredentialConfigured !== true) return t('upstreams.balanceActionKeyMissing')
  return null
}

function toForm(row: UpstreamRecord): FormState {
  return {
    name: row.Name,
    base_url: row.BaseURL,
    // The API deliberately never returns a key. An empty value on update means keep it.
    upstream_key: '',
    multiplier: String(multiplierOf(row)),
    clear_upstream_key: false,
  }
}

function toBody(form: FormState, expectedUpdatedAt?: string): UpstreamCreateInput {
  // Keep the management form intentionally small. Omitted optional fields are
  // preserved by the API on edit, including existing balance configuration.
  const body: {
    name: string
    base_url: string
    multiplier_bp: number
    upstream_key?: string
    clear_upstream_key?: boolean
    expected_updated_at?: string
  } = {
    name: form.name.trim(),
    base_url: form.base_url.trim(),
    multiplier_bp: Math.round(Number(form.multiplier) * 10_000),
  }
  const key = form.upstream_key.trim()
  if (key) body.upstream_key = key
  if (form.clear_upstream_key && !key) body.clear_upstream_key = true
  if (expectedUpdatedAt) body.expected_updated_at = expectedUpdatedAt
  // The generated schema still marks legacy form defaults as required. The
  // server treats those fields as optional, so leave them out to preserve
  // existing values and keep the request aligned with the simplified UI.
  return body as UpstreamCreateInput
}

function normalizedRoot(value: string): string {
  return value.trim().replace(/\/+$/, '')
}

export default function Upstreams() {
  const { t } = useTranslation()
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const debouncedName = useDebounced(name, 300)
  const [status, setStatus] = useState<UpstreamStatus | 'all'>('all')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [sort, setSort] = useState('')
  const [order, setOrder] = useState<SortOrder>('desc')
  const [dialogOpen, setDialogOpen] = useState(false)
  const [editing, setEditing] = useState<UpstreamRecord | null>(null)
  const [deleting, setDeleting] = useState<UpstreamRecord | null>(null)
  const [form, setForm] = useState<FormState>(EMPTY_FORM)
  const [validation, setValidation] = useState<string | null>(null)
  const [pendingProbeIDs, setPendingProbeIDs] = useState<Set<number>>(() => new Set())
  const [pendingModelIDs, setPendingModelIDs] = useState<Set<number>>(() => new Set())
  const [pendingTestIDs, setPendingTestIDs] = useState<Set<number>>(() => new Set())
  const [pendingToggleIDs, setPendingToggleIDs] = useState<Set<number>>(() => new Set())
  const [modelDialog, setModelDialog] = useState<{ id: number; name: string; models: string[] } | null>(null)
  const [selectedModel, setSelectedModel] = useState('')

  const query = useQuery({
    queryKey: ['upstreams', { name: debouncedName, status, page, pageSize, sort, order }],
    queryFn: () => api.listUpstreams({
      name: debouncedName || undefined,
      status: status === 'all' ? undefined : status,
      limit: pageSize,
      offset: (page - 1) * pageSize,
      sort: sort || undefined,
      order,
    }),
    refetchInterval: 30_000,
  })

  const rows = useMemo(() => query.data?.items ?? [], [query.data])
  const total = query.data?.total ?? 0
  // Mutation confirmations must not race a stale query cache. Refetch the
  // currently visible list before closing dialogs or showing success.
  const refreshRows = async () => {
    await qc.refetchQueries({ queryKey: ['upstreams'], type: 'active' })
    // Model catalogues are used by group creation/editing. Any credential,
    // endpoint, or enablement change invalidates the cached catalogue too.
    await qc.invalidateQueries({ queryKey: ['group-upstream-models'] })
    // Keep the compact setup flow coherent across pages: after saving an
    // upstream, the group dialog must refetch its selectable upstream list
    // instead of serving the previous 30-second cache window.
    await qc.invalidateQueries({ queryKey: ['groups', 'upstream-options'] })
    await qc.invalidateQueries({ queryKey: ['upstreams', 'account-options'] })
  }

  // A deletion or filter change can leave the current page past the last page.
  // Clamp it so the list recovers instead of showing a misleading empty state.
  useEffect(() => {
    const lastPage = Math.max(1, Math.ceil(total / pageSize))
    if (page > lastPage) setPage(lastPage)
  }, [page, pageSize, total])

  const metrics = useMemo(() => {
    const active = rows.filter(recordEnabled).length
    const checked = rows.reduce((sum, row) => sum + Math.max(0, row.RequestCount ?? 0), 0)
    const successful = rows.reduce((sum, row) => sum + Math.max(0, row.SuccessCount ?? 0), 0)
    const totalLatency = rows.reduce((sum, row) => sum + Math.max(0, row.LatencyTotalMS ?? 0), 0)
    return {
      total,
      active,
      success: checked > 0 ? Math.min(1, successful / checked) : null,
      latency: checked > 0 ? totalLatency / checked : null,
    }
  }, [rows, total])

  const save = useMutation({
    mutationFn: async ({ editingID, body, preview }: SaveArgs) => {
      if (preview) {
        const result = await api.previewUpstreamModels(preview)
        if (!result.ok || !result.models?.length) {
          const code = probeErrorLabel(result.error_code, t)
          throw new Error(t('upstreams.modelsReadFailed', { code }))
        }
      }
      return editingID != null ? api.updateUpstream(editingID, body) : api.createUpstream(body)
    },
    onSuccess: async (_result, args) => {
      await refreshRows()
      setDialogOpen(false)
      toast.add({ title: t(args.editingID != null ? 'upstreams.saved' : 'upstreams.created'), type: 'success' })
    },
    onError: async error => {
      if (isRevisionConflict(error)) {
        await refreshRows()
        toast.add({ title: t('upstreams.staleUpdate'), type: 'warning' })
      } else {
        const message = errorMessage(error)
        if (message) toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
      }
    },
  })

  const remove = useMutation({
    mutationFn: (id: number) => api.deleteUpstream(id),
    onSuccess: async () => {
      await refreshRows()
      setDeleting(null)
      toast.add({ title: t('upstreams.deleted'), type: 'success' })
    },
    onError: error => {
      const message = errorMessage(error)
      if (message) toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
    },
  })

  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) => api.updateUpstreamStatus(id, enabled),
    onMutate: ({ id }) => {
      setPendingToggleIDs(current => new Set(current).add(id))
    },
    onSuccess: async () => {
      await refreshRows()
      toast.add({ title: t('upstreams.statusSaved'), type: 'success' })
    },
    onSettled: (_result, _error, variables) => {
      setPendingToggleIDs(current => {
        const next = new Set(current)
        next.delete(variables.id)
        return next
      })
    },
    onError: error => {
      const message = errorMessage(error)
      if (message) toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
    },
  })

  const probe = useMutation({
    mutationFn: ({ id, balance }: { id: number; balance: boolean }) => balance ? api.refreshUpstreamBalance(id) : api.probeUpstream(id),
    onMutate: ({ id }) => {
      setPendingProbeIDs(current => new Set(current).add(id))
    },
    onSuccess: async (result, args) => {
      await refreshRows()
      if (args.balance && result.error_code === 'unconfigured') {
        toast.add({ title: t('upstreams.balanceNotConfigured'), type: 'info' })
      } else if (!result.ok) {
        toast.add({ title: t(args.balance ? 'upstreams.balanceRefreshFailed' : 'upstreams.probeFailed', { code: probeErrorLabel(result.error_code, t) }), type: 'error' })
      } else {
        toast.add({ title: t(args.balance ? 'upstreams.balanceRefreshed' : 'upstreams.probed'), type: 'success' })
      }
    },
    onSettled: (_result, _error, variables) => {
      setPendingProbeIDs(current => {
        const next = new Set(current)
        next.delete(variables.id)
        return next
      })
    },
    onError: error => {
      const message = errorMessage(error)
      if (message) toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
    },
  })

  const runProbe = (id: number, balance: boolean) => {
    if (pendingProbeIDs.has(id)) return
    probe.mutate({ id, balance })
  }

  const test = useMutation({
    mutationFn: ({ id, model }: { id: number; model: string }) => api.testUpstream(id, model),
    onMutate: ({ id }) => {
      setPendingTestIDs(current => new Set(current).add(id))
    },
    onSuccess: async result => {
      await refreshRows()
      setModelDialog(null)
      const latency = formatLatency(result.latency_ms ?? null)
      if (result.ok) {
        toast.add({ title: t('upstreams.testSucceeded', { latency }), type: 'success' })
      } else {
        toast.add({ title: t('upstreams.testFailed', { latency, code: probeErrorLabel(result.error_code, t) }), type: 'error' })
      }
    },
    onSettled: (_result, _error, variables) => {
      setPendingTestIDs(current => {
        const next = new Set(current)
        next.delete(variables.id)
        return next
      })
    },
    onError: error => {
      const message = errorMessage(error)
      if (message) toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
    },
  })

  const loadModels = useMutation({
    mutationFn: (id: number) => api.listUpstreamModels(id),
    onMutate: id => {
      setPendingModelIDs(current => new Set(current).add(id))
    },
    onSuccess: (result, id) => {
      if (!result.ok || !result.models?.length) {
        toast.add({ title: t('upstreams.modelsReadFailed', { code: probeErrorLabel(result.error_code, t) }), type: 'error' })
        return
      }
      const row = rows.find(item => item.ID === id)
      const models = sortModelsLatestFirst(result.models)
      setSelectedModel(models[0])
      setModelDialog({ id, name: row?.Name ?? `#${id}`, models })
    },
    onSettled: (_result, _error, id) => {
      setPendingModelIDs(current => {
        const next = new Set(current)
        next.delete(id)
        return next
      })
    },
    onError: error => {
      const message = errorMessage(error)
      if (message) toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
    },
  })

  const runTest = (id: number) => {
    if (pendingTestIDs.has(id) || pendingModelIDs.has(id)) return
    test.reset()
    loadModels.mutate(id)
  }

  const confirmTest = () => {
    if (!modelDialog || !selectedModel.trim() || pendingTestIDs.has(modelDialog.id)) return
    test.mutate({ id: modelDialog.id, model: selectedModel.trim() })
  }

  const openCreate = () => {
    setEditing(null)
    setForm(EMPTY_FORM)
    setValidation(null)
    save.reset()
    setDialogOpen(true)
  }

  const openEdit = (row: UpstreamRecord) => {
    setEditing(row)
    setForm(toForm(row))
    setValidation(null)
    save.reset()
    setDialogOpen(true)
  }

  const submit = () => {
    if (!form.multiplier.trim()) {
      setValidation(t('upstreams.invalidMultiplier'))
      return
    }
    const multiplier = Number(form.multiplier)
    if (!form.name.trim() || !form.base_url.trim()) {
      setValidation(t('upstreams.formRequired'))
      return
    }
    if (!/^https?:\/\//i.test(form.base_url.trim())) {
      setValidation(t('upstreams.invalidUrl'))
      return
    }
    if (/\/v1(?:\/|$)/i.test(form.base_url.trim())) {
      setValidation(t('upstreams.invalidRoot'))
      return
    }
    if (!Number.isFinite(multiplier) || multiplier < 0 || multiplier > 100) {
      setValidation(t('upstreams.invalidMultiplier'))
      return
    }
    const endpointChanged = editing != null && normalizedRoot(editing.BaseURL) !== normalizedRoot(form.base_url)
    if (endpointChanged && editing.CredentialConfigured === true && !form.upstream_key.trim() && !form.clear_upstream_key) {
      setValidation(t('upstreams.keyRequiredForAddressChange'))
      return
    }
    setValidation(null)
    const body = toBody(form, editing?.UpdatedAt)
    const preview = editing == null
      ? { base_url: form.base_url.trim(), ...(form.upstream_key.trim() ? { upstream_key: form.upstream_key.trim() } : {}) }
      : undefined
    save.mutate({ editingID: editing?.ID ?? null, body, preview })
  }

  const toggleSort = (field: string) => {
    if (sort !== field) { setSort(field); setOrder('desc'); setPage(1); return }
    if (order === 'desc') { setOrder('asc'); setPage(1); return }
    setSort('')
    setOrder('desc')
    setPage(1)
  }

  const refresh = () => { void query.refetch() }
  const err = errorMessage(query.error)
  const endpointChanged = editing != null && normalizedRoot(editing.BaseURL) !== normalizedRoot(form.base_url)

  return (
    <motion.div className="space-y-6" initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('upstreams.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('upstreams.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="icon" title={t('common.refresh')} onClick={refresh} disabled={query.isFetching}>
            <RefreshCw className={cn(query.isFetching && 'animate-spin')} />
            <span className="sr-only">{t('common.refresh')}</span>
          </Button>
          <Button onClick={openCreate}><Plus />{t('upstreams.new')}</Button>
        </div>
      </div>

      <div className="flex items-start gap-2 rounded-md border border-border bg-muted/30 px-3 py-2.5 text-sm text-muted-foreground">
        <Info className="mt-0.5 size-4 shrink-0" />
        <p>{t('upstreams.scopeNotice')}</p>
      </div>

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Card size="sm"><CardHeader><CardTitle className="flex items-center gap-2 text-sm text-muted-foreground"><Route className="size-4" />{t('upstreams.metrics.total')}</CardTitle></CardHeader><CardContent className="text-2xl font-semibold tabular-nums">{metrics.total}</CardContent></Card>
        <Card size="sm"><CardHeader><CardTitle className="flex items-center gap-2 text-sm text-muted-foreground"><CircleCheck className="size-4" />{t('upstreams.metrics.activePage')}</CardTitle></CardHeader><CardContent className="text-2xl font-semibold tabular-nums">{metrics.active}</CardContent></Card>
        <Card size="sm"><CardHeader><CardTitle className="flex items-center gap-2 text-sm text-muted-foreground"><Activity className="size-4" />{t('upstreams.metrics.successPage')}</CardTitle></CardHeader><CardContent className="text-2xl font-semibold tabular-nums">{metrics.success == null ? '—' : `${(metrics.success * 100).toFixed(1)}%`}</CardContent></Card>
        <Card size="sm"><CardHeader><CardTitle className="flex items-center gap-2 text-sm text-muted-foreground"><Gauge className="size-4" />{t('upstreams.metrics.latencyPage')}</CardTitle></CardHeader><CardContent className="text-2xl font-semibold tabular-nums">{formatLatency(metrics.latency)}</CardContent></Card>
      </div>

      <ListToolbar name={name} onNameChange={v => { setName(v); setPage(1) }} placeholder={t('upstreams.search')}>
        <Select items={{ all: t('upstreams.status.all'), active: t('upstreams.status.active'), disabled: t('upstreams.status.disabled') }} value={status} onValueChange={v => { setStatus((v || 'all') as UpstreamStatus | 'all'); setPage(1) }}>
          <SelectTrigger size="sm" className="w-32" aria-label={t('upstreams.status.filter')}><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="all" label={t('upstreams.status.all')}>{t('upstreams.status.all')}</SelectItem>
            <SelectItem value="active" label={t('upstreams.status.active')}>{t('upstreams.status.active')}</SelectItem>
            <SelectItem value="disabled" label={t('upstreams.status.disabled')}>{t('upstreams.status.disabled')}</SelectItem>
          </SelectContent>
        </Select>
      </ListToolbar>

      {query.isError && err && <p className="text-sm text-destructive">{t('common.loadFailed', { message: err })}</p>}
      {query.isLoading ? (
        <div className="space-y-2">{Array.from({ length: 5 }).map((_, i) => <Skeleton key={i} className="h-14" />)}</div>
      ) : rows.length === 0 ? (
        <Card className="flex flex-col items-center gap-2 py-14 text-muted-foreground">
          <Route className="size-10" />
          <p className="font-medium">{debouncedName || status !== 'all' ? t('upstreams.noResultsTitle') : t('upstreams.emptyTitle')}</p>
          <p className="text-sm">{debouncedName || status !== 'all' ? t('upstreams.noResultsDesc') : t('upstreams.emptyDesc')}</p>
          {!debouncedName && status === 'all' && <Button className="mt-2" onClick={openCreate}><Plus />{t('upstreams.new')}</Button>}
        </Card>
      ) : (
        <>
          <div className="space-y-3 md:hidden">
            {rows.map(row => {
              const balance = balanceLabel(row, t)
              const balanceBlockedReason = balanceRefreshBlockReason(row, t)
              const latency = averageLatency(row)
              const active = recordEnabled(row)
              const pending = pendingProbeIDs.has(row.ID)
              return (
                <Card key={row.ID} size="sm" className="overflow-hidden">
                  <CardHeader className="space-y-2">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <CardTitle className="truncate text-base" title={row.Name}>{row.Name}</CardTitle>
                        <div className="text-xs text-muted-foreground">#{row.ID}</div>
                      </div>
                      {statusBadge(row, t)}
                    </div>
                    <div className="truncate font-mono text-xs text-muted-foreground" title={row.BaseURL}>{row.BaseURL}</div>
                    {row.CredentialConfigured != null && <div className={cn('text-xs', row.CredentialConfigured ? 'text-emerald-700 dark:text-emerald-400' : 'text-amber-700 dark:text-amber-400')}>{t(row.CredentialConfigured ? 'upstreams.credentialConfigured' : 'upstreams.credentialMissing')}</div>}
                    {row.Note && <div className="truncate text-xs text-muted-foreground/80" title={row.Note}>{row.Note}</div>}
                  </CardHeader>
                  <CardContent className="space-y-3">
                    <div className="grid grid-cols-2 gap-3 text-sm">
                      <div>
                        <div className="text-xs text-muted-foreground">{t('upstreams.table.multiplier')}</div>
                        <div className="font-semibold tabular-nums">{formatMultiplier(multiplierOf(row))}</div>
                      </div>
                      <div>
                        <div className="text-xs text-muted-foreground">{t('upstreams.table.balance')}</div>
                        <div className={cn('font-medium tabular-nums', balance.tone)}>{balance.value}</div>
                        {balance.staleLabel && <div className="mt-1 flex items-center gap-1 text-xs font-medium text-amber-700 dark:text-amber-400"><CircleAlert className="size-3 shrink-0" />{balance.staleLabel}</div>}
                        <div className="text-xs text-muted-foreground">{row.BalanceCheckedAt ? formatDateTime(row.BalanceCheckedAt) : t('upstreams.notChecked')}</div>
                      </div>
                      <div>
                        <div className="text-xs text-muted-foreground">{t('upstreams.table.stability')}</div>
                        {stabilityBadge(row, t)}
                        <div className="mt-1 text-xs text-muted-foreground">{t('upstreams.manualCheckHistory')}</div>
                      </div>
                      <div>
                        <div className="text-xs text-muted-foreground">{t('upstreams.table.latency')}</div>
                        <div className="tabular-nums">{formatLatency(latency)}</div>
                        <div className="text-xs text-muted-foreground">{row.LastCheckedAt ? formatDateTime(row.LastCheckedAt) : t('upstreams.notChecked')}</div>
                      </div>
                    </div>
                    {row.LastError && <div className="flex items-center gap-1 text-xs text-destructive"><CircleX className="size-3 shrink-0" />{probeErrorLabel(row.LastError, t)}</div>}
                    {balanceBlockedReason && row.BalanceConfigured && <div className="text-xs text-amber-700 dark:text-amber-400">{balanceBlockedReason}</div>}
                    <div className="flex items-center justify-between gap-2 border-t pt-3">
                      <div className="flex items-center gap-1">
                       <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.test')} onClick={() => runTest(row.ID)} disabled={pendingTestIDs.has(row.ID) || pendingModelIDs.has(row.ID)}>
                          <Send className={cn((pendingTestIDs.has(row.ID) || pendingModelIDs.has(row.ID)) && 'animate-pulse')} />
                          <span className="sr-only">{t('upstreams.actions.test')}</span>
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.probe')} onClick={() => runProbe(row.ID, false)} disabled={pending}>
                          <Activity className={cn(pending && 'animate-pulse')} />
                          <span className="sr-only">{t('upstreams.actions.probe')}</span>
                        </Button>
                        <span title={balanceBlockedReason ?? t('upstreams.actions.balance')}>
                          <Button variant="ghost" size="icon-sm" title={balanceBlockedReason ?? t('upstreams.actions.balance')} onClick={() => runProbe(row.ID, true)} disabled={pending || balanceBlockedReason != null}>
                            <WalletCards className={cn(pending && 'animate-pulse')} />
                            <span className="sr-only">{t('upstreams.actions.balance')}</span>
                          </Button>
                        </span>
                      </div>
                      <div className="flex items-center gap-1">
                        <Switch checked={active} onCheckedChange={checked => toggle.mutate({ id: row.ID, enabled: checked })} disabled={pendingToggleIDs.has(row.ID)} aria-label={t(active ? 'upstreams.actions.disable' : 'upstreams.actions.enable')} />
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(row)}><Pencil /><span className="sr-only">{t('common.edit')}</span></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => { remove.reset(); setDeleting(row) }}><Trash2 /><span className="sr-only">{t('common.delete')}</span></Button>
                      </div>
                    </div>
                  </CardContent>
                </Card>
              )
            })}
          </div>

          <div className="hidden md:block">
          <Table>
            <TableHeader>
              <TableRow>
                <SortableHeader field="name" label={t('upstreams.table.name')} active={sort === 'name'} order={order} onToggle={toggleSort} />
                <TableHead>{t('upstreams.table.endpoint')}</TableHead>
                <SortableHeader field="multiplier_bp" label={t('upstreams.table.multiplier')} active={sort === 'multiplier_bp'} order={order} onToggle={toggleSort} />
                <TableHead>{t('upstreams.table.balance')}</TableHead>
                <TableHead>{t('upstreams.table.stability')}</TableHead>
                <TableHead>{t('upstreams.table.latency')}</TableHead>
                <TableHead>{t('upstreams.table.status')}</TableHead>
                <TableHead className="text-right">{t('upstreams.table.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody className="[&_td]:py-3">
              {rows.map(row => {
                const balance = balanceLabel(row, t)
                const balanceBlockedReason = balanceRefreshBlockReason(row, t)
                const latency = averageLatency(row)
                const active = recordEnabled(row)
                return (
                  <TableRow key={row.ID}>
                    <TableCell className="max-w-40">
                      <div className="min-w-0">
                        <div className="truncate font-medium" title={row.Name}>{row.Name}</div>
                        <div className="text-xs text-muted-foreground">#{row.ID}</div>
                      </div>
                    </TableCell>
                    <TableCell className="max-w-64">
                      <div className="truncate font-mono text-xs text-muted-foreground" title={row.BaseURL}>{row.BaseURL}</div>
                      {row.CredentialConfigured != null && <div className={cn('text-xs', row.CredentialConfigured ? 'text-emerald-700 dark:text-emerald-400' : 'text-amber-700 dark:text-amber-400')}>{t(row.CredentialConfigured ? 'upstreams.credentialConfigured' : 'upstreams.credentialMissing')}</div>}
                      {row.Note && <div className="max-w-56 truncate text-xs text-muted-foreground/80" title={row.Note}>{row.Note}</div>}
                    </TableCell>
                    <TableCell className="font-semibold tabular-nums">{formatMultiplier(multiplierOf(row))}</TableCell>
                    <TableCell>
                      <div className={cn('font-medium tabular-nums', balance.tone)}>{balance.value}</div>
                      {balance.staleLabel && <div className="mt-1 flex items-center gap-1 text-xs font-medium text-amber-700 dark:text-amber-400"><CircleAlert className="size-3 shrink-0" />{balance.staleLabel}</div>}
                      {balanceBlockedReason && row.BalanceConfigured && <div className="mt-1 text-xs text-amber-700 dark:text-amber-400">{balanceBlockedReason}</div>}
                      <div className="text-xs text-muted-foreground">{row.BalanceCheckedAt ? formatDateTime(row.BalanceCheckedAt) : t('upstreams.notChecked')}</div>
                    </TableCell>
                    <TableCell>
                      {stabilityBadge(row, t)}
                      <div className="mt-1 text-xs text-muted-foreground" title={t('upstreams.stabilityHint')}>{t('upstreams.manualCheckHistory')}</div>
                      <div className="text-xs text-muted-foreground tabular-nums">{row.RequestCount ?? 0} · {successRate(row) == null ? '—' : `${(successRate(row)! * 100).toFixed(1)}%`}</div>
                    </TableCell>
                    <TableCell>
                      <div className="tabular-nums">{formatLatency(latency)}</div>
                      <div className="text-xs text-muted-foreground">{row.LastCheckedAt ? formatDateTime(row.LastCheckedAt) : t('upstreams.notChecked')}</div>
                    </TableCell>
                    <TableCell>
                      {statusBadge(row, t)}
                      {row.LastError && <div className="mt-1 flex max-w-36 items-center gap-1 truncate text-xs text-destructive" title={probeErrorLabel(row.LastError, t)}><CircleX className="size-3 shrink-0" />{probeErrorLabel(row.LastError, t)}</div>}
                    </TableCell>
                    <TableCell className="text-right">
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.test')} onClick={() => runTest(row.ID)} disabled={pendingTestIDs.has(row.ID) || pendingModelIDs.has(row.ID)}>
                           <Send className={cn((pendingTestIDs.has(row.ID) || pendingModelIDs.has(row.ID)) && 'animate-pulse')} />
                          <span className="sr-only">{t('upstreams.actions.test')}</span>
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.probe')} onClick={() => runProbe(row.ID, false)} disabled={pendingProbeIDs.has(row.ID)}>
                          <Activity className={cn(pendingProbeIDs.has(row.ID) && 'animate-pulse')} />
                          <span className="sr-only">{t('upstreams.actions.probe')}</span>
                        </Button>
                        <span title={balanceBlockedReason ?? t('upstreams.actions.balance')}>
                          <Button variant="ghost" size="icon-sm" title={balanceBlockedReason ?? t('upstreams.actions.balance')} onClick={() => runProbe(row.ID, true)} disabled={pendingProbeIDs.has(row.ID) || balanceBlockedReason != null}>
                            <WalletCards className={cn(pendingProbeIDs.has(row.ID) && 'animate-pulse')} />
                            <span className="sr-only">{t('upstreams.actions.balance')}</span>
                          </Button>
                        </span>
                        <Switch checked={active} onCheckedChange={checked => toggle.mutate({ id: row.ID, enabled: checked })} disabled={pendingToggleIDs.has(row.ID)} aria-label={t(active ? 'upstreams.actions.disable' : 'upstreams.actions.enable')} />
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(row)}><Pencil /><span className="sr-only">{t('common.edit')}</span></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => { remove.reset(); setDeleting(row) }}><Trash2 /><span className="sr-only">{t('common.delete')}</span></Button>
                      </div>
                    </TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
          </div>
          <PagePagination total={total} pageSize={pageSize} page={page} pageSizes={[10, 20, 50, 100, 200]} onPageChange={setPage} onPageSizeChange={size => { setPageSize(size); setPage(1) }} />
        </>
      )}

      <Dialog open={dialogOpen} onOpenChange={o => { if (!o && save.isPending) return; setDialogOpen(o); if (!o) setValidation(null) }}>
        <DialogContent className="top-4 max-h-[calc(100dvh-2rem)] translate-y-0 overflow-y-auto sm:top-1/2 sm:max-w-2xl sm:-translate-y-1/2">
          <DialogHeader>
            <DialogTitle>{editing ? t('upstreams.editTitle', { id: editing.ID }) : t('upstreams.newTitle')}</DialogTitle>
            <DialogDescription>{t('upstreams.dialogDesc')}</DialogDescription>
          </DialogHeader>
          <div className="space-y-3">
            <div className="grid gap-3 sm:grid-cols-2">
              <div className="space-y-1.5"><Label htmlFor="up-name">{t('upstreams.form.name')}</Label><Input id="up-name" value={form.name} onChange={e => setForm(f => ({ ...f, name: e.target.value }))} /></div>
              <div className="space-y-1.5"><Label htmlFor="up-mult">{t('upstreams.form.multiplier')}</Label><Input id="up-mult" type="number" min={0} step="0.0001" value={form.multiplier} onChange={e => setForm(f => ({ ...f, multiplier: e.target.value }))} /><p className="text-xs text-muted-foreground">{t('upstreams.form.multiplierHint')}</p></div>
            </div>
            <div className="space-y-1.5"><Label htmlFor="up-url">{t('upstreams.form.endpoint')}</Label><Input id="up-url" type="url" placeholder="https://relay.example.com" value={form.base_url} onChange={e => setForm(f => ({ ...f, base_url: e.target.value }))} /></div>
            <div className="space-y-1.5">
              <Label htmlFor="up-key">{t('upstreams.form.key')}</Label>
              <Input id="up-key" type="password" autoComplete="new-password" placeholder={editing ? t('upstreams.form.keyKeep') : 'sk-…'} value={form.upstream_key} onChange={e => setForm(f => ({ ...f, upstream_key: e.target.value, clear_upstream_key: false }))} />
              <p className="text-xs text-muted-foreground">{t('upstreams.form.keyHint')}</p>
              {editing?.CredentialConfigured === true && (
                <div className="flex items-center gap-2 pt-1">
                  <Switch
                    id="up-clear-key"
                    checked={form.clear_upstream_key}
                    onCheckedChange={checked => setForm(f => ({ ...f, clear_upstream_key: checked, upstream_key: checked ? '' : f.upstream_key }))}
                  />
                  <Label htmlFor="up-clear-key" className="text-xs font-normal">{t('upstreams.form.clearKey')}</Label>
                </div>
              )}
              {endpointChanged && editing?.CredentialConfigured && <p className="text-xs text-amber-700 dark:text-amber-400">{t('upstreams.form.endpointChangedHint')}</p>}
            </div>
            <p className="rounded-md border border-dashed border-border px-3 py-2 text-xs text-muted-foreground">{t('upstreams.balanceAutoHint')}</p>
            {validation && <p className="text-sm text-destructive">{validation}</p>}
            {save.isError && <p className="text-sm text-destructive">{isRevisionConflict(save.error) ? t('upstreams.staleUpdate') : errorMessage(save.error)}</p>}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending}>{save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={modelDialog != null} onOpenChange={o => { if (!o && !test.isPending) setModelDialog(null) }}>
        <DialogContent className="sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t('upstreams.selectModelTitle')}</DialogTitle>
            <DialogDescription>{t('upstreams.selectModelDesc', { name: modelDialog?.name ?? '' })}</DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="up-test-model">{t('upstreams.testModelLabel')}</Label>
            <Select
              items={Object.fromEntries((modelDialog?.models ?? []).map(model => [model, model]))}
              value={selectedModel}
              onValueChange={value => setSelectedModel(value ?? '')}
              disabled={test.isPending || modelDialog == null}
            >
              <SelectTrigger id="up-test-model" className="w-full"><SelectValue placeholder={t('upstreams.selectModelPlaceholder')} /></SelectTrigger>
              <SelectContent>
                {(modelDialog?.models ?? []).map(model => <SelectItem key={model} value={model} label={model}>{model}</SelectItem>)}
              </SelectContent>
            </Select>
          </div>
          {test.isError && errorMessage(test.error) && <p className="text-sm text-destructive">{errorMessage(test.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => setModelDialog(null)} disabled={test.isPending}>{t('common.cancel')}</Button>
            <Button onClick={confirmTest} disabled={!selectedModel.trim() || test.isPending}>{test.isPending ? t('common.testing') : t('upstreams.startTest')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) { remove.reset(); setDeleting(null) } }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader><DialogTitle>{t('upstreams.deleteTitle')}</DialogTitle><DialogDescription>{t('upstreams.deleteDesc', { name: deleting?.Name })}</DialogDescription></DialogHeader>
          {remove.isError && errorMessage(remove.error) && <p className="text-sm text-destructive">{errorMessage(remove.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => { remove.reset(); setDeleting(null) }} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => deleting && remove.mutate(deleting.ID)} disabled={remove.isPending}>{remove.isPending ? t('common.deleting') : t('common.confirmDelete')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </motion.div>
  )
}

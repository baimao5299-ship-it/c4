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
  ListChecks,
  Pencil,
  Plus,
  RefreshCw,
  Route,
  Trash2,
  WalletCards,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { TFunction } from 'i18next'
import { api } from '@/App'
import {
  ApiError,
  ApiUnauthorized,
  type UpstreamCreateInput,
  type UpstreamBatchValidationResponse,
  type UpstreamBatchValidationItem,
  type UpstreamRecord,
  type UpstreamStatus,
  type UpstreamValidationTaskResponse,
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
import { formatMultiplierValue, isStorableMultiplier, multiplierFromApi } from '@/lib/multiplier'
import { ModelValidationProgress } from '@/components/model-validation-progress'

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
}

type ModelValidationResult =
  | { ok: true }
  | { ok: false; reason: string }

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
  return multiplierFromApi(row.Multiplier, row.MultiplierBP)
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
  return formatMultiplierValue(value)
}

function errorMessage(error: unknown): string | null {
  if (error == null) return null
  return error instanceof ApiUnauthorized ? null : (error as Error)?.message ?? String(error)
}

function batchValidationErrorMessage(error: unknown, t: (key: string) => string): string | null {
  if (error instanceof ApiError && error.status === 409) return t('upstreams.batchValidationConflict')
  return errorMessage(error)
}

function isRetryableValidationPollError(error: unknown): boolean {
  if (error instanceof ApiUnauthorized) return false
  if (error instanceof ApiError) {
    return error.status === 408 || error.status === 425 || error.status === 429 || error.status >= 500
  }
  // Fetch reports a connection reset, DNS failure, or browser offline state as
  // a plain TypeError. The task itself is still running on the server, so a
  // later poll can recover the real status instead of showing a false failure.
  return error instanceof TypeError
}

function modelValidationErrorCode(error: unknown): string | null {
  const message = errorMessage(error)
  if (!message) return null
  const match = message.match(/upstream model validation failed\s*\(([^)]+)\)/i)
  return match?.[1]?.trim() || null
}

function isRevisionConflict(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && (error.code === 'revision_conflict' || (!error.code && /(?:id=\d+ changed|configuration changed)/i.test(error.message)))
}

function isDuplicateNameConflict(error: unknown): boolean {
  return error instanceof ApiError && error.status === 409 && (error.code === 'duplicate_name' || (!error.code && /name=/.test(error.message)))
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
    // The service accepts both a bare root and a copied `/v1` endpoint. Send
    // one canonical spelling so an equivalent edit does not trigger another
    // model validation pass or reset the saved telemetry.
    base_url: normalizedRoot(form.base_url),
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
  const trimmed = value.trim().replace(/\/+$/, '')
  // Users often paste an operation URL copied from a client or provider
  // docs. The service stores/probes the upstream root, so compare all
  // supported operation spellings as the same connection before deciding
  // that a replacement key is required.
  return trimmed
    .replace(/\/(?:chat\/completions|responses|messages)$/i, '')
    .replace(/\/v1$/i, '')
}

function modelsForDisplay(row: UpstreamRecord): string[] {
  return sortModelsLatestFirst(row.Models ?? [])
}

function modelsValidationNotice(row: UpstreamRecord, t: TFunction): string | null {
  if (!row.ModelsError) return null
  const models = modelsForDisplay(row)
  const code = probeErrorLabel(row.ModelsError, t)
  // A stored model snapshot can remain routable after a transient or
  // model-specific failure. Calling that state simply "error" made operators
  // discard models they had already tested successfully.
  return models.length > 0
    ? t('upstreams.modelsValidationPartial', { code, count: models.length })
    : t('upstreams.modelsValidationState', { code })
}

function batchItemName(item: UpstreamBatchValidationItem): string {
  return item.upstream.Name?.trim() || `#${item.upstream.ID}`
}

function batchItemModels(item: UpstreamBatchValidationItem): string[] {
  // The server returns only models that answered a real request successfully.
  // Keep those names visible even when another model timed out; hiding them
  // made a partial run look like every model was unavailable.
  // Older servers did not include the item-level `models` field. In that case
  // use the nested row snapshot, but never replace an explicit empty result:
  // an empty array is a definitive "no model passed" outcome.
  const itemModels = Array.isArray(item.models) ? item.models : null
  const snapshotModels = item.upstream?.Models ?? []
  // A few older task responses returned an empty item list for an incomplete
  // row while still carrying the last verified snapshot on `upstream`. Use it
  // only for an incomplete result; an explicit empty complete result remains
  // authoritative and must not be presented as usable.
  const models = itemModels == null || (itemModels.length === 0 && item.validation_complete === false && snapshotModels.length > 0)
    ? snapshotModels
    : itemModels
  return sortModelsLatestFirst(models)
}

function batchItemCount(item: UpstreamBatchValidationItem, field: 'models_total' | 'models_available' | 'models_failed'): number | null {
  const value = item[field]
  return typeof value === 'number' && Number.isFinite(value) ? Math.max(0, value) : null
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
  const [pendingToggleIDs, setPendingToggleIDs] = useState<Set<number>>(() => new Set())
  const [batchValidationOpen, setBatchValidationOpen] = useState(false)
  const [batchValidationResult, setBatchValidationResult] = useState<UpstreamBatchValidationResponse | null>(null)
  const [batchValidationProgress, setBatchValidationProgress] = useState<UpstreamValidationTaskResponse | null>(null)

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
  const refreshRows = async (): Promise<boolean> => {
    let refreshed = true
    try {
      // Refetch the visible query directly so the result tells us whether the
      // list actually refreshed. QueryClient.refetchQueries intentionally
      // resolves even when a query fails, which used to leave a stale list
      // looking like a successful mutation.
      const result = await query.refetch()
      refreshed = !result.isError
      // Model catalogues are used by group creation/editing. Any credential,
      // endpoint, or enablement change invalidates the cached catalogue too.
      await qc.invalidateQueries({ queryKey: ['group-upstream-models'] })
      // Keep the compact setup flow coherent across pages: after saving an
      // upstream, the group dialog must refetch its selectable upstream list
      // instead of serving the previous 30-second cache window.
      await qc.invalidateQueries({ queryKey: ['groups', 'upstream-options'] })
      await qc.invalidateQueries({ queryKey: ['upstreams', 'account-options'] })
    } catch {
      refreshed = false
    }
    return refreshed
  }

  const warnRefreshFailure = () => {
    toast.add({ title: t('dashboard.loadFailedWarning'), type: 'warning' })
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

  const validateSavedUpstream = async (id: number, reportFailure = true): Promise<ModelValidationResult> => {
    setPendingModelIDs(current => new Set(current).add(id))
    try {
      const result = await api.listUpstreamModels(id)
      const hasModels = Array.isArray(result.models) && result.models.length > 0
      if (!hasModels) {
        const reason = probeErrorLabel(result.error_code, t)
        if (reportFailure) toast.add({ title: t('upstreams.modelsReadFailed', { code: reason }), type: 'error' })
        return { ok: false, reason }
      }
      if ((result.ok !== true || result.validation_complete !== true) && reportFailure) {
        toast.add({ title: t('upstreams.modelsPartiallyRead'), type: 'warning' })
      }
      return { ok: true }
    } catch (error) {
      const reason = batchValidationErrorMessage(error, t) ?? t('upstreams.probeErrors.unknown')
      if (reportFailure) toast.add({ title: t('upstreams.actionFailed', { message: reason }), type: 'error' })
      return { ok: false, reason }
    } finally {
      setPendingModelIDs(current => {
        const next = new Set(current)
        next.delete(id)
        return next
      })
    }
  }

  const refreshModelList = async (id: number) => {
    if (upstreamBusy(id)) return
    await validateSavedUpstream(id)
    const refreshed = await refreshRows()
    if (!refreshed) warnRefreshFailure()
  }

  const save = useMutation({
    mutationFn: async (args: SaveArgs) => {
      return args.editingID != null ? api.updateUpstream(args.editingID, args.body) : api.createUpstream(args.body)
    },
    onSuccess: async (_result, args) => {
      // Creation and endpoint/key edits are validated by the server before the
      // database write, so one save never performs a duplicate paid probe.
      const refreshed = await refreshRows()
      setDialogOpen(false)
      if (!refreshed) warnRefreshFailure()
      if (refreshed) {
        toast.add({ title: t(args.editingID != null ? 'upstreams.saved' : 'upstreams.created'), type: 'success' })
      }
    },
    onError: async error => {
      if (isRevisionConflict(error)) {
        await refreshRows()
        toast.add({ title: t('upstreams.staleUpdate'), type: 'warning' })
      } else if (isDuplicateNameConflict(error)) {
        toast.add({ title: t('upstreams.duplicateName'), type: 'error' })
      } else {
        const message = errorMessage(error)
        const validationCode = modelValidationErrorCode(error)
        if (validationCode) {
          toast.add({ title: t('upstreams.savedValidationFailed', { reason: probeErrorLabel(validationCode, t) }), type: 'warning' })
        } else if (message) {
          toast.add({ title: t('upstreams.actionFailed', { message }), type: 'error' })
        }
      }
    },
  })

  const remove = useMutation({
    mutationFn: (id: number) => api.deleteUpstream(id),
    onSuccess: async () => {
      const refreshed = await refreshRows()
      setDeleting(null)
      if (refreshed) toast.add({ title: t('upstreams.deleted'), type: 'success' })
      else warnRefreshFailure()
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
      const refreshed = await refreshRows()
      if (refreshed) toast.add({ title: t('upstreams.statusSaved'), type: 'success' })
      else warnRefreshFailure()
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
      const refreshed = await refreshRows()
      if (!refreshed) warnRefreshFailure()
      if (args.balance && result.error_code === 'unconfigured') {
        toast.add({ title: t('upstreams.balanceNotConfigured'), type: 'info' })
      } else if (!result.ok) {
        toast.add({ title: t(args.balance ? 'upstreams.balanceRefreshFailed' : 'upstreams.probeFailed', { code: probeErrorLabel(result.error_code, t) }), type: 'error' })
      } else if (refreshed) {
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

  const batchValidation = useMutation({
    mutationFn: async (): Promise<UpstreamBatchValidationResponse> => {
      const started = await api.startValidateAllUpstreams()
      // The server allows a larger catalogue to finish serially without
      // turning the tail into an automatic false negative. Keep polling past
      // that bounded server budget plus a small network margin.
      const deadline = Date.now() + 17 * 60_000
      while (Date.now() < deadline) {
        let progress: UpstreamValidationTaskResponse
        try {
          progress = await api.getValidateAllUpstreamsTask(started.task_id)
          setBatchValidationProgress(progress)
        } catch (error) {
          if (!isRetryableValidationPollError(error)) throw error
          // A single lost poll is not a failed validation. Keep the last real
          // counters on screen and retry with a small bounded backoff.
          await new Promise(resolve => window.setTimeout(resolve, 750))
          continue
        }
        if (progress.status === 'completed' && progress.result) return progress.result
        if (progress.status === 'failed') {
          throw new ApiError(500, progress.error || t('upstreams.batchValidationFailedGeneric'))
        }
        await new Promise(resolve => window.setTimeout(resolve, 500))
      }
      throw new ApiError(504, t('upstreams.batchValidationTimedOut'))
    },
    onMutate: () => {
      // Open immediately so a slow upstream cannot look like a dead button.
      setBatchValidationResult(null)
      setBatchValidationProgress(null)
      setBatchValidationOpen(true)
    },
    onSuccess: async result => {
      setBatchValidationResult(result)
      const refreshed = await refreshRows()
      const incomplete = Math.max(0, result.total - result.completed)
      toast.add({
        title: t(incomplete > 0 ? 'upstreams.batchValidationFinishedIncomplete' : 'upstreams.batchValidationFinished', {
          passed: result.passed,
          failed: result.failed,
          incomplete,
        }),
        type: result.failed > 0 || incomplete > 0 ? 'warning' : 'success',
      })
      if (!refreshed) toast.add({ title: t('upstreams.batchValidationRefreshFailed'), type: 'warning' })
    },
    onError: error => {
      const message = batchValidationErrorMessage(error, t)
      if (message) toast.add({ title: t('upstreams.batchValidationFailed', { message }), type: 'error' })
    },
  })

  // Every operation that can change or inspect an upstream shares one row-level
  // busy predicate.  The backend serializes model probes, so allowing a second
  // action for the same row only creates an avoidable 409 race.
  const upstreamBusy = (id: number): boolean =>
    pendingModelIDs.has(id) ||
    pendingProbeIDs.has(id) ||
    pendingToggleIDs.has(id) ||
    (save.isPending && editing?.ID === id) ||
    (remove.isPending && deleting?.ID === id)

  const runProbe = (id: number, balance: boolean) => {
    if (batchValidation.isPending || upstreamBusy(id)) return
    probe.mutate({ id, balance })
  }

  const openCreate = () => {
    if (batchValidation.isPending || hasPendingAction) return
    setEditing(null)
    setForm(EMPTY_FORM)
    setValidation(null)
    save.reset()
    setDialogOpen(true)
  }

  const openEdit = (row: UpstreamRecord) => {
    if (batchValidation.isPending || upstreamBusy(row.ID)) return
    setEditing(row)
    setForm(toForm(row))
    setValidation(null)
    save.reset()
    setDialogOpen(true)
  }

  const submit = () => {
    if (batchValidation.isPending || (editing == null && hasPendingAction)) return
    if (editing != null && upstreamBusy(editing.ID)) return
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
    if (!isStorableMultiplier(multiplier, 100)) {
      setValidation(t('upstreams.invalidMultiplier'))
      return
    }
    const endpointChanged = editing != null && normalizedRoot(editing.BaseURL) !== normalizedRoot(form.base_url)
    if (endpointChanged && editing.CredentialConfigured === true && !form.upstream_key.trim() && !form.clear_upstream_key) {
      setValidation(t('upstreams.keyRequiredForAddressChange'))
      return
    }
    setValidation(null)
    const credentialChanged = editing != null &&
      (form.upstream_key.trim().length > 0 || (form.clear_upstream_key && editing.CredentialConfigured === true))
    const connectionChanged = endpointChanged || credentialChanged
    // A row's UpdatedAt also moves when health telemetry is recorded. Only
    // connection edits need the optimistic revision guard; metadata edits can
    // safely proceed while probes run in the background.
    const body = toBody(form, connectionChanged ? editing?.UpdatedAt : undefined)
    save.mutate({
      editingID: editing?.ID ?? null,
      body,
    })
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
  const editingBusy = editing != null && upstreamBusy(editing.ID)
  const hasPendingAction = save.isPending || remove.isPending || toggle.isPending || probe.isPending || pendingModelIDs.size > 0 || pendingProbeIDs.size > 0 || pendingToggleIDs.size > 0
  const batchLocked = batchValidation.isPending

  return (
    <motion.div className="space-y-6" initial={{ opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.25 }}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">{t('upstreams.title')}</h1>
          <p className="text-sm text-muted-foreground">{t('upstreams.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={() => { if (!batchLocked && !hasPendingAction) batchValidation.mutate() }} disabled={batchLocked || hasPendingAction}>
            <ListChecks className={cn(batchValidation.isPending && 'animate-pulse')} />
            <span>{batchValidation.isPending ? t('upstreams.validatingAll') : t('upstreams.validateAll')}</span>
          </Button>
          <Button variant="outline" size="icon" title={t('common.refresh')} onClick={refresh} disabled={query.isFetching || batchLocked}>
            <RefreshCw className={cn(query.isFetching && 'animate-spin')} />
            <span className="sr-only">{t('common.refresh')}</span>
          </Button>
          <Button onClick={openCreate} disabled={batchLocked || hasPendingAction}><Plus />{t('upstreams.new')}</Button>
        </div>
      </div>

      {batchLocked && <p className="rounded-md border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-sm text-amber-800 dark:text-amber-300">{t('upstreams.batchValidationLocks')}</p>}

      <div className="flex items-start gap-2 rounded-md border border-border bg-muted/30 px-3 py-2.5 text-sm text-muted-foreground">
        <Info className="mt-0.5 size-4 shrink-0" />
        <p>{t('upstreams.scopeNotice')}</p>
      </div>

      {pendingModelIDs.size > 0 && (
        <div className="rounded-md border border-primary/20 bg-primary/5 px-3 py-2.5">
          <ModelValidationProgress />
        </div>
      )}

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
          {!debouncedName && status === 'all' && <Button className="mt-2" onClick={openCreate} disabled={batchLocked}><Plus />{t('upstreams.new')}</Button>}
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
              const models = modelsForDisplay(row)
              const modelNotice = modelsValidationNotice(row, t)
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
                    <div className="space-y-1">
                      <div className="text-xs text-muted-foreground">{t('upstreams.supportedModels')}</div>
                      <div className="break-words font-mono text-xs" title={models.join(', ')}>{models.length ? models.join(', ') : t('upstreams.modelsNotVerified')}</div>
                      {modelNotice && <div className="text-xs text-amber-700 dark:text-amber-400">{modelNotice}</div>}
                    </div>
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
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.refreshModels')} onClick={() => { void refreshModelList(row.ID) }} disabled={batchLocked || upstreamBusy(row.ID)}>
                          <RefreshCw className={cn(pendingModelIDs.has(row.ID) && 'animate-spin')} />
                          <span className="sr-only">{t('upstreams.actions.refreshModels')}</span>
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.probe')} onClick={() => runProbe(row.ID, false)} disabled={batchLocked || upstreamBusy(row.ID)}>
                          <Activity className={cn(pending && 'animate-pulse')} />
                          <span className="sr-only">{t('upstreams.actions.probe')}</span>
                        </Button>
                        <span title={balanceBlockedReason ?? t('upstreams.actions.balance')}>
                          <Button variant="ghost" size="icon-sm" title={balanceBlockedReason ?? t('upstreams.actions.balance')} onClick={() => runProbe(row.ID, true)} disabled={batchLocked || upstreamBusy(row.ID) || balanceBlockedReason != null}>
                            <WalletCards className={cn(pending && 'animate-pulse')} />
                            <span className="sr-only">{t('upstreams.actions.balance')}</span>
                          </Button>
                        </span>
                      </div>
                      <div className="flex items-center gap-1">
                        <Switch checked={active} onCheckedChange={checked => { if (!upstreamBusy(row.ID)) toggle.mutate({ id: row.ID, enabled: checked }) }} disabled={batchLocked || upstreamBusy(row.ID)} aria-label={t(active ? 'upstreams.actions.disable' : 'upstreams.actions.enable')} />
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(row)} disabled={batchLocked || upstreamBusy(row.ID)}><Pencil /><span className="sr-only">{t('common.edit')}</span></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => { remove.reset(); setDeleting(row) }} disabled={batchLocked || upstreamBusy(row.ID)}><Trash2 /><span className="sr-only">{t('common.delete')}</span></Button>
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
                <TableHead>{t('upstreams.table.models')}</TableHead>
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
              const models = modelsForDisplay(row)
              const modelNotice = modelsValidationNotice(row, t)
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
                    <TableCell className="max-w-72">
                      <div className="text-xs text-muted-foreground">{models.length}</div>
                      <div className="max-h-12 overflow-hidden break-words font-mono text-xs" title={models.join(', ')}>{models.length ? models.join(', ') : t('upstreams.modelsNotVerified')}</div>
                      {modelNotice && <div className="text-xs text-amber-700 dark:text-amber-400">{modelNotice}</div>}
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
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.refreshModels')} onClick={() => { void refreshModelList(row.ID) }} disabled={batchLocked || upstreamBusy(row.ID)}>
                          <RefreshCw className={cn(pendingModelIDs.has(row.ID) && 'animate-spin')} />
                          <span className="sr-only">{t('upstreams.actions.refreshModels')}</span>
                        </Button>
                        <Button variant="ghost" size="icon-sm" title={t('upstreams.actions.probe')} onClick={() => runProbe(row.ID, false)} disabled={batchLocked || upstreamBusy(row.ID)}>
                          <Activity className={cn(pendingProbeIDs.has(row.ID) && 'animate-pulse')} />
                          <span className="sr-only">{t('upstreams.actions.probe')}</span>
                        </Button>
                        <span title={balanceBlockedReason ?? t('upstreams.actions.balance')}>
                          <Button variant="ghost" size="icon-sm" title={balanceBlockedReason ?? t('upstreams.actions.balance')} onClick={() => runProbe(row.ID, true)} disabled={batchLocked || upstreamBusy(row.ID) || balanceBlockedReason != null}>
                            <WalletCards className={cn(pendingProbeIDs.has(row.ID) && 'animate-pulse')} />
                            <span className="sr-only">{t('upstreams.actions.balance')}</span>
                          </Button>
                        </span>
                        <Switch checked={active} onCheckedChange={checked => { if (!upstreamBusy(row.ID)) toggle.mutate({ id: row.ID, enabled: checked }) }} disabled={batchLocked || upstreamBusy(row.ID)} aria-label={t(active ? 'upstreams.actions.disable' : 'upstreams.actions.enable')} />
                        <Button variant="ghost" size="icon-sm" title={t('common.edit')} onClick={() => openEdit(row)} disabled={batchLocked || upstreamBusy(row.ID)}><Pencil /><span className="sr-only">{t('common.edit')}</span></Button>
                        <Button variant="ghost" size="icon-sm" className="text-destructive" title={t('common.delete')} onClick={() => { remove.reset(); setDeleting(row) }} disabled={batchLocked || upstreamBusy(row.ID)}><Trash2 /><span className="sr-only">{t('common.delete')}</span></Button>
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
              <div className="space-y-1.5"><Label htmlFor="up-mult">{t('upstreams.form.multiplier')}</Label><Input id="up-mult" type="number" min={0} max={100} step="0.0001" value={form.multiplier} onChange={e => setForm(f => ({ ...f, multiplier: e.target.value }))} /><p className="text-xs text-muted-foreground">{t('upstreams.form.multiplierHint')}</p></div>
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
            {save.isPending && <ModelValidationProgress />}
            {validation && <p className="text-sm text-destructive">{validation}</p>}
            {save.isError && <p className="text-sm text-destructive">{isRevisionConflict(save.error) ? t('upstreams.staleUpdate') : isDuplicateNameConflict(save.error) ? t('upstreams.duplicateName') : errorMessage(save.error)}</p>}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setDialogOpen(false)} disabled={save.isPending}>{t('common.cancel')}</Button>
            <Button onClick={submit} disabled={save.isPending || editingBusy}>{save.isPending ? t('common.saving') : editing ? t('common.saveChanges') : t('common.create')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!deleting} onOpenChange={o => { if (!o && !remove.isPending) { remove.reset(); setDeleting(null) } }}>
        <DialogContent className="sm:max-w-sm">
          <DialogHeader><DialogTitle>{t('upstreams.deleteTitle')}</DialogTitle><DialogDescription>{t('upstreams.deleteDesc', { name: deleting?.Name })}</DialogDescription></DialogHeader>
          {remove.isError && errorMessage(remove.error) && <p className="text-sm text-destructive">{errorMessage(remove.error)}</p>}
          <DialogFooter>
            <Button variant="outline" onClick={() => { remove.reset(); setDeleting(null) }} disabled={remove.isPending}>{t('common.cancel')}</Button>
            <Button variant="destructive" onClick={() => { if (deleting && !upstreamBusy(deleting.ID)) remove.mutate(deleting.ID) }} disabled={remove.isPending || (deleting != null && upstreamBusy(deleting.ID))}>{remove.isPending ? t('common.deleting') : t('common.confirmDelete')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={batchValidationOpen} onOpenChange={o => { if (!o && batchValidation.isPending) return; setBatchValidationOpen(o) }}>
        <DialogContent className="top-4 max-h-[calc(100dvh-2rem)] translate-y-0 overflow-y-auto sm:top-1/2 sm:max-w-2xl sm:-translate-y-1/2">
          <DialogHeader>
            <DialogTitle>{t('upstreams.batchValidationTitle')}</DialogTitle>
            <DialogDescription>{t('upstreams.batchValidationDesc')}</DialogDescription>
          </DialogHeader>
          {batchValidation.isPending && (
            <div className="space-y-3">
              <ModelValidationProgress checked={batchValidationProgress?.upstreams_checked} total={batchValidationProgress?.upstreams_total} label={t('upstreams.batchProgressLabel')} />
              {batchValidationProgress && (
                <p className="text-xs tabular-nums text-muted-foreground">
                  {t('upstreams.batchModelProgress', { checked: batchValidationProgress.models_checked, total: batchValidationProgress.models_total, available: batchValidationProgress.models_available, failed: batchValidationProgress.models_failed })}
                </p>
              )}
              <p className="text-xs text-muted-foreground">{t('upstreams.batchValidationRunning')}</p>
            </div>
          )}
          {batchValidation.isError && !batchValidation.isPending && (
            <p className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
              {t('upstreams.batchValidationFailed', { message: batchValidationErrorMessage(batchValidation.error, t) ?? t('upstreams.probeErrors.unknown') })}
            </p>
          )}
          {batchValidationResult && !batchValidation.isPending && (
            <div className="space-y-3">
              <div className="grid grid-cols-2 gap-2 text-sm sm:grid-cols-4">
                <div className="rounded-md border px-3 py-2"><div className="text-xs text-muted-foreground">{t('upstreams.batchTotal')}</div><div className="font-semibold tabular-nums">{batchValidationResult.total}</div></div>
                <div className="rounded-md border px-3 py-2"><div className="text-xs text-muted-foreground">{t('upstreams.batchCompleted')}</div><div className="font-semibold tabular-nums">{batchValidationResult.completed}</div></div>
                <div className="rounded-md border border-emerald-500/30 px-3 py-2"><div className="text-xs text-muted-foreground">{t('upstreams.batchSucceeded')}</div><div className="font-semibold tabular-nums text-emerald-700 dark:text-emerald-400">{batchValidationResult.passed}</div></div>
                <div className="rounded-md border border-destructive/30 px-3 py-2"><div className="text-xs text-muted-foreground">{t('upstreams.batchFailed')}</div><div className="font-semibold tabular-nums text-destructive">{batchValidationResult.failed}</div></div>
              </div>
              {typeof batchValidationResult.duration_ms === 'number' && Number.isFinite(batchValidationResult.duration_ms) && (
                <p className="text-xs text-muted-foreground">{t('upstreams.batchDuration', { value: (Math.max(0, batchValidationResult.duration_ms) / 1000).toFixed(1) })}</p>
              )}
              <div className="max-h-[45vh] space-y-2 overflow-y-auto pr-1" role="list" aria-label={t('upstreams.batchResults')}>
                {batchValidationResult.items.map((item, index) => {
                  const models = batchItemModels(item)
                  const totalModels = batchItemCount(item, 'models_total')
                  const availableModels = batchItemCount(item, 'models_available')
                  const failedModels = batchItemCount(item, 'models_failed')
                  const errorCode = item.error_code ? probeErrorLabel(item.error_code, t) : null
                  const hasAvailableModels = models.length > 0 || (availableModels ?? 0) > 0
                  // A complete catalogue may still contain model-specific
                  // failures. It is usable, but not fully verified; label it
                  // partial so operators do not mistake one failed model for
                  // an outage or discard the models that did pass.
                  const hasFailedModels = (failedModels ?? 0) > 0 || (errorCode != null && hasAvailableModels)
                  const verified = item.attempted && item.validation_complete && item.ok && !hasFailedModels
                  const partial = item.attempted && hasAvailableModels && !verified
                  return (
                    <div key={`${item.upstream.ID}-${index}`} role="listitem" className="rounded-md border px-3 py-2.5 text-sm">
                      <div className="flex items-start justify-between gap-3">
                        <div className="min-w-0">
                          <div className="truncate font-medium">{batchItemName(item)}</div>
                          {item.upstream.BaseURL && <div className="truncate font-mono text-xs text-muted-foreground" title={item.upstream.BaseURL}>{item.upstream.BaseURL}</div>}
                        </div>
                        <Badge variant={verified ? 'secondary' : partial ? 'outline' : item.attempted ? 'destructive' : 'outline'} className="shrink-0 gap-1">
                          {verified ? <CircleCheck className="size-3" /> : partial ? <CircleAlert className="size-3" /> : <CircleX className="size-3" />}
                          {t(!item.attempted ? 'upstreams.batchNotStarted' : verified ? 'upstreams.batchOk' : partial ? 'upstreams.batchPartial' : 'upstreams.batchFailedItem')}
                        </Badge>
                      </div>
                      {(totalModels != null || availableModels != null || failedModels != null) && (
                        <div className="mt-1 text-xs text-muted-foreground tabular-nums">
                          {t('upstreams.batchModelsSummary', { total: totalModels ?? '—', available: availableModels ?? models.length, failed: failedModels ?? '—' })}
                        </div>
                      )}
                      {!item.validation_complete && <div className="mt-1 text-xs text-amber-700 dark:text-amber-400">{t('upstreams.batchIncomplete')}</div>}
                      {models.length > 0 && <div className="mt-1 break-words font-mono text-xs text-muted-foreground">{models.join(', ')}</div>}
                      {errorCode && <div className="mt-1 text-xs text-destructive">{t('upstreams.batchError', { code: errorCode })}</div>}
                    </div>
                  )
                })}
              </div>
            </div>
          )}
          <DialogFooter>
            <Button variant="outline" onClick={() => setBatchValidationOpen(false)} disabled={batchValidation.isPending}>{t('common.done')}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </motion.div>
  )
}

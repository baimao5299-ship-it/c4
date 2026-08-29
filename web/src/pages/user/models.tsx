import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Activity, ArrowRight, CircleAlert, CircleCheck, Clock3, RefreshCw, Sparkles } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { sortModelsLatestFirst } from '@/lib/model-sort'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { cn } from '@/lib/utils'

type ModelMetric = {
  model: string
  requests: number
  errors: number
  latencyTotal: number
  lastAt: string | null
  groups: string[]
}

type MetricRow = {
  Model?: unknown
  RequestID?: unknown
  ErrorType?: unknown
  LatencyMS?: unknown
  CreatedAt?: unknown
}

const windowRange = (anchor = new Date()) => {
  const to = anchor
  const from = new Date(to.getTime() - 24 * 60 * 60 * 1000)
  return { from: from.toISOString(), to: to.toISOString() }
}

type CursorPage<Row> = { rows?: Row[]; next_cursor?: number | null }

// The API uses an ID cursor rather than offset pagination. Keep the plaza
// complete for busy accounts while bounding refresh work to a predictable
// amount (25 pages x 200 rows per table).
async function fetchCursorPages<Row>(fetchPage: (cursor?: number) => Promise<CursorPage<Row>>, maxPages = 25) {
  const rows: Row[] = []
  let cursor: number | undefined
  for (let page = 0; page < maxPages; page += 1) {
    const result = await fetchPage(cursor)
    rows.push(...(result.rows ?? []))
    const next = result.next_cursor ?? undefined
    if (!next || next === cursor) return { rows, truncated: false }
    cursor = next
  }
  return { rows, truncated: true }
}

function buildMetrics(groups: Awaited<ReturnType<typeof userApi.listUserGroups>>, usageRows: MetricRow[], errorRows: MetricRow[]): ModelMetric[] {
  const byModel = new Map<string, { requests: number; errors: number; latencyTotal: number; lastAt: string | null; groups: Set<string> }>()
  const seenErrorKeys = new Set<string>()
  const ensure = (raw: unknown) => {
    const model = String(raw ?? '').trim()
    if (!model) return null
    if (!byModel.has(model)) byModel.set(model, { requests: 0, errors: 0, latencyTotal: 0, lastAt: null, groups: new Set() })
    return byModel.get(model)!
  }
  for (const group of groups ?? []) {
    const groupName = group.Name || `#${group.ID ?? ''}`
    for (const model of group.AllowedModels ?? []) {
      const metric = ensure(model)
      if (metric && groupName) metric.groups.add(String(groupName))
    }
  }
  for (const row of usageRows ?? []) {
    const metric = ensure(row.Model)
    if (!metric) continue
    metric.requests += 1
    metric.latencyTotal += Math.max(0, Number(row.LatencyMS ?? 0) || 0)
    const at = row.CreatedAt ? String(row.CreatedAt) : null
    if (at && (!metric.lastAt || at > metric.lastAt)) metric.lastAt = at
    if (row.ErrorType && row.ErrorType !== 'none') {
      const requestID = String(row.RequestID ?? '').trim()
      const errorKey = requestID ? `${requestID}\u0000${String(row.Model ?? '').trim()}` : ''
      // An abort is intentionally present in both usage_logs and err_logs.
      // Count each request once, including duplicate rows from a retried read.
      if (!errorKey || !seenErrorKeys.has(errorKey)) {
        metric.errors += 1
        if (errorKey) seenErrorKeys.add(errorKey)
      }
    }
  }
  for (const row of errorRows ?? []) {
    const metric = ensure(row.Model)
    if (!metric) continue
    if (row.ErrorType === 'none') continue
    // abort/failover rows are intentionally mirrored into both tables. Count
    // one request once so the plaza never inflates an error rate.
    const requestID = String(row.RequestID ?? '').trim()
    const errorKey = requestID ? `${requestID}\u0000${String(row.Model ?? '').trim()}` : ''
    if (errorKey && seenErrorKeys.has(errorKey)) continue
    metric.errors += 1
    if (errorKey) seenErrorKeys.add(errorKey)
    const at = row.CreatedAt ? String(row.CreatedAt) : null
    if (at && (!metric.lastAt || at > metric.lastAt)) metric.lastAt = at
  }
  return sortModelsLatestFirst(Array.from(byModel.keys())).map(model => {
    const value = byModel.get(model)!
    return { model, ...value, groups: Array.from(value.groups).slice(0, 3) }
  })
}

function statusFor(metric: ModelMetric, t: (key: string) => string) {
  if (metric.requests === 0 && metric.errors === 0) return { label: t('user.models.status.pending'), className: 'text-muted-foreground', icon: Clock3 }
  const errorRate = metric.errors / Math.max(1, metric.requests + metric.errors)
  if (errorRate >= 0.2) return { label: t('user.models.status.attention'), className: 'text-amber-600 dark:text-amber-400', icon: CircleAlert }
  return { label: t('user.models.status.stable'), className: 'text-emerald-600 dark:text-emerald-400', icon: CircleCheck }
}

function ModelCard({ metric, t }: { metric: ModelMetric; t: (key: string, options?: Record<string, unknown>) => string }) {
  const status = statusFor(metric, t)
  const Icon = status.icon
  const totalSamples = metric.requests + metric.errors
  const errorRate = totalSamples ? (metric.errors / totalSamples) * 100 : 0
  const averageLatency = metric.requests && metric.latencyTotal ? Math.round(metric.latencyTotal / metric.requests) : 0
  return (
    <Card className="h-full transition-transform duration-200 hover:-translate-y-0.5 hover:shadow-lg">
      <CardHeader className="gap-3">
        <div className="flex items-start justify-between gap-3">
          <CardTitle className="min-w-0 truncate font-mono text-base" title={metric.model}>{metric.model}</CardTitle>
          <Badge variant="secondary" className={cn('shrink-0 gap-1', status.className)}><Icon className="size-3.5" />{status.label}</Badge>
        </div>
        <CardDescription className="flex items-center gap-1.5">
          <Activity className="size-4" />{t('user.models.sampleWindow')}
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-3 gap-2 text-center">
          <div className="rounded-lg bg-primary/6 px-2 py-2.5"><div className="text-lg font-semibold tabular-nums">{metric.requests.toLocaleString()}</div><div className="text-[11px] text-muted-foreground">{t('user.models.calls')}</div></div>
          <div className="rounded-lg bg-primary/6 px-2 py-2.5"><div className="text-lg font-semibold tabular-nums">{averageLatency ? `${averageLatency}ms` : '—'}</div><div className="text-[11px] text-muted-foreground">{t('user.models.latency')}</div></div>
          <div className="rounded-lg bg-primary/6 px-2 py-2.5"><div className="text-lg font-semibold tabular-nums">{totalSamples ? `${errorRate.toFixed(1)}%` : '—'}</div><div className="text-[11px] text-muted-foreground">{t('user.models.errorRate')}</div></div>
        </div>
        <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground">
          <span className="truncate">{metric.groups.length ? `${t('user.models.group')}: ${metric.groups.join(', ')}` : t('user.models.groupPending')}</span>
          <span className="shrink-0">{metric.lastAt ? new Date(metric.lastAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '—'}</span>
        </div>
      </CardContent>
    </Card>
  )
}

export default function UserModels({ compact = false }: { compact?: boolean }) {
  const { t } = useTranslation()
  const [rangeAnchor, setRangeAnchor] = useState(() => Date.now())
  useEffect(() => {
    const timer = window.setInterval(() => setRangeAnchor(Date.now()), 60_000)
    return () => window.clearInterval(timer)
  }, [])
  const range = useMemo(() => windowRange(new Date(rangeAnchor)), [rangeAnchor])
  const groupsQ = useQuery({ queryKey: ['user', 'groups'], queryFn: () => userApi.listUserGroups(), staleTime: 60_000 })
  const usageQ = useQuery({
    queryKey: ['user', 'model-usage', range],
    queryFn: () => fetchCursorPages(cursor => userApi.getMyUsageLogs({ ...range, limit: 200, cursor })),
    refetchInterval: 30_000,
  })
  const errorsQ = useQuery({
    queryKey: ['user', 'model-errors', range],
    queryFn: () => fetchCursorPages(cursor => userApi.getMyErrLogs({ ...range, limit: 200, cursor })),
    refetchInterval: 30_000,
  })
  const metrics = useMemo(() => buildMetrics(groupsQ.data ?? [], usageQ.data?.rows ?? [], errorsQ.data?.rows ?? []), [groupsQ.data, usageQ.data, errorsQ.data])
  const visible = compact ? metrics.slice(0, 6) : metrics
  const loading = groupsQ.isLoading || usageQ.isLoading || errorsQ.isLoading
  const failed = groupsQ.isError || usageQ.isError || errorsQ.isError
  const truncated = Boolean(usageQ.data?.truncated || errorsQ.data?.truncated)
  const stableCount = metrics.filter(metric => statusFor(metric, t).label === t('user.models.status.stable')).length
  const avgLatency = metrics.filter(metric => metric.requests && metric.latencyTotal).reduce((sum, metric) => sum + metric.latencyTotal / metric.requests, 0) / Math.max(1, metrics.filter(metric => metric.requests && metric.latencyTotal).length)

  return (
    <section id={compact ? undefined : 'model-plaza'} className="space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <div className="flex items-center gap-2"><Sparkles className="size-5 text-primary" /><h2 className={compact ? 'text-lg font-semibold' : 'text-2xl font-semibold tracking-tight'}>{t('user.models.title')}</h2></div>
          <p className="mt-1 text-sm text-muted-foreground">{t('user.models.subtitle')}</p>
        </div>
        <div className="flex items-center gap-2">
          {!compact && <Button variant="outline" size="sm" onClick={() => { void groupsQ.refetch(); void usageQ.refetch(); void errorsQ.refetch() }} disabled={loading}><RefreshCw className={cn('size-4', loading && 'animate-spin')} />{t('user.models.refresh')}</Button>}
          {compact && <Button variant="ghost" size="sm" render={<Link to="/user/models" />}>{t('user.models.viewAll')}<ArrowRight className="size-4" /></Button>}
        </div>
      </div>
      {!compact && !loading && !failed && (
        <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
          <div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums">{metrics.length}</div><div className="text-xs text-muted-foreground">{t('user.models.total')}</div></div>
          <div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">{stableCount}</div><div className="text-xs text-muted-foreground">{t('user.models.stableCount')}</div></div>
          <div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums">{avgLatency ? `${Math.round(avgLatency)}ms` : '—'}</div><div className="text-xs text-muted-foreground">{t('user.models.avgLatency')}</div></div>
          <div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums">24h</div><div className="text-xs text-muted-foreground">{t('user.models.window')}</div></div>
        </div>
      )}
      {failed ? <p className="rounded-xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">{t('user.models.loadFailed')}</p> : loading ? (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">{Array.from({ length: compact ? 3 : 6 }).map((_, i) => <Skeleton key={i} className="h-44 rounded-[14px]" />)}</div>
      ) : visible.length === 0 ? (
        <Card><CardContent className="flex flex-col items-center gap-2 py-12 text-center text-muted-foreground"><Activity className="size-10" /><p className="font-medium">{t('user.models.emptyTitle')}</p><p className="max-w-md text-sm">{t('user.models.emptyDesc')}</p></CardContent></Card>
      ) : <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">{visible.map(metric => <ModelCard key={metric.model} metric={metric} t={t} />)}</div>}
      {!compact && truncated && <p className="rounded-lg border border-amber-300/60 bg-amber-50/70 p-3 text-xs text-amber-800 dark:border-amber-400/30 dark:bg-amber-950/20 dark:text-amber-200">{t('user.models.dataCapped')}</p>}
      {!compact && <p className="text-xs text-muted-foreground">{t('user.models.disclaimer')}</p>}
    </section>
  )
}

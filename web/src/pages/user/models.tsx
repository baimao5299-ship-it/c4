import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Activity, ArrowRight, Check, CheckCircle2, CircleAlert, Clock3, Copy, RefreshCw, Sparkles } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { sortModelsLatestFirst } from '@/lib/model-sort'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { copyText } from '@/components/key-box'
import { formatPricePerCall, formatPricePerImage, formatPricePerMillion } from '@/components/fmt'
import { cn } from '@/lib/utils'
import type { components } from '@/lib/api/schema'
import { formatMultiplierValue } from '@/lib/multiplier'

type ChannelMetric = components['schemas']['UserChannelMetric']
type ChannelModelPrice = components['schemas']['UserChannelModelPrice'] & {
  // Newer servers expose the catalogue price alongside the effective group
  // price. Keep these optional so an older frontend can still read old payloads.
  OfficialInputPerM?: number | null
  OfficialOutputPerM?: number | null
  OfficialCacheReadPerM?: number | null
  OfficialCacheWritePerM?: number | null
  OfficialPricePerCall?: number | null
  OfficialPricePerImage?: number | null
}

const rollingRange = (anchor: number) => {
  const to = new Date(anchor)
  return { from: new Date(to.getTime() - 24 * 60 * 60 * 1000).toISOString(), to: to.toISOString() }
}

function statusFor(metric: ChannelMetric, t: (key: string) => string) {
  if (metric.Status === 'no_data') return { label: t('user.models.status.noData'), className: 'text-muted-foreground', icon: Clock3 }
  if (metric.Status === 'degraded') return { label: t('user.models.status.attention'), className: 'text-amber-600 dark:text-amber-400', icon: CircleAlert }
  return { label: t('user.models.status.stable'), className: 'text-emerald-600 dark:text-emerald-400', icon: CheckCircle2 }
}

function formatUpdated(value: string | null | undefined, empty: string) {
  if (!value) return empty
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return empty
  return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

function modelRows(metric: ChannelMetric): ChannelModelPrice[] {
  const models = sortModelsLatestFirst(metric.AllowedModels ?? [])
  const byName = new Map((metric.ModelPrices ?? []).map(row => [row.Model, row as ChannelModelPrice]))
  // Older C4 servers may not send ModelPrices yet. Keep those models visible
  // with explicit empty prices during a rolling frontend/backend upgrade.
  return models.map(model => byName.get(model) ?? { Model: model })
}

function priceMode(price: ChannelModelPrice): 'token' | 'call' | 'image' {
  if (price.Mode === 'call' || price.PricePerCall != null) return 'call'
  if (price.Mode === 'image' || price.PricePerImage != null) return 'image'
  return 'token'
}

type PriceFormatter = (value: number | null | undefined) => string

function basePrice(value: number | null | undefined, multiplier: number): number | null {
  if (value == null || !Number.isFinite(value) || !Number.isFinite(multiplier) || multiplier <= 0) return null
  const base = value / multiplier
  return Number.isFinite(base) ? base : null
}

function PricePair({
  label,
  value,
  officialValue,
  multiplier,
  format,
  t,
}: {
  label: string
  value?: number | null
  officialValue?: number | null
  multiplier: number
  format: PriceFormatter
  t: (key: string, options?: Record<string, unknown>) => string
}) {
  // Explicit catalogue prices are authoritative. The derived value is only a
  // compatibility fallback for servers that predate the Official* fields.
  const official = officialValue !== undefined ? officialValue : basePrice(value, multiplier)
  const hasDiscount = official != null && value != null && value < official
  const appliedLabel = hasDiscount ? t('user.models.discountedShort') : t('user.models.groupShort')
  const showStrike = hasDiscount
  return (
    <div className="flex min-w-0 flex-col gap-0.5">
      <span className="text-[10px] text-muted-foreground">{label}</span>
      <div className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 tabular-nums">
        <span className={cn('whitespace-nowrap text-[10px] text-muted-foreground', showStrike && 'line-through decoration-muted-foreground/60')} title={t('user.models.officialPrice')}>
          {t('user.models.officialShort')} {format(official)}
        </span>
        <span className="whitespace-nowrap font-medium" title={appliedLabel}>
          {appliedLabel} {format(value)}
        </span>
      </div>
    </div>
  )
}

function ModelNameButton({
  model,
  copyState,
  onCopy,
  t,
}: {
  model: string
  copyState: 'success' | 'error' | null
  onCopy: () => void
  t: (key: string, options?: Record<string, unknown>) => string
}) {
  const copied = copyState === 'success'
  const failed = copyState === 'error'
  return (
    <button
      type="button"
      className="inline-flex min-h-11 min-w-0 max-w-full items-center gap-1 text-left font-mono hover:text-primary sm:min-h-0"
      title={t('user.models.copyModel')}
      aria-label={t('user.models.copyModel')}
      onClick={onCopy}
    >
      <span className="min-w-0 truncate">{model}</span>
      {copied ? <><Check className="size-3.5 shrink-0 text-emerald-600 dark:text-emerald-400" /><span className="shrink-0 font-sans text-[10px] text-emerald-600 dark:text-emerald-400">{t('user.models.copiedModel')}</span></> : failed ? <span className="shrink-0 font-sans text-[10px] text-destructive">{t('user.models.copyFailed')}</span> : <Copy className="size-3 shrink-0 text-muted-foreground" />}
    </button>
  )
}

function ChannelCard({ metric, t }: { metric: ChannelMetric; t: (key: string, options?: Record<string, unknown>) => string }) {
  const status = statusFor(metric, t)
  const Icon = status.icon
  const models = sortModelsLatestFirst(metric.AllowedModels ?? [])
  const prices = modelRows(metric)
  const [copyState, setCopyState] = useState<{ model: string; status: 'success' | 'error' } | null>(null)
  const copyTimer = useRef<number | undefined>(undefined)
  useEffect(() => () => {
    if (copyTimer.current != null) window.clearTimeout(copyTimer.current)
  }, [])
  const copyModel = async (model: string) => {
    const ok = await copyText(model)
    setCopyState({ model, status: ok ? 'success' : 'error' })
    if (copyTimer.current != null) window.clearTimeout(copyTimer.current)
    copyTimer.current = window.setTimeout(() => {
      setCopyState(current => current?.model === model ? null : current)
    }, 2000)
  }
  const multiplier = Number.isFinite(metric.PriceMultiplier) && metric.PriceMultiplier >= 0 ? metric.PriceMultiplier : 1
  const success = metric.RequestCount > 0 ? `${metric.SuccessRate.toFixed(1)}%` : '—'
  return (
    <Card className="h-full transition-transform duration-200 hover:-translate-y-0.5 hover:shadow-lg">
      <CardHeader className="gap-3">
        <div className="flex w-full min-w-0 items-start justify-between gap-3">
          <CardTitle className="min-w-0 truncate text-base" title={metric.Name}>{metric.Name}</CardTitle>
          <Badge variant="secondary" className={cn('shrink-0 gap-1', status.className)}><Icon className="size-3.5" />{status.label}</Badge>
        </div>
        <CardDescription className="flex w-full min-w-0 flex-wrap items-center gap-x-3 gap-y-1"><span className="flex min-w-0 items-center gap-1.5 truncate"><Activity className="size-4 shrink-0" />{t('user.models.channelWindow')}</span><span className="shrink-0 font-mono text-xs">{t('user.models.multiplier', { value: formatMultiplierValue(metric.PriceMultiplier) })}</span></CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="grid grid-cols-3 gap-2 text-center">
          <div className="rounded-lg bg-primary/6 px-2 py-2.5"><div className="text-lg font-semibold tabular-nums">{metric.RequestCount.toLocaleString()}</div><div className="text-[11px] text-muted-foreground">{t('user.models.calls')}</div></div>
          <div className="rounded-lg bg-primary/6 px-2 py-2.5"><div className="text-lg font-semibold tabular-nums">{metric.AverageLatencyMS ? `${metric.AverageLatencyMS}ms` : '—'}</div><div className="text-[11px] text-muted-foreground">{t('user.models.latency')}</div></div>
          <div className="rounded-lg bg-primary/6 px-2 py-2.5"><div className="text-lg font-semibold tabular-nums">{success}</div><div className="text-[11px] text-muted-foreground">{t('user.models.successRate')}</div></div>
        </div>
        <div className="space-y-2">
          <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground"><span>{t('user.models.modelsLabel')}</span><span className="shrink-0">{formatUpdated(metric.LastCalledAt, t('user.models.notCalled'))}</span></div>
          {prices.length ? <div className="max-h-64 divide-y overflow-y-auto rounded-lg border bg-background/60">{prices.map(price => {
            const mode = priceMode(price)
            return <div key={price.Model} className="grid grid-cols-1 items-center gap-x-2 gap-y-1 px-2.5 py-2 text-xs sm:grid-cols-[minmax(0,1fr)_auto_auto]"><ModelNameButton model={price.Model} copyState={copyState?.model === price.Model ? copyState.status : null} onCopy={() => { void copyModel(price.Model) }} t={t} />{mode === 'call' ? <PricePair label={t('user.models.callShort')} value={price.PricePerCall} officialValue={price.OfficialPricePerCall} multiplier={multiplier} format={value => formatPricePerCall(value, t('user.models.callUnit'))} t={t} /> : mode === 'image' ? <PricePair label={t('user.models.imageShort')} value={price.PricePerImage} officialValue={price.OfficialPricePerImage} multiplier={multiplier} format={value => formatPricePerImage(value, t('user.models.imageUnit'))} t={t} /> : <><PricePair label={t('user.models.inputShort')} value={price.InputPerM} officialValue={price.OfficialInputPerM} multiplier={multiplier} format={value => formatPricePerMillion(value)} t={t} /><PricePair label={t('user.models.outputShort')} value={price.OutputPerM} officialValue={price.OfficialOutputPerM} multiplier={multiplier} format={value => formatPricePerMillion(value)} t={t} /></>}</div>
          })}</div> : <p className="text-xs text-muted-foreground">{t('user.models.modelsPending')}</p>}
          {models.length > 0 && <p className="text-[11px] text-muted-foreground">{t('user.models.priceUnit')}</p>}
        </div>
      </CardContent>
    </Card>
  )
}

export default function UserModels({ compact = false }: { compact?: boolean }) {
  const { t } = useTranslation()
  const [rangeAnchor, setRangeAnchor] = useState(() => Date.now())
  useEffect(() => { const timer = window.setInterval(() => setRangeAnchor(Date.now()), 60_000); return () => window.clearInterval(timer) }, [])
  const range = useMemo(() => rollingRange(rangeAnchor), [rangeAnchor])
  const channelsQ = useQuery({ queryKey: ['user', 'channel-monitor', range], queryFn: () => userApi.getChannelMonitor(range), refetchInterval: 30_000 })
  const metrics = channelsQ.data?.rows ?? []
  const visible = compact ? metrics.slice(0, 6) : metrics
  const loading = channelsQ.isLoading
  const refreshing = channelsQ.isFetching
  const hasData = channelsQ.data != null
  const stale = channelsQ.isError && hasData
  const stableCount = metrics.filter(metric => metric.Status === 'stable').length
  const measured = metrics.filter(metric => metric.RequestCount > 0)
  const avgLatency = measured.length ? measured.reduce((sum, metric) => sum + metric.AverageLatencyMS, 0) / measured.length : 0
  return (
    <section id={compact ? undefined : 'channel-monitor'} className="space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-3"><div><div className="flex items-center gap-2"><Sparkles className="size-5 text-primary" /><h2 className={compact ? 'text-lg font-semibold' : 'text-2xl font-semibold tracking-tight'}>{t('user.models.title')}</h2></div><p className="mt-1 text-sm text-muted-foreground">{t('user.models.subtitle')}</p>{hasData && <p className="mt-1 text-xs text-muted-foreground">{stale ? t('user.models.staleData') : t('user.models.updatedAt', { time: formatUpdated(channelsQ.data?.window_to, t('user.models.notCalled')) })}</p>}</div><div className="flex items-center gap-2">{!compact && <Button variant="outline" size="sm" onClick={() => { void channelsQ.refetch() }} disabled={refreshing}><RefreshCw className={cn('size-4', refreshing && 'animate-spin')} />{t('user.models.refresh')}</Button>}{compact && <Button variant="ghost" size="sm" render={<Link to="/user/models" />}>{t('user.models.viewAll')}<ArrowRight className="size-4" /></Button>}</div></div>
      {!compact && !loading && !channelsQ.isError && <div className="grid grid-cols-2 gap-3 sm:grid-cols-4"><div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums">{metrics.length}</div><div className="text-xs text-muted-foreground">{t('user.models.totalChannels')}</div></div><div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">{stableCount}</div><div className="text-xs text-muted-foreground">{t('user.models.stableCount')}</div></div><div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums">{avgLatency ? `${Math.round(avgLatency)}ms` : '—'}</div><div className="text-xs text-muted-foreground">{t('user.models.avgLatency')}</div></div><div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3"><div className="text-xl font-semibold tabular-nums">24h</div><div className="text-xs text-muted-foreground">{t('user.models.window')}</div></div></div>}
      {channelsQ.isError && !hasData ? <p className="rounded-xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">{t('user.models.loadFailed')}</p> : loading ? <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">{Array.from({ length: compact ? 3 : 6 }).map((_, i) => <Skeleton key={i} className="h-44 rounded-[14px]" />)}</div> : visible.length === 0 ? <Card><CardContent className="flex flex-col items-center gap-2 py-12 text-center text-muted-foreground"><Activity className="size-10" /><p className="font-medium">{t('user.models.emptyTitle')}</p><p className="max-w-md text-sm">{t('user.models.emptyDesc')}</p></CardContent></Card> : <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">{visible.map(metric => <ChannelCard key={metric.GroupID} metric={metric} t={t} />)}</div>}
      {!compact && <p className="text-xs text-muted-foreground">{t('user.models.disclaimer')}</p>}
    </section>
  )
}

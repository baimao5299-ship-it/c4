import { useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Activity, ArrowRight, Check, CircleAlert, Copy, PauseCircle, RefreshCw, Sparkles } from 'lucide-react'
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

type ChannelMetric = components['schemas']['UserChannelMetric'] & {
  /** Added by the group remark API; optional for rolling upgrades. */
  Remark?: string | null
}
type ChannelModelPrice = components['schemas']['UserChannelModelPrice'] & {
  // Newer servers expose the catalogue price alongside the effective group
  // price. Keep these optional so an older frontend can still read old payloads.
  OfficialInputPerM?: number | null
  OfficialImgInTokPerM?: number | null
  OfficialImgOutTokPerM?: number | null
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
  if (metric.Status === 'paused') return { label: t('user.models.status.paused'), className: 'text-muted-foreground', icon: PauseCircle }
  if (metric.Status === 'maintenance') return { label: t('user.models.status.maintenance'), className: 'text-amber-600 dark:text-amber-400', icon: CircleAlert }
  return { label: t('user.models.status.available'), className: 'text-emerald-600 dark:text-emerald-400', icon: Check }
}

function modelRows(metric: ChannelMetric): ChannelModelPrice[] {
  const models = sortModelsLatestFirst(metric.AllowedModels ?? [])
  // The allowlist is trimmed by current write paths, but older groups and
  // rolling backend upgrades can still return surrounding whitespace in either
  // array. Use the canonical trimmed identifier only for the lookup key while
  // returning the original price row unchanged so provider spelling/display is
  // preserved.
  const byName = new Map<string, ChannelModelPrice>()
  for (const row of metric.ModelPrices ?? []) {
    const key = row.Model.trim()
    if (key === '') continue
    // Prefer an already-canonical row when duplicate legacy rows differ only
    // by whitespace; otherwise keep the first stable response entry.
    const existing = byName.get(key)
    if (!existing || row.Model === key) byName.set(key, row as ChannelModelPrice)
  }
  // Older C4 servers may not send ModelPrices yet. Keep those models visible
  // with explicit empty prices during a rolling frontend/backend upgrade.
  return models.map(model => {
    const canonical = model.trim()
    return byName.get(canonical) ?? { Model: canonical || model }
  })
}

function priceMode(price: ChannelModelPrice): 'token' | 'call' | 'image' {
  if (price.Mode === 'call') return 'call'
  if (price.Mode === 'image') return 'image'
  if (price.Mode === 'token') return 'token'
  if (price.PricePerCall != null || price.OfficialPricePerCall != null) return 'call'
  // Older API responses may omit Mode. Infer image mode from any image
  // component so token-only image models are still shown with their prices.
  if (price.PricePerImage != null || price.OfficialPricePerImage != null || price.ImgInTokPerM != null || price.ImgOutTokPerM != null || price.OfficialImgInTokPerM != null || price.OfficialImgOutTokPerM != null) return 'image'
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

function PriceDetails({
  price,
  mode,
  multiplier,
  t,
}: {
  price: ChannelModelPrice
  mode: 'token' | 'call' | 'image'
  multiplier: number
  t: (key: string, options?: Record<string, unknown>) => string
}) {
  const pairs: Array<{ key: string; label: string; value?: number | null; officialValue?: number | null; format: PriceFormatter }> = []
  const add = (key: string, label: string, value: number | null | undefined, officialValue: number | null | undefined, format: PriceFormatter) => {
    // Do not render an empty component. A zero is a real free price and is
    // intentionally retained by this check.
    if (value == null && officialValue == null) return
    pairs.push({ key, label, value, officialValue, format })
  }
  const hasImageFields = [price.ImgInTokPerM, price.ImgOutTokPerM, price.PricePerImage, price.OfficialImgInTokPerM, price.OfficialImgOutTokPerM, price.OfficialPricePerImage].some(value => value != null)
  if (mode === 'call') {
    add('call', t('user.models.callShort'), price.PricePerCall, price.OfficialPricePerCall, value => formatPricePerCall(value, t('user.models.callUnit')))
  }
  // Some older or manually edited catalogue rows have an explicit token/call
  // mode while still carrying image fields. Render every populated component
  // so a mode mismatch cannot hide a real price from the user.
  if (mode === 'image' || hasImageFields) {
    add('image-input', t('user.models.imageInputShort'), price.ImgInTokPerM, price.OfficialImgInTokPerM, value => formatPricePerMillion(value))
    add('image-output', t('user.models.imageOutputShort'), price.ImgOutTokPerM, price.OfficialImgOutTokPerM, value => formatPricePerMillion(value))
    add('image', t('user.models.imageShort'), price.PricePerImage, price.OfficialPricePerImage, value => formatPricePerImage(value, t('user.models.imageUnit')))
  }
  if (mode !== 'call' || price.InputPerM != null || price.OutputPerM != null || price.CacheReadPerM != null || price.CacheWritePerM != null || price.OfficialInputPerM != null || price.OfficialOutputPerM != null || price.OfficialCacheReadPerM != null || price.OfficialCacheWritePerM != null) {
    // Multimodal catalogue rows can also carry normal token/cache prices.
    // Keep them visible instead of assuming image mode is mutually exclusive.
    add('input', t('user.models.inputShort'), price.InputPerM, price.OfficialInputPerM, value => formatPricePerMillion(value))
    add('output', t('user.models.outputShort'), price.OutputPerM, price.OfficialOutputPerM, value => formatPricePerMillion(value))
    add('cache-read', t('user.models.cacheReadShort'), price.CacheReadPerM, price.OfficialCacheReadPerM, value => formatPricePerMillion(value))
    add('cache-write', t('user.models.cacheWriteShort'), price.CacheWritePerM, price.OfficialCacheWritePerM, value => formatPricePerMillion(value))
  }
  if (pairs.length === 0) return null
  return <div className="flex min-w-0 flex-wrap items-start gap-x-3 gap-y-1">{pairs.map(pair => <PricePair key={pair.key} label={pair.label} value={pair.value} officialValue={pair.officialValue} multiplier={multiplier} format={pair.format} t={t} />)}</div>
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
  return (
    <Card className="h-full transition-transform duration-200 hover:-translate-y-0.5 hover:shadow-lg">
      <CardHeader className="gap-3">
        <div className="flex w-full min-w-0 items-start justify-between gap-3">
          <CardTitle className="min-w-0 truncate text-base" title={metric.Name}>{metric.Name}</CardTitle>
          <Badge variant="secondary" className={cn('shrink-0 gap-1', status.className)}><Icon className="size-3.5" />{status.label}</Badge>
        </div>
        <CardDescription className="flex w-full min-w-0 flex-wrap items-center gap-x-3 gap-y-1"><span className="flex min-w-0 items-center gap-1.5 truncate"><Activity className="size-4 shrink-0" />{t('user.models.publicChannel')}</span><span className="shrink-0 font-mono text-xs">{t('user.models.multiplier', { value: formatMultiplierValue(metric.PriceMultiplier) })}</span></CardDescription>
        {metric.Remark && <p className="rounded-md border border-primary/20 bg-primary/5 px-2.5 py-1.5 text-xs text-muted-foreground" title={metric.Remark}>{metric.Remark}</p>}
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="space-y-2">
          <div className="flex items-center justify-between gap-3 text-xs text-muted-foreground"><span>{t('user.models.modelsLabel')}</span><span className="shrink-0">{t('user.models.statusManual')}</span></div>
          {prices.length ? <div className="max-h-72 space-y-2 overflow-y-auto rounded-lg border bg-background/60 p-2">{prices.map(price => {
            const mode = priceMode(price)
            // An official catalogue value is still useful when the current
            // group has no resolved billable value (for example while a
            // conditional variant is being repaired). Only mark a model as
            // completely unpriced when both views are empty.
            const hasAnyPrice = [
              price.InputPerM, price.OutputPerM, price.CacheReadPerM, price.CacheWritePerM,
              price.PricePerCall, price.PricePerImage, price.ImgInTokPerM, price.ImgOutTokPerM,
              price.OfficialInputPerM, price.OfficialOutputPerM, price.OfficialCacheReadPerM,
              price.OfficialCacheWritePerM, price.OfficialPricePerCall, price.OfficialPricePerImage,
              price.OfficialImgInTokPerM, price.OfficialImgOutTokPerM,
            ].some(value => value != null && Number.isFinite(value))
            return <div key={price.Model} className={cn('rounded-md border bg-card/80 px-3 py-2.5 text-xs shadow-sm', !hasAnyPrice && 'border-amber-400/60 bg-amber-50/40 dark:bg-amber-950/10')}>
              <div className="flex min-w-0 items-center justify-between gap-3 border-b border-border/60 pb-2">
                <ModelNameButton model={price.Model} copyState={copyState?.model === price.Model ? copyState.status : null} onCopy={() => { void copyModel(price.Model) }} t={t} />
                {!hasAnyPrice && <span className="shrink-0 font-medium text-amber-700 dark:text-amber-400">{t('user.models.unpriced')}</span>}
              </div>
              <div className="pt-2"><PriceDetails price={price} mode={mode} multiplier={multiplier} t={t} /></div>
            </div>
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
  return (
    <section id={compact ? undefined : 'channel-monitor'} className="space-y-5">
      <div className="flex flex-wrap items-end justify-between gap-3"><div><div className="flex items-center gap-2"><Sparkles className="size-5 text-primary" /><h2 className={compact ? 'text-lg font-semibold' : 'text-2xl font-semibold tracking-tight'}>{t('user.models.title')}</h2></div><p className="mt-1 text-sm text-muted-foreground">{t('user.models.subtitle')}</p>{hasData && stale && <p className="mt-1 text-xs text-amber-700 dark:text-amber-400">{t('user.models.staleData')}</p>}</div><div className="flex items-center gap-2">{!compact && <Button variant="outline" size="sm" onClick={() => { void channelsQ.refetch() }} disabled={refreshing}><RefreshCw className={cn('size-4', refreshing && 'animate-spin')} />{t('user.models.refresh')}</Button>}{compact && <Button variant="ghost" size="sm" render={<Link to="/user/models" />}>{t('user.models.viewAll')}<ArrowRight className="size-4" /></Button>}</div></div>
      {!compact && !loading && !channelsQ.isError && <div className="rounded-xl border border-border/70 bg-card/50 px-4 py-3 text-sm text-muted-foreground">{t('user.models.publicCatalogHint')}</div>}
      {channelsQ.isError && !hasData ? <p className="rounded-xl border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">{t('user.models.loadFailed')}</p> : loading ? <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">{Array.from({ length: compact ? 3 : 6 }).map((_, i) => <Skeleton key={i} className="h-44 rounded-[14px]" />)}</div> : visible.length === 0 ? <Card><CardContent className="flex flex-col items-center gap-2 py-12 text-center text-muted-foreground"><Activity className="size-10" /><p className="font-medium">{t('user.models.emptyTitle')}</p><p className="max-w-md text-sm">{t('user.models.emptyDesc')}</p></CardContent></Card> : <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3">{visible.map(metric => <ChannelCard key={metric.GroupID} metric={metric} t={t} />)}</div>}
      {!compact && <p className="text-xs text-muted-foreground">{t('user.models.disclaimer')}</p>}
    </section>
  )
}
